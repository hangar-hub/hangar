package convert_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/datasets/convert"
	"github.com/jamesagarside/hangar/internal/datasets/features"
	"github.com/jamesagarside/hangar/internal/datasets/pq"
)

// expected is written by compat/viewer/oracle/convert_cases.py from the
// `datasets` library.
type expected struct {
	Builder   string              `json:"builder"`
	Options   map[string]any      `json:"options"`
	DataFiles map[string][]string `json:"data_files"`
	Features  json.RawMessage     `json:"features"`
	// CardFeatures are the Features passed to `datasets`, as a card's
	// `features` declare them.
	CardFeatures json.RawMessage `json:"card_features"`
	// Error is why `datasets` failed to generate the dataset, for cases
	// where it does.
	Error  string `json:"error"`
	Splits map[string]struct {
		NumExamples    int64 `json:"num_examples"`
		NumBytes       int64 `json:"num_bytes"`
		EmbeddedNBytes int64 `json:"embedded_nbytes"`
		// HubNumBytes is set for copied Parquet files.
		HubNumBytes *int64 `json:"hub_num_bytes"`
		Rows        []any  `json:"rows"`
	} `json:"splits"`
}

func localFile(t testing.TB, root, rel string) convert.File {
	p := filepath.Join(root, rel)
	st, err := os.Stat(p)
	require.NoError(t, err)
	raw, err := os.ReadFile(p)
	require.NoError(t, err)
	sum := sha256.Sum256(raw)
	return convert.File{Path: rel, Size: st.Size(), OID: hex.EncodeToString(sum[:]),
		Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil },
		OpenAt: func(context.Context) (io.ReaderAt, error) {
			return bytes.NewReader(raw), nil
		}}
}

func pyValue(v any, a *features.Arrow, f *features.Feature) any {
	if v == nil {
		return nil
	}
	if f != nil && f.IsMedia() {
		s := v.([]any)
		size := any(nil)
		if s[0] != nil {
			size = float64(len(s[0].([]byte)))
		}
		return map[string]any{"media": s[1], "size": size}
	}
	switch a.ID {
	case features.Binary, features.LargeBinary:
		return map[string]any{"b64": base64.StdEncoding.EncodeToString(v.([]byte))}
	case features.Timestamp:
		t := v.(time.Time)
		s := t.Format("2006-01-02T15:04:05")
		if us := t.Nanosecond() / 1000; us != 0 {
			s += fmt.Sprintf(".%06d", us)
		}
		if a.TZ != "" {
			s += "+00:00"
		}
		return map[string]any{"ts": s}
	case features.Date32, features.Date64:
		return map[string]any{"date": v.(time.Time).Format("2006-01-02")}
	case features.Decimal128:
		return map[string]any{"dec": v.(string)}
	case features.Struct:
		vals := v.([]any)
		out := map[string]any{}
		for i, fl := range a.Fields {
			var sub *features.Feature
			if f != nil && f.Kind == features.KindStruct {
				sub = f.Fields[i].Feature
			}
			out[fl.Name] = pyValue(vals[i], fl.Type, sub)
		}
		return out
	case features.List, features.LargeList, features.FixedSizeList:
		vals := v.([]any)
		out := make([]any, len(vals))
		var sub *features.Feature
		if f != nil && f.Elem != nil {
			sub = f.Elem
		}
		for i, x := range vals {
			out[i] = pyValue(x, a.Elem, sub)
		}
		return out
	}
	switch x := v.(type) {
	case int64:
		return float64(x)
	case uint64:
		return float64(x)
	case float64:
		if math.IsNaN(x) {
			return map[string]any{"float": "nan"}
		}
		return x
	}
	return v
}

