package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
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
