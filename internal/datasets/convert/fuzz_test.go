package convert_test

import (
	"context"
	"testing"

	"github.com/hangar-hub/hangar/internal/datasets"
	"github.com/hangar-hub/hangar/internal/datasets/convert"
)

// fuzzConvert converts one fuzzed file with a builder: it may fail, but
// must not panic, and what it writes must agree with its own counts.
func fuzzConvert(t *testing.T, builder, name string, data []byte, opts map[string]any) {
	files := map[string]convert.File{name: memFile(name, data)}
	cfg := datasets.Config{Name: "default", Builder: builder, BuilderOptions: opts,
		Files: map[string][]string{"train": {name}}, SplitOrder: []string{"train"}}
	ctx := context.Background()
	if fs, rows, err := convert.Head(ctx, cfg, "train", files, 5); err == nil {
		if len(rows) > 5 {
			t.Fatalf("Head returned %d rows", len(rows))
		}
		for _, r := range rows {
			if len(r) != len(fs) {
				t.Fatalf("a row has %d values for %d features", len(r), len(fs))
			}
		}
	}
	res, err := convert.Convert(ctx, cfg, files, convert.Options{TempDir: t.TempDir(), MaxSplitBytes: 1 << 20})
	if err != nil {
		return
	}
	defer res.Cleanup()
	if _, err := res.Features.StorageSchema(); err != nil {
		t.Fatalf("inferred Features have no storage: %v", err)
	}
	var rows int64
	for _, sh := range res.Splits[0].Shards {
		rows += sh.NumRows
	}
	if rows != res.Splits[0].NumRows {
		t.Fatalf("shards hold %d rows, the Split %d", rows, res.Splits[0].NumRows)
	}
}

func FuzzCSV(f *testing.F) {
	for _, s := range []string{
		"a,b\n1,x\n2,y\n", "x\ty\n\"q\tq\",1\n", "a,a,\n\"multi\nline\",,3\n", "n\n1\n\n2.5\nnan\n", "a;b\n1;2\n",
		"\ufeffid,when\n1,2020-01-01\n2,\n", "a,b\n1\n1,2,3\n", "\"unterminated\n",
	} {
		f.Add([]byte(s), false)
		f.Add([]byte(s), true)
	}
	f.Fuzz(func(t *testing.T, data []byte, semicolon bool) {
		opts := map[string]any{}
		if semicolon {
			opts["sep"] = ";"
		}
		fuzzConvert(t, "csv", "a.csv", data, opts)
	})
}

func FuzzJSONLines(f *testing.F) {
	for _, s := range []string{
		`{"a": 1, "b": "x"}` + "\n" + `{"a": 2.5, "c": [1, {"d": null}]}` + "\n",
		`{"t": "2020-01-01T10:00:00"}` + "\n" + `{"t": "x"}` + "\n",
		`[{"q": 1}, {"q": "s"}]`, `["one", "two"]`, `{"s": {"k": 1}}` + "\n" + `{"s": null}` + "\n",
		`{"big": 12345678901234567890}` + "\n", "{\"a\": 1}\n{broken\n", `{"m": [1e20, -0.0, 1.5e-7]}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		fuzzConvert(t, "json", "a.jsonl", data, nil)
	})
}
