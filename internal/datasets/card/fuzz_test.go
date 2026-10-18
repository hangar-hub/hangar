package card_test

import (
	"encoding/json"
	"testing"

	"github.com/hangar-hub/hangar/internal/datasets/card"
)

// FuzzParse parses dataset cards: any card either fails with ErrInvalid
// or gives a default Config that exists and metadata that encodes as JSON.
func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"---\nconfigs:\n- config_name: a\n  data_files:\n  - split: train\n    path: x/*.csv\n---\n# T\n",
		"---\ndataset_info:\n  features:\n  - name: l\n    sequence:\n    - name: x\n      dtype: int8\n---\n",
		"---\nbase: &b\n  a: 1\nm:\n  <<: *b\n---\n", "---\nlicense: [mit, 3]\ntags: x\n---\nbody", "no front matter",
		"---\nconfigs:\n- config_name: a\n  default: true\n- config_name: default\n---\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := card.Parse(data, nil)
		if err != nil {
			return
		}
		def, err := c.DefaultConfig()
		if err != nil {
			t.Fatalf("a parsed card has no valid default Config: %v", err)
		}
		if def != "" {
			found := false
			for _, cs := range c.Configs {
				found = found || cs.Name == def
			}
			if !found {
				t.Fatalf("default Config %q is not declared", def)
			}
		}
		if _, err := json.Marshal(c.Data); err != nil {
			t.Fatalf("metadata does not encode: %v", err)
		}
	})
}
