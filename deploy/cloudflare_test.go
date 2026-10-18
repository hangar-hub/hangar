package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestDocsRedirectsCoverEverySection checks that deploy/cloudflare/docs-root/
// _redirects sends every top-level section of the documentation site to the
// latest version. MkDocs builds each version's 404.html with links from the
// site root, such as /get-started/, and Cloudflare Pages serves that page for
// missing pages under the version, so a section with no rule breaks its links.
func TestDocsRedirectsCoverEverySection(t *testing.T) {
	raw, err := os.ReadFile("cloudflare/docs-root/_redirects")
	if err != nil {
		t.Fatal(err)
	}
	rules := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || strings.HasPrefix(f[0], "#") {
			continue
		}
		if len(f) != 3 {
			t.Fatalf("rule %q does not have a source, a target and a status", line)
		}
		rules[f[0]] = f[1] + " " + f[2]
	}

	// The site's top-level paths: docs/ less what mkdocs.yml excludes, and the
	// search index Material writes.
	excluded := map[string]bool{"spec": true, "research": true, "build": true, "__pycache__": true,
		"index.md": true, "dependencies.md": true, "requirements.txt": true, "hooks.py": true}
	sections := []string{"search"}
	entries, err := os.ReadDir("../docs")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case excluded[name]:
		case e.IsDir():
			sections = append(sections, name)
		case filepath.Ext(name) == ".md":
			sections = append(sections, strings.TrimSuffix(name, ".md"))
		}
	}
	for _, s := range sections {
		want := "/latest/" + s + "/:splat 301"
		if s == "assets" || s == "search" {
			// Files the 404 page loads: served from latest, not redirected.
			want = "/latest/" + s + "/:splat 200"
		}
		if got := rules["/"+s+"/*"]; got != want {
			t.Errorf("_redirects has %q for /%s/*, want %q", got, s, want)
		}
	}
	if len(rules) != len(sections) {
		t.Errorf("_redirects has %d rules for %d sections: %v", len(rules), len(sections), rules)
	}
}
