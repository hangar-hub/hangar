//go:build compat

package gitcompat

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

func TestCloneFetchPull(t *testing.T) {
	forEachBackend(t, func(t *testing.T, _ string, f BackendFactory) {
		s := newSuite(t, f, suiteOptions{})
		want := s.resolve(modelID, "main")
		for _, proto := range []string{"0", "2"} {
			dir := s.clone(modelID, "", "-c", "protocol.version="+proto)
			require.Equal(t, want, s.must(dir, "rev-parse", "HEAD"))
			s.must(dir, "fsck", "--strict")
			shallow := s.clone(modelID, "", "-c", "protocol.version="+proto, "--depth", "1")
			require.Equal(t, "true", s.must(shallow, "rev-parse", "--is-shallow-repository"))
		}
		partial := s.clone(modelID, "", "--filter=blob:none")
		require.Equal(t, "true", s.must(partial, "config", "remote.origin.promisor"))
		require.Contains(t, s.must(partial, "show", "HEAD:README.md"), "tiny-model")

		ds := s.clone(datasetID, "")
		require.Equal(t, s.resolve(datasetID, "main"), s.must(ds, "rev-parse", "HEAD"))

		// fetch and pull see new Commits from either replica.
		a := s.clone(modelID, tokOwner)
		head := s.commit(a, "config.json", `{"hidden_size":8}`, "add config")
		s.must(a, "push", "-q", s.replicas[1].RemoteURL(modelID, tokOwner, false), "main")
		b := s.clone(modelID, "")
		require.Equal(t, head, s.must(b, "rev-parse", "HEAD"))
		s.commit(a, "config.json", `{"hidden_size":16}`, "bigger")
		s.must(a, "push", "-q", "origin", "main")
		s.must(b, "fetch", "-q")
		s.must(b, "pull", "-q", "--ff-only")
		require.Equal(t, s.must(a, "rev-parse", "HEAD"), s.must(b, "rev-parse", "HEAD"))
	})
}

func TestBranchesTagsAndForcePushRules(t *testing.T) {
	forEachBackend(t, func(t *testing.T, _ string, f BackendFactory) {
		s := newSuite(t, f, suiteOptions{})
		dir := s.clone(modelID, tokWriter)
		s.must(dir, "checkout", "-q", "-b", "feature")
		sha := s.commit(dir, "f.txt", "feature\n", "feature")
		s.must(dir, "push", "-q", "origin", "feature")
		require.Equal(t, sha, s.resolve(modelID, "feature"))
		s.must(dir, "tag", "v1.0")
		s.must(dir, "tag", "-a", "v2.0", "-m", "release")
		s.must(dir, "push", "-q", "origin", "--tags")
		require.Equal(t, sha, s.resolve(modelID, "v1.0"))
		require.Equal(t, sha, s.resolve(modelID, "v2.0"))
		out := s.must("", "ls-remote", s.remote(modelID, ""))
		require.Contains(t, out, "refs/tags/v2.0^{}")

		// Force-pushing a non-default branch is allowed.
		s.must(dir, "commit", "-q", "--amend", "-m", "rewritten")
		s.must(dir, "push", "-q", "--force", "origin", "feature")
		// Deleting a branch and a tag.
		s.must(dir, "push", "-q", "origin", "--delete", "feature", "v1.0")
		_, err := s.repos.Resolve(context.Background(), modelID, "feature")
		require.ErrorIs(t, err, repo.ErrRevisionNotFound)

		// The default branch cannot be deleted, and only Namespace admins
		// can force-push it.
		s.must(dir, "checkout", "-q", "main")
		out, err = s.run(dir, "push", "origin", "--delete", "main")
		require.Error(t, err)
		require.Contains(t, out, "default branch")
		main := s.resolve(modelID, "main")
		s.must(dir, "commit", "-q", "--amend", "-m", "rewrite main")
		out, err = s.run(dir, "push", "--force", "origin", "main")
		require.Error(t, err)
		require.Contains(t, out, "force")
		require.Equal(t, main, s.resolve(modelID, "main"))
		admin := s.clone(modelID, tokOwner)
		s.must(admin, "commit", "-q", "--amend", "-m", "admin rewrite")
		s.must(admin, "push", "-q", "--force", "origin", "main")
		require.Equal(t, s.must(admin, "rev-parse", "HEAD"), s.resolve(modelID, "main"))

		// Pull request refs are not writable with git push.
		out, err = s.run(admin, "push", "origin", "HEAD:refs/pr/1")
		require.Error(t, err)
		require.Contains(t, out, "pull request")
	})
}

