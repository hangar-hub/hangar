package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/cli/clitest"
	"github.com/jamesagarside/hangar/internal/model"
)

func writeTree(t *testing.T, root string, files map[string][]byte) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, c, 0o644))
	}
}

func TestPushCreatesAndCommitsWithMultipartUploads(t *testing.T) {
	for _, presign := range []bool{false, true} {
		t.Run(map[bool]string{false: "hangar part URLs", true: "presigned part URLs"}[presign], func(t *testing.T) {
			e := newEnv(t)
			e.login()
			e.inst.Presign = presign
			dir := filepath.Join(e.home, "model")
			files := map[string][]byte{
				"config.json":          []byte(`{"a":1}`),
				"README.md":            []byte("# Mine\n"),
				"model.safetensors":    blobOf("weights", 200<<10), // four 64 KiB parts
				"sub/tokenizer.json":   blobOf("tok", 3<<10),       // over the inline limit
				".git/HEAD":            []byte("ref: refs/heads/main\n"),
				".cache/huggingface/x": []byte("meta"),
				"model.bin.incomplete": []byte("partial"),
				"extra/skip-me.tmp":    []byte("skip"),
			}
			writeTree(t, dir, files)
			r := e.ok("push", dir, "acme/mine", "--create", "--private", "--exclude", "*.tmp", "-m", "First upload", "--json")
			var res pushResult
			require.NoError(t, json.Unmarshal([]byte(r.stdout), &res))
			require.Equal(t, 4, res.Files)
			require.False(t, res.NoOp)

			id := mustID(t, model.RepoTypeModel, "acme/mine")
			got := e.inst.Files(id, "main")
			for _, p := range []string{"config.json", "README.md", "model.safetensors", "sub/tokenizer.json"} {
				require.Equal(t, files[p], got[p], p)
			}
			for _, p := range []string{".git/HEAD", ".cache/huggingface/x", "model.bin.incomplete", "extra/skip-me.tmp"} {
				require.NotContains(t, got, p)
			}
			require.Equal(t, 4+1, e.inst.PartPuts(), "four parts for the weights, one for the tokenizer")
			require.Equal(t, "private", string(e.inst.Repo(id).Visibility))

			// The same files again: nothing is uploaded and the Commit is a no-op.
			r = e.ok("push", dir, "acme/mine", "--exclude", "*.tmp")
			require.Contains(t, r.stdout, "Nothing to commit")
			require.Equal(t, 5, e.inst.PartPuts())
		})
	}
}

func TestPushResumesAfterAFailedPart(t *testing.T) {
	e := newEnv(t)
	e.login()
	dir := filepath.Join(e.home, "model")
	weights := blobOf("resume", 256<<10)
	writeTree(t, dir, map[string][]byte{"model.safetensors": weights})
	e.inst.AddRepo(mustID(t, model.RepoTypeModel, "acme/resume"))
	e.inst.FailPart(sha(weights), 3, 100)

	r := e.run("push", dir, "acme/resume")
	require.Equal(t, ExitFailure, r.code)
	require.Contains(t, r.err.Error(), "run the command again to resume")
	putsAfterFailure := e.inst.PartPuts()
	require.GreaterOrEqual(t, putsAfterFailure, 1)

	e.inst.FailPart(sha(weights), 3, 0)
	e.ok("push", dir, "acme/resume", "--branch", "main")
	resent := e.inst.PartPuts() - putsAfterFailure
	require.Less(t, resent, 4, "only the parts not stored before are sent again")
	require.GreaterOrEqual(t, resent, 1)
	require.Equal(t, weights, e.inst.Files(mustID(t, model.RepoTypeModel, "acme/resume"), "main")["model.safetensors"])
}

