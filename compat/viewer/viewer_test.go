//go:build compat

package viewer

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// snapshot is a file written by snapshot.py.
type snapshot struct {
	Dataset   string                     `json:"dataset"`
	Revision  string                     `json:"revision"`
	Responses map[string]json.RawMessage `json:"responses"`
}

type manifestEntry struct {
	Repo   string `json:"repo"`
	Format string `json:"format"`
}

func here() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(file)
}

// fixtures makes sure the Hub snapshots and source files are cached,
// running snapshot.py (Python standard library only) when they are not.
func fixtures(t *testing.T) []manifestEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(here(), "datasets.json"))
	require.NoError(t, err)
	var m struct {
		Datasets []manifestEntry `json:"datasets"`
	}
	require.NoError(t, json.Unmarshal(raw, &m))
	missing := false
	for _, d := range m.Datasets {
		if _, err := os.Stat(filepath.Join(here(), "..", ".fixtures", "viewer", strings.ReplaceAll(d.Repo, "/", "__"), ".revision")); err != nil {
			missing = true
		}
	}
	if missing {
		cmd := exec.Command("python3", filepath.Join(here(), "snapshot.py"))
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "download the compat/viewer fixtures: %s", out)
	}
	return m.Datasets
}

func loadSnapshot(t *testing.T, repo string) *snapshot {
	raw, err := os.ReadFile(filepath.Join(here(), "hub", strings.ReplaceAll(repo, "/", "__")+".json"))
	require.NoError(t, err)
	var s snapshot
	require.NoError(t, json.Unmarshal(raw, &s))
	return &s
}

func sourceFiles(t *testing.T, repo string) map[string][]byte {
	root := filepath.Join(here(), "..", ".fixtures", "viewer", strings.ReplaceAll(repo, "/", "__"))
	files := map[string][]byte{}
	require.NoError(t, filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == ".revision" || rel == ".gitattributes" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = b
		return nil
	}))
	return files
}

func fetch(t *testing.T, base, endpoint string, params url.Values) (int, any) {
	t.Helper()
	res, err := http.Get(base + "/" + endpoint + "?" + params.Encode())
	require.NoError(t, err)
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	var v any
	require.NoError(t, json.Unmarshal(b, &v), string(b))
	return res.StatusCode, v
}

// differ collects mismatches between the Hub's and Hangar's documents.
type differ struct {
	errs []string
	// floatTolerance is the relative tolerance of floats; 0 means exact.
	floatTolerance float64
}

func (d *differ) addf(path, format string, args ...any) {
	d.errs = append(d.errs, path+": "+fmt.Sprintf(format, args...))
}

func (d *differ) compare(path string, hub, got any) {
	switch h := hub.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			d.addf(path, "the Hub has an object, Hangar %T", got)
			return
		}
		var keys []string
		for k := range h {
			keys = append(keys, k)
		}
		for k := range g {
			if _, ok := h[k]; !ok {
				d.addf(path, "Hangar has an extra key %q", k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			gv, ok := g[k]
			if !ok {
				d.addf(path, "Hangar lacks key %q", k)
				continue
			}
			d.compare(path+"."+k, h[k], gv)
		}
	case []any:
		g, ok := got.([]any)
		if !ok {
			d.addf(path, "the Hub has a list, Hangar %T", got)
			return
		}
		if len(g) != len(h) {
			d.addf(path, "the Hub has %d items, Hangar %d", len(h), len(g))
			return
		}
		for i := range h {
			d.compare(fmt.Sprintf("%s[%d]", path, i), h[i], g[i])
		}
	case float64:
		g, ok := got.(float64)
		if !ok {
			d.addf(path, "the Hub has %v, Hangar %v", h, got)
			return
		}
		if d.floatTolerance > 0 && h != math.Trunc(h) {
			if math.Abs(h-g) > d.floatTolerance*math.Max(math.Abs(h), 1e-9) {
				d.addf(path, "the Hub has %v, Hangar %v (more than %.0f%% apart)", h, g, d.floatTolerance*100)
			}
			return
		}
		if h != g {
			d.addf(path, "the Hub has %v, Hangar %v", h, g)
		}
	default:
		if fmt.Sprint(hub) != fmt.Sprint(got) {
			d.addf(path, "the Hub has %#v, Hangar %#v", hub, got)
		}
	}
}