func TestPushRejectedOnMirror(t *testing.T) {
	forEachBackend(t, func(t *testing.T, _ string, f BackendFactory) {
		s := newSuite(t, f, suiteOptions{})
		dir := filepath.Join(t.TempDir(), "m")
		s.must("", "init", "-q", "-b", "main", dir)
		s.commit(dir, "a.txt", "a", "a")
		out, err := s.run(dir, "push", s.remote(mirrorID, tokOwner), "main")
		require.Error(t, err)
		require.Contains(t, out, "Mirror")
	})
}

func TestConcurrentPushesThroughTwoReplicas(t *testing.T) {
	forEachBackend(t, func(t *testing.T, _ string, f BackendFactory) {
		s := newSuite(t, f, suiteOptions{})
		a := s.clone(modelID, tokOwner)
		b := s.clone(modelID, tokOwner)
		aSHA := s.commit(a, "a.txt", "from a\n", "a")
		s.commit(b, "b.txt", "from b\n", "b")
		// B's pre-push hook runs after B has read the ref advertisement from
		// replica 2; it pushes A through replica 1, so B's push races A's.
		hook := fmt.Sprintf("#!/bin/sh\nenv -u GIT_DIR -u GIT_WORK_TREE -u GIT_INDEX_FILE git -C %q push -q %q main >/dev/null 2>&1\nexit 0\n",
			a, s.replicas[0].RemoteURL(modelID, tokOwner, true))
		require.NoError(t, os.WriteFile(filepath.Join(b, ".git", "hooks", "pre-push"), []byte(hook), 0o755))
		out, err := s.run(b, "push", s.replicas[1].RemoteURL(modelID, tokOwner, true), "main")
		require.Error(t, err, out)
		require.Contains(t, out, "reference has changed concurrently")
		require.Equal(t, aSHA, s.resolve(modelID, "main"), "the first push won and nothing else moved")
		// B recovers by fetching and rebasing.
		require.NoError(t, os.Remove(filepath.Join(b, ".git", "hooks", "pre-push")))
		s.must(b, "pull", "-q", "--rebase", "origin", "main")
		s.must(b, "push", "-q", s.replicas[1].RemoteURL(modelID, tokOwner, true), "main")
	})
}

func TestHTTPAPICommitsAndGitAgree(t *testing.T) {
	forEachBackend(t, func(t *testing.T, _ string, f BackendFactory) {
		s := newSuite(t, f, suiteOptions{})
		ctx := context.Background()
		res, err := s.repos.Commit(ctx, repo.CommitParams{Repo: modelID, Revision: "main", Summary: "upload through the API", Actor: s.owner,
			Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "tokenizer.json", Content: strings.NewReader(`{"v":1}`)}}})
		require.NoError(t, err)
		dir := s.clone(modelID, tokOwner)
		require.Equal(t, string(res.SHA), s.must(dir, "rev-parse", "HEAD"))
		require.Equal(t, `{"v":1}`, s.must(dir, "show", "HEAD:tokenizer.json"))
		s.must(dir, "fsck", "--strict")

		pushed := s.commit(dir, "vocab.txt", "a\nb\n", "add vocab")
		s.must(dir, "push", "-q", "origin", "main")
		require.Equal(t, pushed, s.resolve(modelID, "main"))
		log, err := s.repos.Log(ctx, modelID, "main", repo.LogOpts{Limit: 2})
		require.NoError(t, err)
		require.Equal(t, model.CommitSHA(pushed), log[0].SHA)
		require.Equal(t, res.SHA, log[1].SHA)
		rc, _, err := s.repos.OpenFile(ctx, modelID, model.CommitSHA(pushed), "vocab.txt", nil)
		require.NoError(t, err)
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		require.Equal(t, "a\nb\n", string(got))
	})
}