func TestPushErrors(t *testing.T) {
	e := newEnv(t)
	e.login()
	dir := filepath.Join(e.home, "model")
	writeTree(t, dir, map[string][]byte{"a.txt": []byte("a")})

	r := e.run("push", dir, "acme/missing")
	require.Equal(t, ExitNotFound, r.code)
	require.Contains(t, r.err.Error(), "--create")

	r = e.run("push", filepath.Join(e.home, "nope"), "acme/x", "--create")
	require.Equal(t, ExitFailure, r.code)
	r = e.run("push", filepath.Join(dir, "a.txt"), "acme/x", "--create")
	require.Equal(t, ExitUsage, r.code)
	empty := filepath.Join(e.home, "empty")
	require.NoError(t, os.MkdirAll(empty, 0o755))
	r = e.run("push", empty, "acme/x", "--create")
	require.Equal(t, ExitUsage, r.code)

	// A Mirror refuses Commits.
	id, _ := ggufRepo(e)
	e.ok("pull", id.String(), "--server-only")
	r = e.run("push", dir, id.String())
	require.Equal(t, ExitAuth, r.code)
	require.Contains(t, r.err.Error(), "read-only")

	// Without a Token the Instance answers 401.
	e.inst.AddRepo(mustID(t, model.RepoTypeModel, "acme/existing"))
	e.ok("logout")
	r = e.run("push", dir, "acme/existing", "--url", e.inst.URL)
	require.Equal(t, ExitAuth, r.code)
}

func TestPushDatasetAndPullRequest(t *testing.T) {
	e := newEnv(t)
	e.login()
	dir := filepath.Join(e.home, "data")
	writeTree(t, dir, map[string][]byte{"train.csv": []byte("a\n1\n")})
	r := e.ok("push", dir, "datasets/acme/reviews", "--create")
	require.Contains(t, r.stdout, "datasets/acme/reviews")
	require.NotNil(t, e.inst.Repo(mustID(t, model.RepoTypeDataset, "acme/reviews")))
	writeTree(t, dir, map[string][]byte{"test.csv": []byte("a\n2\n")})
	r = e.ok("push", dir, "acme/reviews", "--type", "dataset", "--create-pr", "--description", "More rows")
	require.Contains(t, r.stdout, "Opened pull request #1")
}

func TestJobsListWatchCancel(t *testing.T) {
	e := newEnv(t)
	e.login()
	id, _ := ggufRepo(e)
	e.ok("pull", id.String()+":Q8_0", "--server-only")

	r := e.ok("jobs", "list")
	require.Contains(t, r.stdout, "succeeded")
	require.Contains(t, r.stdout, "bartowski/Tiny-GGUF")
	r = e.ok("jobs", "list", "--all", "--kind", "pull", "--status", "succeeded", "--json")
	var jobs []map[string]any
	require.NoError(t, json.Unmarshal([]byte(r.stdout), &jobs))
	require.Len(t, jobs, 1)
	jobID := jobs[0]["id"].(string)

	r = e.ok("jobs", "watch", jobID, "--json")
	require.Contains(t, r.stdout, `"status": "succeeded"`)
	r = e.ok("jobs", "cancel", jobID)
	require.Contains(t, r.stdout, "Cancelled")
	r = e.ok("jobs", "list", "--status", "failed")
	require.Contains(t, r.stdout, "No Jobs")

	require.Equal(t, ExitUsage, e.run("jobs", "watch", "not-a-uuid").code)
	require.Equal(t, ExitUsage, e.run("jobs", "cancel", "not-a-uuid").code)
	require.Equal(t, ExitUsage, e.run("jobs", "list", "--status", "lost").code)
	require.Equal(t, ExitNotFound, e.run("jobs", "watch", "0192f5c6-0000-7000-8000-000000000000").code)
	require.Equal(t, ExitNotFound, e.run("jobs", "cancel", "0192f5c6-0000-7000-8000-000000000000").code)

	// A cancelled Job ends watch with an error.
	other := mustID(t, model.RepoTypeModel, "acme/slow")
	e.inst.AddUpstream(other, clitest.File{Path: "w.bin", Content: []byte("w")})
	e.inst.JobPolls = 1000
	go func() {
		for range 500 {
			r := e.run("jobs", "list", "--status", "running", "--json")
			if r.err == nil && len(r.stdout) > 10 {
				var js []map[string]any
				if json.Unmarshal([]byte(r.stdout), &js) == nil && len(js) > 0 {
					e.run("jobs", "cancel", js[0]["id"].(string))
					return
				}
			}
		}
	}()
	r = e.run("pull", other.String())
	require.Equal(t, ExitFailure, r.code)
	require.Contains(t, r.err.Error(), "cancelled")
}
