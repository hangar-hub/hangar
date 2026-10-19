package deploy

import (
	"encoding/json"
	"html"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestDocsRedirectsCoverEverySection checks that deploy/cloudflare/docs-root/
// _redirects sends every top-level section of the documentation site to the
// latest version. MkDocs builds each version's 404.html with links from the
// site root, such as /get-started/, and the docs Worker serves that page for
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

// wranglerConfig is the part of a wrangler.jsonc the docs Worker's tests read.
type wranglerConfig struct {
	Schema string `json:"$schema"`
	Name   string `json:"name"`
	Main   string `json:"main"`
	Assets struct {
		Directory        string `json:"directory"`
		NotFoundHandling string `json:"not_found_handling"`
	} `json:"assets"`
	Routes      []wranglerRoute `json:"routes"`
	Previews    *struct{}       `json:"previews"`
	WorkersDev  bool            `json:"workers_dev"`
	PreviewURLs bool            `json:"preview_urls"`
}

// wranglerRoute is a route in a wrangler.jsonc. Enabled is nil when the file
// leaves it out, which serves production traffic.
type wranglerRoute struct {
	Pattern         string `json:"pattern"`
	CustomDomain    bool   `json:"custom_domain"`
	PreviewsEnabled bool   `json:"previews_enabled"`
	Enabled         *bool  `json:"enabled"`
}

func (r wranglerRoute) servesProduction() bool { return r.Enabled == nil || *r.Enabled }

// readWranglerConfig parses a wrangler.jsonc whose comments are whole lines,
// and checks that it pins the wrangler version the Makefile pins, which the
// commands that deploy it run.
func readWranglerConfig(t *testing.T, path string) wranglerConfig {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAll(raw, nil)
	var cfg wranglerConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	makefile, err := os.ReadFile("../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	pin := regexp.MustCompile(`wrangler@(\d+\.\d+\.\d+)`).FindSubmatch(makefile)
	if pin == nil {
		t.Fatal("the Makefile pins no wrangler version")
	}
	if want := "https://unpkg.com/wrangler@" + string(pin[1]) + "/config-schema.json"; cfg.Schema != want {
		t.Errorf("%s: $schema = %q, want %q", path, cfg.Schema, want)
	}
	return cfg
}

// TestDocsWorkerConfig checks deploy/cloudflare/docs-root/wrangler.jsonc, which
// Workers Builds deploys from the root of gh-pages: the Worker serves that
// root as static assets on hangar.page, answers missing pages with the nearest
// 404.html, serves branch Previews only under docs.hangar.page and nothing on
// workers.dev, and .assetsignore keeps its configuration and the git checkout
// off the site.
func TestDocsWorkerConfig(t *testing.T) {
	cfg := readWranglerConfig(t, "cloudflare/docs-root/wrangler.jsonc")
	if cfg.Name != "hangar-docs" || cfg.Main != "" || cfg.Assets.Directory != "." ||
		cfg.Assets.NotFoundHandling != "404-page" || cfg.WorkersDev || cfg.PreviewURLs {
		t.Errorf("wrangler.jsonc is not an assets-only Worker named hangar-docs serving its own directory, off workers.dev: %+v", cfg)
	}
	if len(cfg.Routes) != 2 {
		t.Fatalf("wrangler.jsonc routes = %+v, want hangar.page and docs.hangar.page", cfg.Routes)
	}
	if r := cfg.Routes[0]; r.Pattern != "hangar.page" || !r.CustomDomain || !r.servesProduction() || r.PreviewsEnabled {
		t.Errorf("route %+v, want the custom domain hangar.page for production only", r)
	}
	if r := cfg.Routes[1]; r.Pattern != "docs.hangar.page" || !r.CustomDomain || r.servesProduction() || !r.PreviewsEnabled {
		t.Errorf("route %+v, want the custom domain docs.hangar.page for Previews only", r)
	}

	ignore, err := os.ReadFile("cloudflare/docs-root/.assetsignore")
	if err != nil {
		t.Fatal(err)
	}
	patterns := map[string]bool{}
	for _, line := range strings.Split(string(ignore), "\n") {
		patterns[strings.TrimSpace(line)] = true
	}
	for _, want := range []string{"wrangler.jsonc", ".assetsignore", ".git", ".wrangler"} {
		if !patterns[want] {
			t.Errorf(".assetsignore does not list %s", want)
		}
	}
}

// TestDocsPreviewConfig checks deploy/cloudflare/docs-preview/wrangler.jsonc,
// which scripts/docs-preview.sh publishes branch Previews with: the same
// Worker as production, the site/ that the script builds at the root of the
// checkout, a previews block, which `wrangler preview` requires, and no route,
// so a Preview never takes hangar.page.
func TestDocsPreviewConfig(t *testing.T) {
	cfg := readWranglerConfig(t, "cloudflare/docs-preview/wrangler.jsonc")
	if cfg.Name != "hangar-docs" || cfg.Main != "" || cfg.Previews == nil || len(cfg.Routes) != 0 ||
		cfg.Assets.NotFoundHandling != "404-page" || cfg.WorkersDev || cfg.PreviewURLs {
		t.Errorf("the preview config is not an assets-only hangar-docs with a previews block, no routes and nothing on workers.dev: %+v", cfg)
	}
	dir, err := filepath.Abs(filepath.Join("cloudflare", "docs-preview", cfg.Assets.Directory))
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.Abs(filepath.Join("..", "site"))
	if err != nil {
		t.Fatal(err)
	}
	if dir != want {
		t.Errorf("the preview config serves %s, want %s, where scripts/docs-preview.sh builds", dir, want)
	}
}

// TestDemoWorkerConfig checks ui/demo/wrangler.jsonc: production and branch
// Previews on demo.hangar.page (a Preview at <name>.demo.hangar.page), a
// previews block, which `make demo-preview` needs, and nothing on workers.dev.
func TestDemoWorkerConfig(t *testing.T) {
	cfg := readWranglerConfig(t, "../ui/demo/wrangler.jsonc")
	if cfg.Name != "hangar-demo" || cfg.Previews == nil || cfg.WorkersDev || cfg.PreviewURLs {
		t.Errorf("ui/demo/wrangler.jsonc is not hangar-demo with a previews block, off workers.dev: %+v", cfg)
	}
	if len(cfg.Routes) != 1 {
		t.Fatalf("ui/demo/wrangler.jsonc routes = %+v, want demo.hangar.page", cfg.Routes)
	}
	if r := cfg.Routes[0]; r.Pattern != "demo.hangar.page" || !r.CustomDomain || !r.servesProduction() || !r.PreviewsEnabled {
		t.Errorf("route %+v, want the custom domain demo.hangar.page for production and Previews", r)
	}
}

// TestPreviewName checks scripts/preview-name.sh, which names a branch's
// Preview and so the first label of its address: a DNS label of at most 50
// characters, from WORKERS_CI_BRANCH in Workers Builds.
func TestPreviewName(t *testing.T) {
	run := func(env []string, args ...string) (string, error) {
		cmd := exec.Command("../scripts/preview-name.sh", args...)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	for branch, want := range map[string]string{
		"main":                 "main",
		"feat/docs-on-workers": "feat-docs-on-workers",
		"Fix/UPPER__case-":     "fix-upper-case",
		"dependabot/npm_and_yarn/ui/types/node-26.0.0":       "dependabot-npm-and-yarn-ui-types-node-26-0-0",
		"release-please--branches--main--components--hangar": "release-please-branches-main-components-hangar",
		strings.Repeat("a", 49) + "-b":                       strings.Repeat("a", 49),
	} {
		got, err := run(nil, branch)
		if err != nil || got != want {
			t.Errorf("preview-name.sh %q = %q, %v; want %q", branch, got, err, want)
		}
	}
	if got, err := run([]string{"WORKERS_CI_BRANCH=feat/x"}); err != nil || got != "feat-x" {
		t.Errorf("with WORKERS_CI_BRANCH=feat/x: %q, %v; want feat-x", got, err)
	}
	if _, err := run(nil, "///"); err == nil {
		t.Error("preview-name.sh accepted a branch with no letters or digits")
	}
}

// TestDocsHomePage checks the home page at the root of https://hangar.page/,
// deploy/cloudflare/docs-root/index.html: the metadata search engines and link
// previews read, that its JSON-LD parses and its FAQ says what the page says,
// that every local link names a docs page or a root file, and that robots.txt
// and the sitemap list it.
func TestDocsHomePage(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join("cloudflare", "docs-root", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	page := read("index.html")
	first := func(pattern string) string {
		t.Helper()
		m := regexp.MustCompile(pattern).FindStringSubmatch(page)
		if m == nil {
			t.Errorf("the home page has no match for %s", pattern)
			return ""
		}
		return html.UnescapeString(m[1])
	}

	if title := first(`<title>([^<]+)</title>`); len(title) > 60 || !strings.HasPrefix(title, "Hangar") {
		t.Errorf("title %q should start with Hangar and fit in 60 characters", title)
	}
	if desc := first(`<meta\s+name="description"\s+content="([^"]+)"`); len(desc) < 70 || len(desc) > 160 {
		t.Errorf("description is %d characters, want 70 to 160: %q", len(desc), desc)
	}
	if got := first(`<link rel="canonical" href="([^"]+)"`); got != "https://hangar.page/" {
		t.Errorf("canonical = %q", got)
	}
	if got := first(`<html lang="([^"]+)"`); got != "en-GB" {
		t.Errorf("lang = %q, want en-GB", got)
	}
	if n := strings.Count(page, "<h1"); n != 1 {
		t.Errorf("the home page has %d h1 elements, want 1", n)
	}
	if got := first(`<meta property="og:image" content="([^"]+)"`); got != "https://hangar.page/og.png" {
		t.Errorf("og:image = %q", got)
	}
	og, err := os.Open(filepath.Join("cloudflare", "docs-root", "og.png"))
	if err != nil {
		t.Fatal(err)
	}
	defer og.Close()
	cfg, err := png.DecodeConfig(og)
	if err != nil {
		t.Fatalf("og.png: %v", err)
	}
	if cfg.Width != 1200 || cfg.Height != 630 {
		t.Errorf("og.png is %dx%d, want 1200x630", cfg.Width, cfg.Height)
	}

	// The FAQ in the JSON-LD must be the one on the page, in order.
	var ld struct {
		Graph []struct {
			Type       string `json:"@type"`
			MainEntity []struct {
				Name   string `json:"name"`
				Answer struct {
					Text string `json:"text"`
				} `json:"acceptedAnswer"`
			} `json:"mainEntity"`
		} `json:"@graph"`
	}
	if err := json.Unmarshal([]byte(first(`(?s)<script type="application/ld\+json">(.*?)</script>`)), &ld); err != nil {
		t.Fatalf("JSON-LD: %v", err)
	}
	var types []string
	var faq []string
	for _, node := range ld.Graph {
		types = append(types, node.Type)
		for _, q := range node.MainEntity {
			faq = append(faq, q.Name, q.Answer.Text)
		}
	}
	if !slices.Equal(types, []string{"WebSite", "SoftwareApplication", "FAQPage"}) {
		t.Errorf("JSON-LD types = %v", types)
	}
	var shown []string
	tags := regexp.MustCompile(`<[^>]+>`)
	for _, m := range regexp.MustCompile(`(?s)<details>\s*<summary>(.*?)</summary>\s*<p>(.*?)</p>`).FindAllStringSubmatch(page, -1) {
		shown = append(shown, html.UnescapeString(tags.ReplaceAllString(m[1], "")), html.UnescapeString(m[2]))
	}
	if len(shown) == 0 || !slices.Equal(faq, shown) {
		t.Errorf("the JSON-LD FAQ does not match the page:\nJSON-LD %q\npage    %q", faq, shown)
	}

	// Local links: /latest/<page>/ names a page in docs/, /latest/assets/...
	// a file there, and anything else a file beside the home page.
	for _, m := range regexp.MustCompile(`(?:href|src)="(/[^"]*)"`).FindAllStringSubmatch(page, -1) {
		link := m[1]
		var candidates []string
		switch {
		case link == "/":
			continue
		case strings.HasPrefix(link, "/latest/assets/"):
			candidates = []string{filepath.Join("..", "docs", strings.TrimPrefix(link, "/latest/"))}
		case strings.HasPrefix(link, "/latest/"):
			p := strings.TrimSuffix(strings.TrimPrefix(link, "/latest/"), "/")
			if p == "" {
				p = "index"
			}
			candidates = []string{filepath.Join("..", "docs", p+".md"), filepath.Join("..", "docs", p, "index.md")}
		default:
			candidates = []string{filepath.Join("cloudflare", "docs-root", strings.TrimPrefix(link, "/"))}
		}
		found := false
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				found = true
			}
		}
		if !found {
			t.Errorf("the home page links to %s, but none of %v exists", link, candidates)
		}
	}

	if robots := read("robots.txt"); !strings.Contains(robots, "Sitemap: https://hangar.page/sitemap.xml") {
		t.Errorf("robots.txt does not list the home page's sitemap:\n%s", robots)
	}
	if sitemap := read("sitemap.xml"); !strings.Contains(sitemap, "<loc>https://hangar.page/</loc>") {
		t.Errorf("sitemap.xml does not list the home page:\n%s", sitemap)
	}
}