func TestAuth(t *testing.T) {
	forEachBackend(t, func(t *testing.T, _ string, f BackendFactory) {
		s := newSuite(t, f, suiteOptions{})
		dir := s.clone(modelID, "")
		s.commit(dir, "x.txt", "x", "x")

		out, err := s.run(dir, "push", s.remote(modelID, ""), "main")
		require.Error(t, err)
		require.Contains(t, strings.ToLower(out), "could not read username", "anonymous pushes get a credential prompt")
		out, err = s.run(dir, "push", s.remote(modelID, "hgr_wrong"), "main")
		require.Error(t, err)
		require.Contains(t, out, "Authentication failed")
		out, err = s.run(dir, "push", s.remote(modelID, tokReader), "main")
		require.Error(t, err)
		require.Contains(t, out, "403")
		s.must(dir, "push", "-q", s.remote(modelID, tokOwner), "main")

		// Private Repos: 404, not 403, for users without access.
		out, err = s.run("", "ls-remote", s.remote(privateID, tokOther))
		require.Error(t, err)
		require.Contains(t, out, "not found")
		resp, err := http.Get(s.replicas[0].URL + "/owner/private-model.git/info/refs?service=git-upload-pack")
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		s.clone(privateID, tokOwner)
		s.clone(privateID, tokReader)
	})
}

func randomFile(t *testing.T, n int) ([]byte, string) {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:])
}

// lfsRepo clones a Repo and tracks *.bin with Git LFS.
func (s *suite) lfsRepo(id model.RepoID) string {
	s.t.Helper()
	dir := s.clone(id, tokOwner)
	s.must(dir, "lfs", "track", "*.bin")
	return dir
}

func TestLFSPushCloneAndPull(t *testing.T) {
	for _, s3 := range []bool{false, true} {
		t.Run(map[bool]string{false: "hangar-urls", true: "presigned"}[s3], func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, _ string, f BackendFactory) {
				s := newSuite(t, f, suiteOptions{s3: s3})
				dir := s.lfsRepo(modelID)
				small, smallOID := randomFile(t, 4096)
				large, largeOID := randomFile(t, 3<<20) // over the 1 MiB multipart threshold and max blob size
				s.write(dir, "weights/small.bin", small)
				s.write(dir, "weights/large.bin", large)
				s.must(dir, "add", "-A")
				s.must(dir, "commit", "-q", "-m", "weights")
				out := s.must(dir, "push", "origin", "main")
				require.NotContains(t, out, "Locking support detected")
				require.NotContains(t, out, "locking API")
				require.Equal(t, s.must(dir, "rev-parse", "HEAD"), s.resolve(modelID, "main"))
				if s3 {
					require.Positive(t, s.blobs.Calls("PresignPut"), "uploads went straight to the store")
				}
				for oid, content := range map[string][]byte{smallOID: small, largeOID: large} {
					rc, obj, err := s.lfs.Open(context.Background(), oid, nil)
					require.NoError(t, err)
					got, _ := io.ReadAll(rc)
					_ = rc.Close()
					require.True(t, obj.Verified)
					require.True(t, bytes.Equal(content, got))
				}

				// A full clone smudges LFS files from either replica.
				for i := range s.replicas {
					c := filepath.Join(t.TempDir(), "c")
					s.must("", "clone", "-q", s.replicas[i].RemoteURL(modelID, "", true), c)
					got, err := os.ReadFile(filepath.Join(c, "weights", "large.bin"))
					require.NoError(t, err)
					require.True(t, bytes.Equal(large, got))
				}
				if s3 {
					require.Positive(t, s.blobs.Calls("PresignGet"), "downloads redirected to the store")
				}

				// GIT_LFS_SKIP_SMUDGE=1, then git lfs pull --include.
				c := s.cloneEnv(modelID, "", []string{"GIT_LFS_SKIP_SMUDGE=1"})
				ptr, err := os.ReadFile(filepath.Join(c, "weights", "small.bin"))
				require.NoError(t, err)
				require.Contains(t, string(ptr), "oid sha256:"+smallOID)
				s.must(c, "lfs", "pull", "--include", "weights/small.bin")
				got, err := os.ReadFile(filepath.Join(c, "weights", "small.bin"))
				require.NoError(t, err)
				require.True(t, bytes.Equal(small, got))
				ptr, err = os.ReadFile(filepath.Join(c, "weights", "large.bin"))
				require.NoError(t, err)
				require.Contains(t, string(ptr), "oid sha256:"+largeOID, "files outside --include stay pointers")

				// A second Repo deduplicates: git lfs push uploads nothing.
				d := s.lfsRepo(datasetID)
				s.write(d, "copy.bin", large)
				s.must(d, "add", "-A")
				s.must(d, "commit", "-q", "-m", "copy")
				s.must(d, "push", "-q", "origin", "main")
			})
		})
	}
}

