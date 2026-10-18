package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeTools writes docker, kind, kubectl and go stand-ins into a directory
// that each record their arguments in calls. docker image inspect succeeds
// only for the images in local; kind load docker-image fails for the
// images in unloadable, and so does docker save.
func fakeTools(t *testing.T, local, unloadable []string) (dir, calls string) {
	t.Helper()
	dir = t.TempDir()
	calls = filepath.Join(dir, "calls")
	tool := func(name, body string) {
		script := "#!/usr/bin/env bash\necho \"" + name + " $*\" >> " + calls + "\n" + body + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tool("docker", `case "$1 $2" in
"image inspect") for i in `+strings.Join(local, " ")+`; do [[ "$3" == "$i" ]] && exit 0; done; exit 1 ;;
"save "*) exit 1 ;;
esac
exit 0`)
	tool("kind", `if [[ "$1 $2" == "load docker-image" ]]; then
  for i in `+strings.Join(append([]string{"none"}, unloadable...), " ")+`; do [[ "$3" == "$i" ]] && exit 1; done
fi
exit 0`)
	tool("kubectl", `echo arm64`)
	tool("go", `exit 0`)
	return dir, calls
}

func runKindScript(t *testing.T, tools string, image string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", "../scripts/kind-chart-test.sh", image)
	cmd.Env = append(os.Environ(), "PATH="+tools+string(os.PathListSeparator)+os.Getenv("PATH"), "KIND_CLUSTER=hangar-script-test")
	start := time.Now()
	out, err := cmd.CombinedOutput()
	// With the fakes the script takes about a second on a quiet host, but
	// 34 seconds during a full make test on a loaded one. The failure this
	// guards against is a 5 to 6 minute wait for a rollout, so 2 minutes
	// still tells the two apart.
	if d := time.Since(start); d > 2*time.Minute {
		t.Errorf("the script took %s", d)
	}
	return string(out), err
}

// TestKindScriptFailsAtOnceWithoutTheImage checks that
// scripts/kind-chart-test.sh refuses a Hangar image that is not local
// before it creates a cluster. The chart tests run Hangar with
// imagePullPolicy Never, so a missing image used to make every TestKind
// test wait 5 to 6 minutes and fail with ErrImageNeverPull.
func TestKindScriptFailsAtOnceWithoutTheImage(t *testing.T) {
	tools, calls := fakeTools(t, []string{"postgres:18-alpine"}, nil)
	out, err := runKindScript(t, tools, "hangar:missing")
	if err == nil {
		t.Fatalf("the script passed without the image:\n%s", out)
	}
	if !strings.Contains(out, "the Hangar image hangar:missing is not in the local Docker image store") ||
		!strings.Contains(out, "make image IMAGE=hangar:missing") {
		t.Errorf("the script did not explain the missing image:\n%s", out)
	}
	recorded, _ := os.ReadFile(calls)
	if strings.Contains(string(recorded), "kind create") || strings.Contains(string(recorded), "go test") {
		t.Errorf("the script went on without the image:\n%s", recorded)
	}
}

// TestKindScriptFailsWhenTheImageCannotBeLoaded checks that a Hangar image
// that is local but cannot be loaded into the cluster also stops the
// script before the tests, as the node cannot pull it either.
func TestKindScriptFailsWhenTheImageCannotBeLoaded(t *testing.T) {
	tools, calls := fakeTools(t, []string{"hangar:ci"}, []string{"hangar:ci"})
	out, err := runKindScript(t, tools, "hangar:ci")
	if err == nil {
		t.Fatalf("the script passed without loading the image:\n%s", out)
	}
	if !strings.Contains(out, "could not load the Hangar image hangar:ci into the cluster") {
		t.Errorf("the script did not explain the failed load:\n%s", out)
	}
	recorded, _ := os.ReadFile(calls)
	if strings.Contains(string(recorded), "go test") {
		t.Errorf("the script ran the tests without the image:\n%s", recorded)
	}
	if !strings.Contains(string(recorded), "kind delete cluster --name hangar-script-test") {
		t.Errorf("the script did not delete its cluster:\n%s", recorded)
	}
}

// TestKindScriptLoadsTheImageAndRunsTheTests checks the path that works:
// a local Hangar image is loaded, the other images are optional, and the
// TestKind tests run.
func TestKindScriptLoadsTheImageAndRunsTheTests(t *testing.T) {
	tools, calls := fakeTools(t, []string{"hangar:ci"}, nil)
	out, err := runKindScript(t, tools, "hangar:ci")
	if err != nil {
		t.Fatalf("the script failed: %v\n%s", err, out)
	}
	recorded, _ := os.ReadFile(calls)
	for _, want := range []string{"kind load docker-image hangar:ci --name hangar-script-test", "go test -count=1 -tags helm kind -run ^TestKind"} {
		if !strings.Contains(string(recorded), want) {
			t.Errorf("no %q in the calls:\n%s", want, recorded)
		}
	}
}
