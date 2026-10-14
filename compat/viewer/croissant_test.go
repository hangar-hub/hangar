//go:build compat

package viewer

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/datasets/viewertest"
	"github.com/jamesagarside/hangar/internal/model"
)

// TestCroissantValidates converts every compat/viewer dataset, plus a
// dataset with several Configs and nested Features, and validates the
// Croissant document Hangar serves for each with mlcroissant.
func TestCroissantValidates(t *testing.T) {
	work := t.TempDir()
	s := viewertest.New(t, viewertest.Options{})
	var paths []string
	save := func(name string, id model.RepoID) {
		doc, err := s.Viewer.Croissant(context.Background(), model.Anonymous(), id)
		require.NoError(t, err, name)
		p := filepath.Join(work, name+".json")
		require.NoError(t, os.WriteFile(p, doc, 0o600))
		paths = append(paths, p)
	}
	for _, entry := range fixtures(t) {
		_, name, _ := strings.Cut(entry.Repo, "/")
		r, sha := s.Dataset(t, name, sourceFiles(t, entry.Repo))
		s.Convert(t, r, sha)
		save(name, r.ID)
	}
	r, sha := s.Dataset(t, "nested", map[string][]byte{
		"README.md": []byte("---\nlicense: apache-2.0\npretty_name: Nested\nconfigs:\n- config_name: one\n  data_files: one/*.jsonl\n" +
			"- config_name: two words\n  data_files:\n  - split: train\n    path: two/*.csv\n  - split: test\n    path: two/test.csv\n---\n# Nested\n\nRows with nested values.\n"),
		"one/a.jsonl": []byte(`{"id": 1, "tags": ["a", "b"], "meta": {"source": "x", "score": 0.5}, "when": "2024-01-02T03:04:05"}` + "\n" +
			`{"id": 2, "tags": [], "meta": {"source": "y", "score": null}, "when": "2024-02-03T04:05:06"}` + "\n"),
		"two/train.csv": []byte("text,label\nhello,1\nworld,0\n"),
		"two/test.csv":  []byte("text,label\nagain,1\n"),
	})
	s.Convert(t, r, sha)
	save("nested", r.ID)

	args := append([]string{"run", "--no-project", "--quiet", "--with", "mlcroissant==1.1.1",
		"python", "-I", filepath.Join(here(), "oracle", "croissant_validate.py")}, paths...)
	cmd := exec.Command("uv", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, "run mlcroissant: %s", stderr.String())
	var results map[string]struct {
		Errors   []string `json:"errors"`
		Warnings []string `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(out, &results))
	require.Len(t, results, len(paths))
	for _, p := range paths {
		res := results[p]
		name := filepath.Base(p)
		require.Empty(t, res.Errors, "%s: mlcroissant errors", name)
		// Recommended properties the card does not give (a licence, a
		// citation) are left out, as the Hub leaves them out; any other
		// warning fails.
		var other []string
		for _, w := range res.Warnings {
			if strings.HasSuffix(w, "is recommended, but does not exist.") {
				t.Logf("%s: %s", name, w)
				continue
			}
			other = append(other, w)
		}
		require.Empty(t, other, "%s: mlcroissant warnings", name)
	}
}