func TestLFSMultipartTransfer(t *testing.T) {
	for _, s3 := range []bool{false, true} {
		t.Run(map[bool]string{false: "hangar-urls", true: "presigned"}[s3], func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, _ string, f BackendFactory) {
				s := newSuite(t, f, suiteOptions{s3: s3})
				dir := s.lfsRepo(modelID)
				// hf lfs-enable-largefiles sets up the multipart custom transfer.
				s.must(dir, "config", "lfs.customtransfer.multipart.path", hfBinary(t))
				s.must(dir, "config", "lfs.customtransfer.multipart.args", "lfs-multipart-upload")
				large, oid := randomFile(t, (3<<20)+12345) // 7 parts of 512 KiB
				s.write(dir, "model.bin", large)
				s.must(dir, "add", "-A")
				s.must(dir, "commit", "-q", "-m", "large")
				out, err := s.git.RunEnv(context.Background(), dir, []string{"GIT_TRACE=1"}, "push", "origin", "main")
				require.NoError(t, err, out)
				require.Contains(t, out, "multipart", "git-lfs used the multipart transfer")
				require.Positive(t, s.blobs.Calls("CreateMultipart"), "the object was uploaded in parts")
				if s3 {
					require.Positive(t, s.blobs.Calls("PresignPart"), "parts went straight to the store")
				}
				obj, err := s.lfs.Stat(context.Background(), oid)
				require.NoError(t, err)
				require.True(t, obj.Verified)
				require.Equal(t, int64(len(large)), obj.Size)

				c := s.clone(modelID, "")
				got, err := os.ReadFile(filepath.Join(c, "model.bin"))
				require.NoError(t, err)
				require.True(t, bytes.Equal(large, got))
			})
		})
	}
}

func TestLFSPushWithoutObjectsRefused(t *testing.T) {
	forEachBackend(t, func(t *testing.T, _ string, f BackendFactory) {
		s := newSuite(t, f, suiteOptions{})
		dir := s.clone(modelID, tokOwner)
		_, oid := randomFile(t, 100)
		s.write(dir, "w.bin", model.LFSPointer{OID: oid, Size: 100}.Bytes())
		s.must(dir, "add", "-A")
		s.must(dir, "commit", "-q", "-m", "pointer only")
		out, err := s.run(dir, "push", "origin", "main")
		require.Error(t, err)
		require.Contains(t, out, oid)

		// A large file committed without LFS is refused with advice.
		big, _ := randomFile(t, 2<<20)
		s.must(dir, "reset", "-q", "--hard", "HEAD~1")
		s.write(dir, "big.dat", big)
		s.must(dir, "add", "-A")
		s.must(dir, "commit", "-q", "-m", "big")
		out, err = s.run(dir, "push", "origin", "main")
		require.Error(t, err)
		require.Contains(t, strings.ToLower(out), "lfs")
	})
}

