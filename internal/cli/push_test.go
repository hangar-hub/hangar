package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/cli/clitest"
	"github.com/hangar-hub/hangar/internal/model"
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
			// An Instance with hfapi.regular_upload_max at 1 KiB, so a small
			// file that .gitattributes leaves out of LFS still goes up as an
			// upload.
			e.inst.InlineMax = 1 << 10
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
	waits := recordWaits(t)
	// Four 64 KiB parts go up in parallel. Part 3 fails every time it is
	// sent, and only after parts 1, 2 and 4 are stored, whatever order the
	// requests arrive in.
	e.inst.FailPartLast(sha(weights), 3, transferAttempts)

	r := e.run("push", dir, "acme/resume")
	require.Equal(t, ExitFailure, r.code)
	require.Contains(t, r.err.Error(), "run the command again to resume")
	require.Contains(t, r.err.Error(), "part 3")
	require.Len(t, *waits, transferAttempts-1, "part 3 was sent %d times", transferAttempts)
	putsAfterFailure := e.inst.PartPuts()
	require.Equal(t, 3, putsAfterFailure)

	e.ok("push", dir, "acme/resume", "--branch", "main")
	require.Equal(t, 1, e.inst.PartPuts()-putsAfterFailure, "only part 3, which was not stored, is sent again")
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

// TestPushToANewBranch follows docs/guides/uploading.md: `hangar push
// --branch march` on a Repo that has no `march` branch yet creates it from
// the default branch, as `hf upload --revision` does, and commits there.
// The Commit endpoint answers 404 for a branch that does not exist, so
// without the branch step the command uploaded everything and then failed.
func TestPushToANewBranch(t *testing.T) {
	e := newEnv(t)
	e.login()
	id := mustID(t, model.RepoTypeDataset, "acme/reviews")
	e.inst.AddRepo(id, clitest.File{Path: "README.md", Content: []byte("# Reviews\n")})
	dir := filepath.Join(e.home, "data")
	writeTree(t, dir, map[string][]byte{"data/train.csv": []byte("text,label\ngood,1\n")})

	r := e.ok("push", dir, "datasets/acme/reviews", "--branch", "march")
	require.Contains(t, r.stderr, "Created branch march")
	march := e.inst.Files(id, "march")
	require.Equal(t, []byte("text,label\ngood,1\n"), march["data/train.csv"])
	require.Equal(t, []byte("# Reviews\n"), march["README.md"], "the branch starts from the default branch")
	require.NotContains(t, e.inst.Files(id, "main"), "data/train.csv")

	// The branch exists now: the next push commits to it without creating it.
	writeTree(t, dir, map[string][]byte{"data/test.csv": []byte("text,label\nbad,0\n")})
	r = e.ok("push", dir, "datasets/acme/reviews", "--branch", "march")
	require.NotContains(t, r.stderr, "Created branch")
	require.Contains(t, e.inst.Files(id, "march"), "data/test.csv")
	require.Equal(t, 2, e.inst.Requests["POST /api/datasets/{ns}/{name}/branch/{branch}"])
}
