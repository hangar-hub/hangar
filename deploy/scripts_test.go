package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestSilentSkipCheck checks that scripts/check-silent-skips.sh catches a
// test that returns early, and so reports PASS, when a backend is not
// configured, and accepts one that calls t.Skipf. Each fixture line starts
// with "> " so the check does not flag this file.
func TestSilentSkipCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		src  string
		fail bool
	}{
		"logs and returns on an Enabled check": {fail: true, src: `
> func TestX(t *testing.T) {
> 	if !blobtest.S3Enabled() {
> 		t.Logf("S3 tests not registered")
> 		return
> 	}
> }`},
		"returns on a helper's Enabled check": {fail: true, src: `
> func TestX(t *testing.T) {
> 	if !s3Enabled(t) {
> 		return
> 	}
> }`},
		"returns on a HANGAR_TEST_ variable": {fail: true, src: `
> func TestX(t *testing.T) {
> 	if os.Getenv("HANGAR_TEST_THING") == "" {
> 		return
> 	}
> }`},
		"logs and returns on any condition": {fail: true, src: `
> func TestX(t *testing.T) {
> 	if !strings.HasPrefix(endpoint, "http://") {
> 		// Needs plain HTTP.
> 		t.Logf("needs plain HTTP")
> 		return
> 	}
> }`},
		"skips with the reason": {src: `
> func TestX(t *testing.T) {
> 	if !blobtest.S3Enabled() {
> 		t.Skipf("needs S3")
> 	}
> 	blobtest.RequireS3(t)
> 	dbtest.RequirePostgres(t)
> }`},
		"returns from a helper process": {src: `
> func TestWorkerProcess(t *testing.T) {
> 	if os.Getenv("HANGAR_W6_WORKER_DSN") == "" {
> 		return
> 	}
> }`},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "x_test.go"), []byte("package x\n"+strings.ReplaceAll(tc.src, "\n> ", "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("bash", "../scripts/check-silent-skips.sh", dir).CombinedOutput()
			if tc.fail {
				if err == nil {
					t.Fatalf("the check passed")
				}
				if !strings.Contains(string(out), "x_test.go:4: the test returns early and reports PASS") {
					t.Errorf("the check did not name the line:\n%s", out)
				}
			} else if err != nil {
				t.Fatalf("the check failed: %v\n%s", err, out)
			}
		})
	}
}

// TestDocLinkCheck checks that scripts/check-doc-links.py catches a relative
// Markdown link to a file that does not exist, such as docs/build linking
// 03-hub-api.md for 03-hf-api.md, and accepts links that resolve, external
// links, anchors and links inside code.
func TestDocLinkCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		src  string
		fail bool
	}{
		"links a missing spec file":           {fail: true, src: "Intro.\n\nSee [03](spec/03-hub-api.md).\n"},
		"links a missing file with an anchor": {fail: true, src: "Intro.\n\nSee [03](spec/03-hub-api.md#xet).\n"},
		"links a missing image":               {fail: true, src: "Intro.\n\nThe ![diagram](missing.svg).\n"},
		"links files that exist":              {src: "See [03](spec/03-hf-api.md), [03 Xet](spec/03-hf-api.md#xet) and [the folder](spec/).\n"},
		"links elsewhere":                     {src: "See [the Hub](https://huggingface.co), [mail](mailto:a@example.com) and [above](#intro).\n"},
		"links inside code":                   {src: "Use `[x](missing.md)`.\n\n```markdown\n[x](missing.md)\n```\n"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "spec"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "spec", "03-hf-api.md"), []byte("# 03\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			page := filepath.Join(dir, "page.md")
			if err := os.WriteFile(page, []byte(tc.src), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("python3", "../scripts/check-doc-links.py", page).CombinedOutput()
			if tc.fail {
				if err == nil {
					t.Fatalf("the check passed:\n%s", out)
				}
				if !strings.Contains(string(out), "page.md:3: broken link") {
					t.Errorf("the check did not name the line:\n%s", out)
				}
			} else if err != nil {
				t.Fatalf("the check failed: %v\n%s", err, out)
			}
		})
	}
}