// TestLFSObjectsOfOtherReposNeedTheirBytes covers the decision James made
// on 2026-10-18 ("check at Commit") with git and git-lfs: other knows the
// SHA-256 and size of an object in owner's private Repo. A push of a
// pointer to it is refused, and the batch API asks other to upload it as
// if it did not exist. git-lfs then uploads the real bytes and the push
// succeeds, with no second copy stored. writer, who can read the private
// Repo, pushes a pointer to it without uploading anything.
func TestLFSObjectsOfOtherReposNeedTheirBytes(t *testing.T) {
	forEachBackend(t, func(t *testing.T, _ string, f BackendFactory) {
		s := newSuite(t, f, suiteOptions{})
		ctx := context.Background()
		secret, oid := randomFile(t, 50_000)
		dir := s.lfsRepo(privateID)
		s.write(dir, "secret.bin", secret)
		s.must(dir, "add", "-A")
		s.must(dir, "commit", "-q", "-m", "secret")
		s.must(dir, "push", "-q", "origin", "main")
		otherID := model.RepoID{Type: model.RepoTypeModel, Namespace: "other", Name: "own"}
		_, err := s.repos.Create(ctx, repo.CreateParams{ID: otherID, Visibility: model.VisibilityPublic, Actor: s.other, InitialReadme: true})
		require.NoError(t, err)
		pointer := model.LFSPointer{OID: oid, Size: int64(len(secret))}.Bytes()

		// A pointer alone, past git-lfs's own pre-push check.
		d := s.clone(otherID, tokOther)
		s.write(d, "w.bin", pointer)
		s.must(d, "add", "-A")
		s.must(d, "commit", "-q", "-m", "pointer only")
		out, err := s.run(d, "push", "--no-verify", "origin", "main")
		require.Error(t, err, out)
		require.Contains(t, out, "which has not been uploaded")

		// The batch asks for the bytes, without the stored size.
		body := `{"operation":"upload","transfers":["basic"],"objects":[{"oid":"` + oid + `","size":1}]}`
		req, err := http.NewRequest(http.MethodPost, s.replicas[0].URL+"/other/own.git/info/lfs/objects/batch", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
		req.SetBasicAuth("other", tokOther)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))
		require.Contains(t, string(raw), `"upload"`)
		require.NotContains(t, string(raw), "50000")

		// git-lfs uploads the real bytes, which are checked, not stored again.
		s.must(d, "reset", "-q", "--hard", "HEAD~1")
		s.must(d, "lfs", "track", "*.bin")
		s.write(d, "w.bin", secret)
		s.must(d, "add", "-A")
		s.must(d, "commit", "-q", "-m", "the real bytes")
		s.must(d, "push", "-q", "origin", "main")
		stored, ok := s.blobs.Bytes(model.LFSObjectKey(oid))
		require.True(t, ok)
		require.True(t, bytes.Equal(secret, stored))
		for _, k := range s.blobs.Keys() {
			require.False(t, strings.HasPrefix(k, "tmp/"), "no staged copy is left: %s", k)
		}
		c := s.clone(otherID, "")
		got, err := os.ReadFile(filepath.Join(c, "w.bin"))
		require.NoError(t, err)
		require.True(t, bytes.Equal(secret, got))

		// writer can read the private Repo, so a pointer needs no upload.
		w := s.clone(datasetID, tokWriter)
		s.write(w, "copy.bin", pointer)
		s.must(w, "add", "-A")
		s.must(w, "commit", "-q", "-m", "pointer to a readable object")
		out, err = s.run(w, "push", "--no-verify", "origin", "main")
		require.NoError(t, err, out)
	})
}
