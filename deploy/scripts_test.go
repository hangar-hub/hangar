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