// TestTestCitationCheck checks that scripts/check-test-citations.py catches a
// test cited at a file that does not define it, such as the threat model
// putting TestHTTPClientRedactsPresignedURLs in internal/cli/redact_test.go,
// and a name no test file defines. The fixtures cite real tests and files in
// the repository.
func TestTestCitationCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		src  string
		want string
	}{
		"cites two tests at the file of only one": {
			want: "page.md:3: TestHTTPClientRedactsPresignedURLs is not in internal/cli/redact_test.go",
			src:  "Intro.\n\n| I | Leak. | Redact. | `TestHTTPClientRedactsPresignedURLs`, `TestPullDoesNotPrintPresignedDownloadURLs` (`internal/cli/redact_test.go`). |\n",
		},
		"cites a test that does not exist": {
			want: "page.md:3: TestNoSuchThingAnywhere is not defined in any test file",
			src:  "Intro.\n\nKept by `TestNoSuchThingAnywhere`.\n",
		},
		"cites each test at its file": {
			src: "Intro.\n\n| I | Leak. | Redact. | `TestHTTPClientRedactsPresignedURLs` (`internal/upstream/client_test.go`), `TestPullDoesNotPrintPresignedDownloadURLs` (`internal/cli/redact_test.go`); `TestSecretsStayOutOfLogsAndTelemetry`. |\n",
		},
		"cites a test with a note and no file": {
			src: "Intro.\n\n`TestPermissionMatrix` (with its `hub_admin` column), `FuzzWhere` (`internal/datasets/query/query_test.go`).\n",
		},
		"names a test inside a code block": {
			src: "Intro.\n\n```text\nTestNoSuchThingAnywhere\n`TestNoSuchThingAnywhere`\n```\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			page := filepath.Join(t.TempDir(), "page.md")
			if err := os.WriteFile(page, []byte(tc.src), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("python3", "../scripts/check-test-citations.py", page).CombinedOutput()
			if tc.want != "" {
				if err == nil {
					t.Fatalf("the check passed:\n%s", out)
				}
				if !strings.Contains(string(out), tc.want) {
					t.Errorf("the check did not report %q:\n%s", tc.want, out)
				}
			} else if err != nil {
				t.Fatalf("the check failed: %v\n%s", err, out)
			}
		})
	}
}

// TestPercentSpacingCheck checks that scripts/check-spelling.sh catches a
// space before a percent sign, as in "0.11 % against 0.1 %", and leaves
// percentages written without one, and ones in code, alone.
func TestPercentSpacingCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		src  string
		fail bool
	}{
		"spaces a percentage":     {fail: true, src: "Intro.\n\nIt failed (0.11 % against 0.1%).\n"},
		"writes it without space": {src: "Intro.\n\nIt failed (0.11% against 0.1%), at 75% per package.\n"},
		"spaces it inside code":   {src: "Intro.\n\nRun `printf '%d %%' 5 %`.\n"},
	} {
		t.Run(name, func(t *testing.T) {
			page := filepath.Join(t.TempDir(), "page.md")
			if err := os.WriteFile(page, []byte(tc.src), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command("bash", "../scripts/check-spelling.sh", page).CombinedOutput()
			if tc.fail {
				if err == nil {
					t.Fatalf("the check passed:\n%s", out)
				}
				if !strings.Contains(string(out), "page.md:3:") || !strings.Contains(string(out), "no space before the percent sign") {
					t.Errorf("the check did not name the line:\n%s", out)
				}
			} else if err != nil {
				t.Fatalf("the check failed: %v\n%s", err, out)
			}
		})
	}
}

