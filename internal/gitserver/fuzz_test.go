package gitserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hangar-hub/hangar/internal/model"
)

// FuzzParseBatch checks that no batch body panics the parser and that every
// accepted request has a valid operation and object count.
func FuzzParseBatch(f *testing.F) {
	f.Add([]byte(`{"operation":"upload","transfers":["basic","multipart"],"objects":[{"oid":"` + strings.Repeat("a", 64) + `","size":1}],"hash_algo":"sha256"}`))
	f.Add([]byte(`{"operation":"download","objects":[],"ref":{"name":"refs/heads/main"}}`))
	f.Add([]byte(`{"operation":"download","objects":[{"oid":1}]}`))
	f.Add([]byte(`[]`))
	f.Fuzz(func(t *testing.T, body []byte) {
		req, err := parseBatch(body)
		if err != nil {
			return
		}
		if req.Operation != "upload" && req.Operation != "download" {
			t.Fatalf("accepted operation %q", req.Operation)
		}
		if len(req.Objects) > maxBatchObjects {
			t.Fatalf("accepted %d objects", len(req.Objects))
		}
		for _, o := range req.Objects {
			_ = validObject(o)
		}
	})
}

// FuzzParseRepoPath checks that accepted paths name a valid Repo and never
// carry traversal segments.
func FuzzParseRepoPath(f *testing.F) {
	for _, s := range []string{"/alice/tiny.git/info/refs", "/datasets/a/b/git-upload-pack", "/a/b.git/info/lfs/objects/batch", "/a/b/%2e%2e", "//", "/a/b.git/x%2Fy"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, path string) {
		rp, err := parseRepoPath(path)
		if err != nil {
			return
		}
		if err := rp.ID.Validate(); err != nil {
			t.Fatalf("accepted invalid repo id %v: %v", rp.ID, err)
		}
		for _, s := range rp.Rest {
			if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\\") {
				t.Fatalf("accepted segment %q", s)
			}
		}
	})
}

// FuzzReadPackedRefs checks that packed-refs parsing never panics and keeps
// only well-formed entries.
func FuzzReadPackedRefs(f *testing.F) {
	f.Add("# pack-refs with: peeled\n" + strings.Repeat("a", 40) + " refs/heads/main\n^" + strings.Repeat("b", 40) + "\n")
	f.Add("garbage\n\n \n")
	f.Fuzz(func(t *testing.T, content string) {
		path := filepath.Join(t.TempDir(), "packed-refs")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		refs := map[string]string{}
		if err := readPackedRefs(path, refs); err != nil {
			return
		}
		for name, sha := range refs {
			if !strings.HasPrefix(name, "refs/") || !model.IsFullSHA(sha) || sha != strings.ToLower(sha) {
				t.Fatalf("kept %q -> %q", name, sha)
			}
		}
	})
}