func TestConvertMatchesDatasets(t *testing.T) {
	dirs, err := filepath.Glob("testdata/cases/*/expected.json")
	require.NoError(t, err)
	require.NotEmpty(t, dirs)
	for _, exp := range dirs {
		root := filepath.Dir(exp)
		name := filepath.Base(root)
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(exp)
			require.NoError(t, err)
			var want expected
			require.NoError(t, json.Unmarshal(raw, &want))
			filesRoot := filepath.Join(root, "files")
			files := map[string]convert.File{}
			require.NoError(t, filepath.Walk(filesRoot, func(p string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return err
				}
				rel, _ := filepath.Rel(filesRoot, p)
				files[filepath.ToSlash(rel)] = localFile(t, filesRoot, filepath.ToSlash(rel))
				return nil
			}))
			cfg := datasets.Config{Name: "default", Builder: want.Builder, BuilderOptions: map[string]any{}, Files: want.DataFiles}
			if want.CardFeatures != nil {
				cfg.Features, err = features.ParseFeatures(want.CardFeatures)
				require.NoError(t, err)
			}
			for k, v := range want.Options {
				if f, ok := v.(float64); ok && f == math.Trunc(f) {
					v = int64(f)
				}
				cfg.BuilderOptions[k] = v
			}
			for _, s := range []string{"train", "validation", "test"} {
				if _, ok := want.DataFiles[s]; ok {
					cfg.SplitOrder = append(cfg.SplitOrder, s)
				}
			}
			res, err := convert.Convert(context.Background(), cfg, files, convert.Options{
				TempDir: t.TempDir(), DatasetName: "x", RepoURL: "hf://datasets/ns/x@" + "0123456789012345678901234567890123456789",
				Open: func(_ context.Context, p string) (convert.File, error) {
					f, ok := files[p]
					if !ok {
						return convert.File{}, fmt.Errorf("no file %s", p)
					}
					return f, nil
				}})
			if want.Error != "" {
				require.Error(t, err)
				require.Contains(t, strings.ToLower(err.Error()), strings.ToLower(want.Error))
				return
			}
			require.NoError(t, err)
			defer res.Cleanup()
			gotFeatures, err := json.Marshal(res.Features)
			require.NoError(t, err)
			require.JSONEq(t, string(want.Features), string(gotFeatures))
			var names []string
			for n := range want.Splits {
				names = append(names, n)
			}
			sort.Strings(names)
			require.Len(t, res.Splits, len(names))
			st, err := res.Features.StorageSchema()
			require.NoError(t, err)
			for _, sr := range res.Splits {
				ws, ok := want.Splits[sr.Name]
				require.True(t, ok, sr.Name)
				require.Equal(t, ws.NumExamples, sr.NumRows, "num_examples of %s", sr.Name)
				wantBytes := ws.NumBytes
				if ws.EmbeddedNBytes != ws.NumBytes && hasMedia(res.Features) {
					// The Hub embeds media before it counts bytes.
					wantBytes = ws.EmbeddedNBytes
				}
				if ws.HubNumBytes != nil {
					wantBytes = *ws.HubNumBytes
				}
				var rows [][]any
				for _, sh := range sr.Shards {
					var b []byte
					if sh.LocalPath != "" {
						b, err = os.ReadFile(sh.LocalPath)
					} else {
						b, err = os.ReadFile(filepath.Join(filesRoot, sh.SourcePath))
					}
					require.NoError(t, err)
					pf, err := pq.Open(bytes.NewReader(b), int64(len(b)))
					require.NoError(t, err)
					cols, _ := pf.Columns(nil)
					rs, err := pf.ReadRows(cols, 0, -1)
					require.NoError(t, err)
					rows = append(rows, rs...)
				}
				require.Len(t, rows, len(ws.Rows))
				// __url__ is where the oracle read the shard; count the
				// bytes of Hangar's value instead.
				for i := range rows {
					if u, ok := ws.Rows[i].(map[string]any)["__url__"].(string); ok {
						wantBytes += int64(len("hf://datasets/ns/x@0123456789012345678901234567890123456789/shard-0000.tar") - len(u))
					}
				}
				require.Equal(t, wantBytes, sr.NumBytes, "num_bytes of %s", sr.Name)
				for i, r := range rows {
					got := map[string]any{}
					for j, fl := range res.Features {
						got[fl.Name] = pyValue(r[j], st.Fields[j].Type, fl.Feature)
					}
					wantRow := ws.Rows[i].(map[string]any)
					// __url__ names where `datasets` read the shard, which
					// differs between machines.
					if _, ok := wantRow["__url__"]; ok {
						require.Equal(t, "hf://datasets/ns/x@0123456789012345678901234567890123456789/shard-0000.tar", got["__url__"])
						delete(wantRow, "__url__")
						delete(got, "__url__")
					}
					gb, _ := json.Marshal(got)
					wb, _ := json.Marshal(wantRow)
					require.JSONEq(t, string(wb), string(gb), "%s row %d", sr.Name, i)
				}
			}
		})
	}
}

func hasMedia(fs features.Features) bool {
	for _, f := range fs {
		if f.Feature.Contains(func(x *features.Feature) bool { return x.IsMedia() }) {
			return true
		}
	}
	return false
}