// TestDocsRootFiles checks that scripts/docs-root-files.sh puts the files in
// deploy/cloudflare/docs-root, dotfiles included, at the root of gh-pages,
// which the docs Worker serves, without touching mike's version directories, the checked-out branch
// or the working tree, and commits nothing when they are already there.
func TestDocsRootFiles(t *testing.T) {
	repo := t.TempDir()
	// The script commits too, so it runs with the same identity: CI runners
	// have none of their own.
	gitEnv := append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script, err := os.ReadFile("../scripts/docs-root-files.sh")
	if err != nil {
		t.Fatal(err)
	}
	git("init", "-q", "-b", "main")
	write("scripts/docs-root-files.sh", string(script))
	rootFiles, err := os.ReadDir(filepath.Join("cloudflare", "docs-root"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"dev/index.html", "index.html"}
	for _, f := range rootFiles {
		body, err := os.ReadFile(filepath.Join("cloudflare", "docs-root", f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		write(filepath.Join("deploy/cloudflare/docs-root", f.Name()), string(body))
		want = append(want, f.Name())
	}
	slices.Sort(want)
	// The home page in docs-root replaces mike's root redirect.
	want = slices.Compact(want)
	git("add", ".")
	git("commit", "-q", "-m", "main")
	// What mike leaves: a version directory and the root redirect.
	git("checkout", "-q", "--orphan", "gh-pages")
	git("rm", "-q", "-r", "--cached", ".")
	write("dev/index.html", "dev")
	write("index.html", "redirect")
	git("add", "dev/index.html", "index.html")
	git("commit", "-q", "-m", "Deployed dev")
	git("checkout", "-q", "-f", "main")

	run := func() string {
		t.Helper()
		cmd := exec.Command(filepath.Join(repo, "scripts/docs-root-files.sh"))
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the script failed: %v\n%s", err, out)
		}
		return string(out)
	}
	if out := run(); !strings.Contains(out, "gh-pages now has the root files") {
		t.Fatalf("the script did not commit:\n%s", out)
	}
	files := strings.Fields(git("ls-tree", "-r", "--name-only", "gh-pages"))
	if !slices.Equal(files, want) {
		t.Errorf("gh-pages has %v, want %v", files, want)
	}
	if got := git("show", "gh-pages:_headers"); !strings.Contains(got, "Content-Security-Policy:") {
		t.Errorf("gh-pages:_headers is not the one in deploy/cloudflare/docs-root:\n%s", got)
	}
	if got := git("show", "gh-pages:index.html"); !strings.Contains(got, "<title>Hangar") {
		t.Errorf("gh-pages:index.html is not the home page in deploy/cloudflare/docs-root:\n%.200s", got)
	}
	if !slices.Contains(files, ".assetsignore") || !slices.Contains(files, "wrangler.jsonc") {
		t.Errorf("gh-pages is missing the Worker's .assetsignore or wrangler.jsonc: %v", files)
	}
	if branch := git("branch", "--show-current"); branch != "main" {
		t.Errorf("the script left %s checked out", branch)
	}
	if status := git("status", "--porcelain"); status != "" {
		t.Errorf("the script changed the working tree:\n%s", status)
	}
	head := git("rev-parse", "gh-pages")
	if out := run(); !strings.Contains(out, "already has the root files") {
		t.Errorf("the second run did not see the files:\n%s", out)
	}
	if again := git("rev-parse", "gh-pages"); again != head {
		t.Errorf("the second run committed %s", again)
	}
}

// TestDocsOutdatedBanner checks that scripts/docs-outdated-banner.py fills
// the empty banner placeholder that Material renders in docs versions built
// before overrides/main.html, with that file's text and a link to the page's
// canonical URL, leaves pages that already have the banner alone, and
// refuses a version that is not published.
func TestDocsOutdatedBanner(t *testing.T) {
	site := t.TempDir()
	const empty = `<div data-md-color-scheme="default" data-md-component="outdated" hidden>

      </div>`
	pages := map[string]string{
		"1.0/index.html":               `<head><link rel="canonical" href="https://hangar.page/latest/"></head><body>` + empty + `</body>`,
		"1.0/guides/ollama/index.html": `<head><link rel="canonical" href="https://hangar.page/latest/guides/ollama/"></head><body>` + empty + `</body>`,
		"1.0/404.html":                 `<head></head><body>` + empty + `</body>`,
	}
	for name, body := range pages {
		path := filepath.Join(site, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(versions ...string) (string, error) {
		args := append([]string{"../scripts/docs-outdated-banner.py", site}, versions...)
		out, err := exec.Command("python3", args...).CombinedOutput()
		return string(out), err
	}
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(site, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	out, err := run("1.0")
	if err != nil {
		t.Fatalf("the script failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "added the banner to 3 of 3 pages") {
		t.Errorf("the script did not report three pages:\n%s", out)
	}
	template, err := os.ReadFile("../overrides/main.html")
	if err != nil {
		t.Fatal(err)
	}
	const sentence = "You're reading the documentation for an older version of Hangar."
	if !strings.Contains(string(template), sentence) {
		t.Fatalf("overrides/main.html no longer says %q; update this test", sentence)
	}
	for name, href := range map[string]string{
		"1.0/guides/ollama/index.html": "https://hangar.page/latest/guides/ollama/",
		"1.0/404.html":                 "https://hangar.page/latest/",
	} {
		got := read(name)
		if !strings.Contains(got, `data-md-component="outdated" hidden><aside class="md-banner md-banner--warning">`) ||
			!strings.Contains(got, sentence) || !strings.Contains(got, `<a href="`+href+`">`) {
			t.Errorf("%s has no banner linking to %s:\n%s", name, href, got)
		}
		if strings.Contains(got, "{{") {
			t.Errorf("%s has a template expression left in it:\n%s", name, got)
		}
	}

	before := read("1.0/guides/ollama/index.html")
	if out, err := run("1.0"); err != nil || !strings.Contains(out, "added the banner to 0 of 3 pages") {
		t.Errorf("the second run changed pages, or failed: %v\n%s", err, out)
	}
	if read("1.0/guides/ollama/index.html") != before {
		t.Error("the second run changed a page that already had the banner")
	}
	if out, err := run("0.9"); err == nil {
		t.Errorf("the script accepted a version that is not published:\n%s", out)
	}
}

// TestIndexNow checks that scripts/indexnow.py submits the pages that changed
// between two gh-pages commits: new pages, pages with a new <lastmod>, the
// home page when its file changed, and pages that have gone, read from the
// sitemaps robots.txt lists, through mike's latest symlink. It also checks
// that the root of docs-root holds a key file IndexNow accepts.
func TestIndexNow(t *testing.T) {
	repo := t.TempDir()
	gitEnv := append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, body string) {
		t.Helper()
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sitemap := func(entries ...string) string {
		var b strings.Builder
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
		for i := 0; i < len(entries); i += 2 {
			b.WriteString("<url><loc>https://hangar.page/latest/" + entries[i] + "</loc>")
			if entries[i+1] != "" {
				b.WriteString("<lastmod>" + entries[i+1] + "</lastmod>")
			}
			b.WriteString("</url>")
		}
		return b.String() + "</urlset>"
	}
	robots, err := os.ReadFile(filepath.Join("cloudflare", "docs-root", "robots.txt"))
	if err != nil {
		t.Fatal(err)
	}
	homeSitemap, err := os.ReadFile(filepath.Join("cloudflare", "docs-root", "sitemap.xml"))
	if err != nil {
		t.Fatal(err)
	}

	git("init", "-q", "-b", "gh-pages")
	write("robots.txt", string(robots))
	write("sitemap.xml", string(homeSitemap))
	write("index.html", "home")
	write("1.0/sitemap.xml", sitemap("", "2026-10-20", "faq/", "2026-10-20", "guides/git/", "2026-10-20", "guides/old/", "2026-10-20"))
	if err := os.Symlink("1.0", filepath.Join(repo, "latest")); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "1.0")
	before := git("rev-parse", "HEAD")

	// 1.1 changes the FAQ, adds a guide, drops one, and becomes latest.
	write("1.1/sitemap.xml", sitemap("", "2026-10-20", "faq/", "2026-10-27", "guides/git/", "2026-10-20", "guides/new/", "2026-10-27"))
	git("rm", "-q", "latest")
	if err := os.Symlink("1.1", filepath.Join(repo, "latest")); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "1.1")
	afterDocs := git("rev-parse", "HEAD")
	write("index.html", "new home")
	git("commit", "-q", "-am", "home")

	run := func(args ...string) string {
		t.Helper()
		abs, err := filepath.Abs("../scripts/indexnow.py")
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("python3", append([]string{abs, "--dry-run"}, args...)...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("the script failed: %v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	for _, tc := range []struct {
		name          string
		before, after string
		want          []string
	}{
		{"new version", before, afterDocs, []string{
			"https://hangar.page/latest/faq/", "https://hangar.page/latest/guides/new/", "https://hangar.page/latest/guides/old/"}},
		{"home page", afterDocs, "HEAD", []string{"https://hangar.page/"}},
		{"nothing", "HEAD", "HEAD", nil},
		{"first deploy", "", before, []string{
			"https://hangar.page/", "https://hangar.page/latest/", "https://hangar.page/latest/faq/",
			"https://hangar.page/latest/guides/git/", "https://hangar.page/latest/guides/old/"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Fields(run(tc.before, tc.after))
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}

	// IndexNow reads the key from https://hangar.page/<key>.txt.
	entries, err := os.ReadDir(filepath.Join("cloudflare", "docs-root"))
	if err != nil {
		t.Fatal(err)
	}
	keyFile := regexp.MustCompile(`^([0-9a-f]{32})\.txt$`)
	var keys []string
	for _, e := range entries {
		m := keyFile.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		body, err := os.ReadFile(filepath.Join("cloudflare", "docs-root", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != m[1] {
			t.Errorf("%s holds %q, want only the key", e.Name(), body)
		}
		keys = append(keys, m[1])
	}
	if len(keys) != 1 {
		t.Errorf("docs-root has %d IndexNow key files, want 1", len(keys))
	}
}