// dropSrc removes media URLs, which name each server's own assets.
func dropSrc(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			if k == "src" {
				continue
			}
			out[k] = dropSrc(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = dropSrc(e)
		}
		return out
	}
	return v
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// normaliseInfo applies the differences DIFFERENCES.md lists to /info;
// ds is Hangar's Repo ID for the Hub's dataset hubDS.
func normaliseInfo(hub, got map[string]any, ds, hubDS string) {
	for name, hc := range obj(hub["dataset_info"]) {
		h, g := obj(hc), obj(obj(got["dataset_info"])[name])
		if g == nil {
			continue
		}
		// Keys that depend on when and how the Hub converted the dataset.
		for _, k := range []string{"download_checksums", "download_size", "size_in_bytes"} {
			if _, ok := h[k]; !ok {
				delete(g, k)
			}
		}
		// download_checksums keys name the Repo and the Revision the Hub
		// converted.
		for _, m := range []map[string]any{h, g} {
			cs := obj(m["download_checksums"])
			if cs == nil {
				continue
			}
			norm := map[string]any{}
			for k, v := range cs {
				if i := strings.Index(k, "@"); i >= 0 {
					if j := strings.Index(k[i:], "/"); j >= 0 {
						k = k[:i] + k[i+j:]
					}
				}
				if rest, ok := strings.CutPrefix(k, "hf://datasets/"+ds+"/"); ok {
					k = "hf://datasets/" + hubDS + "/" + rest
				}
				norm[k] = v
			}
			m["download_checksums"] = norm
		}
	}
}

// TestViewerMatchesHub converts every compat/viewer dataset on each
// backend and compares the Dataset Viewer's responses with the Hub's.
func TestViewerMatchesHub(t *testing.T) {
	entries := fixtures(t)
	for _, kind := range backends(t) {
		t.Run(kind.name, func(t *testing.T) {
			b := kind.start(t)
			for _, entry := range entries {
				t.Run(entry.Repo, func(t *testing.T) { matchHub(t, b, entry) })
			}
		})
	}
}

func matchHub(t *testing.T, b backend, entry manifestEntry) {
	snap := loadSnapshot(t, entry.Repo)
	_, name, _ := strings.Cut(entry.Repo, "/")
	ds := b.Dataset(t, name, sourceFiles(t, entry.Repo))
	copied := entry.Format == "parquet"
	var report []string
	for key, raw := range snap.Responses {
		var hub any
		require.NoError(t, json.Unmarshal(raw, &hub))
		parts := strings.Fields(key)
		endpoint := parts[0]
		params := url.Values{"dataset": {ds}}
		if len(parts) > 1 {
			c, sp, _ := strings.Cut(parts[1], "/")
			params.Set("config", c)
			params.Set("split", sp)
		}
		if endpoint == "rows" {
			params.Set("offset", parts[2])
			params.Set("length", "100")
			if parts[2] != "0" {
				params.Set("length", "7")
			}
		}
		status, got := fetch(t, b.ViewerURL(), endpoint, params)
		if status != http.StatusOK {
			report = append(report, fmt.Sprintf("%s: status %d: %v", key, status, got))
			continue
		}
		d := &differ{}
		h, g := obj(hub), obj(got)
		switch endpoint {
		case "first-rows", "rows":
			h["dataset"], g["dataset"] = nil, nil
			hub, got = dropSrc(h), dropSrc(g)
		case "info":
			normaliseInfo(h, g, ds, snap.Dataset)
		case "size":
			for _, level := range []string{"dataset", "configs", "splits"} {
				for _, item := range sizeItems(obj(h["size"])[level]) {
					if item["num_bytes_original_files"] == nil {
						for _, gi := range sizeItems(obj(g["size"])[level]) {
							delete(gi, "num_bytes_original_files")
						}
						delete(item, "num_bytes_original_files")
					}
					if !copied {
						delete(item, "num_bytes_parquet_files")
					}
				}
				if !copied {
					for _, gi := range sizeItems(obj(g["size"])[level]) {
						delete(gi, "num_bytes_parquet_files")
					}
				}
			}
		case "parquet":
			for _, m := range []map[string]any{h, g} {
				for _, f := range m["parquet_files"].([]any) {
					fm := obj(f)
					u := fm["url"].(string)
					fm["url"] = u[strings.Index(u, "/resolve/"):]
					fm["dataset"] = nil
					if !copied {
						delete(fm, "size")
					}
				}
			}
		case "statistics":
			d.floatTolerance = 0.01
		}
		if m := obj(got); m != nil {
			renameDataset(m, ds, snap.Dataset)
		}
		d.compare(key, hub, got)
		for _, e := range d.errs {
			if reason, ok := known(entry.Repo, e); ok {
				t.Logf("known difference (%s): %s", reason, e)
				continue
			}
			report = append(report, e)
		}
	}
	sort.Strings(report)
	require.Empty(t, report, "differences from the Hub at %s:\n%s", snap.Revision, strings.Join(report, "\n"))
}

