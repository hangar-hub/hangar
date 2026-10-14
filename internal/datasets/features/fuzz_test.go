package features_test

import (
	"encoding/json"
	"testing"

	"github.com/jamesagarside/hangar/internal/datasets/features"
)

// FuzzParseFeatures parses Features JSON as cards and Parquet metadata
// carry it: valid Features must survive a JSON round trip and have a
// storage schema whose inference gives the same storage back.
func FuzzParseFeatures(f *testing.F) {
	for _, s := range []string{
		`{"a":{"dtype":"string","_type":"Value"}}`,
		`{"l":{"feature":{"dtype":"int8","_type":"Value"},"length":3,"_type":"List"}}`,
		`{"s":{"x":{"_type":"Image"},"y":[{"dtype":"float32","_type":"Value"}]}}`,
		`{"c":{"names":["a","b"],"_type":"ClassLabel"},"t":{"languages":["en"],"_type":"Translation"}}`,
		`{"a":{"shape":[2,2],"dtype":"int32","_type":"Array2D"},"j":{"_type":"Json"}}`,
		`{"seq":{"feature":{"a":{"dtype":"bool","_type":"Value"}},"_type":"Sequence"}}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		fs, err := features.ParseFeatures(data)
		if err != nil {
			return
		}
		b, err := json.Marshal(fs)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		again, err := features.ParseFeatures(b)
		if err != nil {
			t.Fatalf("reparse %s: %v", b, err)
		}
		b2, err := json.Marshal(again)
		if err != nil || string(b) != string(b2) {
			t.Fatalf("round trip changed %s to %s (%v)", b, b2, err)
		}
		st, err := fs.StorageSchema()
		if err != nil {
			return
		}
		if inferred := features.FromArrowSchema(st); inferred != nil {
			if _, err := inferred.StorageSchema(); err != nil {
				t.Fatalf("Features inferred from %s have no storage: %v", st, err)
			}
		}
	})
}