func sizeItems(v any) []map[string]any {
	switch x := v.(type) {
	case map[string]any:
		return []map[string]any{x}
	case []any:
		var out []map[string]any
		for _, e := range x {
			out = append(out, obj(e))
		}
		return out
	}
	return nil
}

// renameDataset replaces Hangar's test Repo ID with the Hub's in "dataset"
// fields, since the test Namespace differs.
func renameDataset(v any, from, to string) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if k == "dataset" && e == from {
				x[k] = to
				continue
			}
			renameDataset(e, from, to)
		}
	case []any:
		for _, e := range x {
			renameDataset(e, from, to)
		}
	}
}

// knownDifferences are the Hub responses that predate the Hub's current
// code, keyed by dataset and then by the start of the difference's path.
// The Hub keeps a cached response until the dataset changes, so these
// datasets still show what older Hub workers computed; Hangar follows the
// current code, which the oracles in compat/viewer/oracle check
// (DIFFERENCES.md, "Responses cached by older Hub workers").
var knownDifferences = map[string]map[string]string{
	"lhoestq/demo1": {
		// Older generate_bins closed integer histograms with an extra
		// [max, max] bin; the current code does not.
		"statistics default/test.statistics[4].column_statistics.histogram.":  "older integer bins",
		"statistics default/train.statistics[4].column_statistics.histogram.": "older integer bins",
	},
	"cornell-movie-review-data/rotten_tomatoes": {
		// The current fill_builder_info sums the row groups'
		// total_byte_size; the cached numbers came from an older method.
		"info.dataset_info.default.dataset_size:":                "older copied-Parquet num_bytes",
		"info.dataset_info.default.splits.train.num_bytes:":      "older copied-Parquet num_bytes",
		"info.dataset_info.default.splits.validation.num_bytes:": "older copied-Parquet num_bytes",
		"info.dataset_info.default.splits.test.num_bytes:":       "older copied-Parquet num_bytes",
		"size.size.configs[0].num_bytes_memory:":                 "older copied-Parquet num_bytes",
		"size.size.dataset.num_bytes_memory:":                    "older copied-Parquet num_bytes",
		"size.size.splits[0].num_bytes_memory:":                  "older copied-Parquet num_bytes",
		"size.size.splits[1].num_bytes_memory:":                  "older copied-Parquet num_bytes",
		"size.size.splits[2].num_bytes_memory:":                  "older copied-Parquet num_bytes",
	},
	"datasets-examples/doc-audio-6": {
		// The cached /info names an older Revision (c8f51c4); datasets 5.1.0
		// gives 324026 bytes for these files, as Hangar does.
		"info.dataset_info.default.dataset_size:":           "older audiofolder num_bytes",
		"info.dataset_info.default.size_in_bytes:":          "older audiofolder num_bytes",
		"info.dataset_info.default.splits.train.num_bytes:": "older audiofolder num_bytes",
		"size.size.configs[0].num_bytes_memory:":            "older audiofolder num_bytes",
		"size.size.dataset.num_bytes_memory:":               "older audiofolder num_bytes",
		"size.size.splits[0].num_bytes_memory:":             "older audiofolder num_bytes",
	},
}

func known(repo, diff string) (string, bool) {
	for prefix, reason := range knownDifferences[repo] {
		if strings.HasPrefix(diff, prefix) {
			return reason, true
		}
	}
	return "", false
}
