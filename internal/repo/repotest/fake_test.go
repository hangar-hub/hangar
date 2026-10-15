package repotest_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/repo/repotest"
)

var (
	alice   = model.Principal{Kind: model.PrincipalUser, UserID: uuid.New(), Username: "alice", DisplayName: "Alice", Email: "alice@example.com"}
	modelID = model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "tiny"}
)

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

func newRepo(t *testing.T) (*repotest.Fake, *repo.Repo) {
	t.Helper()
	f := repotest.NewFake(t)
	r, err := f.Create(context.Background(), repo.CreateParams{ID: modelID, Visibility: model.VisibilityPublic, Actor: alice, InitialReadme: true, Licence: "apache-2.0"})
	require.NoError(t, err)
	return f, r
}

func TestCreateMakesInitialCommitWithDefaultGitAttributes(t *testing.T) {
	ctx := context.Background()
	f, r := newRepo(t)
	require.Equal(t, modelID, r.ID)
	require.Equal(t, "main", r.DefaultBranch)

	sha, err := f.Resolve(ctx, modelID, "main")
	require.NoError(t, err)
	rc, e, err := f.OpenFile(ctx, modelID, sha, ".gitattributes", nil)
	require.NoError(t, err)
	got, _ := io.ReadAll(rc)
	require.Equal(t, repo.DefaultGitAttributes(model.RepoTypeModel), got)
	require.Equal(t, model.GitBlobID(got), e.OID, "tree oid is the git blob sha1")

	_, err = f.Create(ctx, repo.CreateParams{ID: modelID, Actor: alice})
	require.ErrorIs(t, err, repo.ErrExists)
	_, err = f.Create(ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "bad name"}})
	require.ErrorIs(t, err, repo.ErrInvalid)

	// The same name as a dataset is a different Repo.
	ds, err := f.Create(ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeDataset, Namespace: "alice", Name: "tiny"}, Actor: alice})
	require.NoError(t, err)
	dsSHA, err := f.Resolve(ctx, ds.ID, "main")
	require.NoError(t, err)
	rc, _, err = f.OpenFile(ctx, ds.ID, dsSHA, ".gitattributes", nil)
	require.NoError(t, err)
	got, _ = io.ReadAll(rc)
	require.Contains(t, string(got), "*.mp3 filter=lfs")

	mirror, err := f.Create(ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "jinaai", Name: "x"}, Mirror: true})
	require.NoError(t, err)
	require.True(t, mirror.Mirror)
	_, err = f.Resolve(ctx, mirror.ID, "main")
	require.ErrorIs(t, err, repo.ErrRevisionNotFound, "Mirrors start empty")
}

func TestCommitsAreRealGitCommits(t *testing.T) {
	ctx := context.Background()
	f, _ := newRepo(t)
	weights := bytes.Repeat([]byte("w"), 5000)
	obj, err := f.LFS.Put(ctx, weights)
	require.NoError(t, err)

	when := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	res, err := f.Commit(ctx, repo.CommitParams{
		Repo: modelID, Revision: "main", Summary: "Add weights", Description: "Body", Actor: alice, Time: when,
		Operations: []repo.Operation{
			{Kind: repo.OpAdd, Path: "config.json", Content: strings.NewReader(`{"model_type":"bert"}`)},
			{Kind: repo.OpAdd, Path: "sub/dir/notes.txt", Content: strings.NewReader("notes\n")},
			{Kind: repo.OpAddLFS, Path: "model.safetensors", OID: obj.OID, Size: obj.Size},
		},
	})
	require.NoError(t, err)
	require.False(t, res.NoOp)

	dir, err := f.Dir(modelID)
	require.NoError(t, err)
	gitCmd(t, dir, "fsck", "--strict")
	require.Equal(t, string(res.SHA), gitCmd(t, dir, "rev-parse", "refs/heads/main"))
	require.Equal(t, fmt.Sprintf("Alice <alice@example.com> %d +0000", when.Unix()), gitCmd(t, dir, "log", "-1", "--format=%an <%ae> %at +0000"))

	// The LFS pointer is byte-identical with git lfs pointer.
	ptr := gitCmd(t, dir, "cat-file", "blob", "main:model.safetensors")
	require.Equal(t, strings.TrimSpace(string(model.LFSPointer{OID: obj.OID, Size: obj.Size}.Bytes())), ptr)

	info, err := f.GetCommit(ctx, modelID, res.SHA)
	require.NoError(t, err)
	require.Equal(t, "Add weights", info.Title)
	require.Equal(t, "Add weights\n\nBody", info.Message)
	require.Len(t, info.Parents, 1)
	require.Equal(t, when, info.Author.When)

	// A Commit that changes nothing is a no-op.
	again, err := f.Commit(ctx, repo.CommitParams{Repo: modelID, Summary: "same", Actor: alice,
		Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "config.json", Content: strings.NewReader(`{"model_type":"bert"}`)}}})
	require.NoError(t, err)
	require.True(t, again.NoOp)
	require.Equal(t, res.SHA, again.SHA)
}

func TestTreeEntriesAndFiles(t *testing.T) {
	ctx := context.Background()
	f, _ := newRepo(t)
	weights := []byte("0123456789")
	obj, err := f.LFS.Put(ctx, weights)
	require.NoError(t, err)
	res, err := f.Commit(ctx, repo.CommitParams{Repo: modelID, Summary: "files", Actor: alice, Operations: []repo.Operation{
		{Kind: repo.OpAdd, Path: "onnx/model.txt", Content: strings.NewReader("x")},
		{Kind: repo.OpAddLFS, Path: "onnx/w.bin", OID: obj.OID, Size: obj.Size},
	}})
	require.NoError(t, err)

	root, err := f.Tree(ctx, modelID, res.SHA, "", repo.TreeOpts{})
	require.NoError(t, err)
	var paths []string
	for _, e := range root {
		paths = append(paths, e.Path)
	}
	require.Equal(t, []string{"onnx", ".gitattributes", "README.md"}, paths, "directories first")

	rec, err := f.Tree(ctx, modelID, res.SHA, "", repo.TreeOpts{Recursive: true, Expand: true})
	require.NoError(t, err)
	byPath := map[string]model.Entry{}
	for _, e := range rec {
		byPath[e.Path] = e
	}
	require.Contains(t, byPath, "onnx/w.bin")
	w := byPath["onnx/w.bin"]
	require.NotNil(t, w.LFS)
	require.Equal(t, obj.OID, w.LFS.OID)
	require.Equal(t, int64(10), w.Size, "LFS files report the content size")
	require.Equal(t, model.LFSPointer{OID: obj.OID, Size: 10}.GitOID(), w.OID, "oid is the pointer's git sha1")
	require.True(t, w.Present)
	require.Equal(t, res.SHA, w.LastCommit.SHA)
	require.Equal(t, "files", w.LastCommit.Title)
	require.Equal(t, model.EntryDirectory, byPath["onnx"].Type)

	sub, err := f.Tree(ctx, modelID, res.SHA, "onnx", repo.TreeOpts{})
	require.NoError(t, err)
	require.Len(t, sub, 2)
	page, err := f.Tree(ctx, modelID, res.SHA, "onnx", repo.TreeOpts{After: "onnx/model.txt", Limit: 1})
	require.NoError(t, err)
	require.Len(t, page, 1)
	require.Equal(t, "onnx/w.bin", page[0].Path)

	_, err = f.Tree(ctx, modelID, res.SHA, "onnx/w.bin", repo.TreeOpts{})
	require.ErrorIs(t, err, repo.ErrEntryNotFound, "tree of a file path is EntryNotFound")
	_, err = f.Tree(ctx, modelID, res.SHA, "missing", repo.TreeOpts{})
	require.ErrorIs(t, err, repo.ErrEntryNotFound)
	_, err = f.Entry(ctx, modelID, model.CommitSHA(strings.Repeat("a", 40)), "x")
	require.ErrorIs(t, err, repo.ErrRevisionNotFound)

	entries, err := f.Entries(ctx, modelID, res.SHA, []string{"onnx", "nope.txt", "README.md"}, false)
	require.NoError(t, err)
	require.Len(t, entries, 2, "missing paths are omitted")

	rc, e, err := f.OpenFile(ctx, modelID, res.SHA, "onnx/w.bin", &blob.Range{Start: 2, End: 4})
	require.NoError(t, err)
	got, _ := io.ReadAll(rc)
	require.Equal(t, "234", string(got))
	require.Equal(t, obj.OID, e.ETag())
	rc, _, err = f.OpenFile(ctx, modelID, res.SHA, "onnx/model.txt", &blob.Range{Start: 0, End: -1})
	require.NoError(t, err)
	got, _ = io.ReadAll(rc)
	require.Equal(t, "x", string(got))
	_, _, err = f.OpenFile(ctx, modelID, res.SHA, "onnx", nil)
	require.ErrorIs(t, err, repo.ErrEntryNotFound)
}

func TestCommitOperationsAndErrors(t *testing.T) {
	ctx := context.Background()
	f, _ := newRepo(t)
	base, err := f.Commit(ctx, repo.CommitParams{Repo: modelID, Summary: "add", Actor: alice, Operations: []repo.Operation{
		{Kind: repo.OpAdd, Path: "logs/a.txt", Content: strings.NewReader("a")},
		{Kind: repo.OpAdd, Path: "logs/b.txt", Content: strings.NewReader("b")},
		{Kind: repo.OpAdd, Path: "keep.txt", Content: strings.NewReader("k")},
	}})
	require.NoError(t, err)

	res, err := f.Commit(ctx, repo.CommitParams{Repo: modelID, Summary: "edit", Actor: alice, ParentCommit: base.SHA[:7], Operations: []repo.Operation{
		{Kind: repo.OpDeleteFolder, Path: "logs/"},
		{Kind: repo.OpCopy, Path: "copy.txt", SrcPath: "keep.txt"},
		{Kind: repo.OpDelete, Path: "keep.txt"},
	}})
	require.NoError(t, err)
	tree, err := f.Tree(ctx, modelID, res.SHA, "", repo.TreeOpts{Recursive: true})
	require.NoError(t, err)
	var paths []string
	for _, e := range tree {
		paths = append(paths, e.Path)
	}
	require.ElementsMatch(t, []string{".gitattributes", "README.md", "copy.txt"}, paths)

	_, err = f.Commit(ctx, repo.CommitParams{Repo: modelID, Summary: "x", Actor: alice, ParentCommit: base.SHA, Operations: []repo.Operation{
		{Kind: repo.OpAdd, Path: "y", Content: strings.NewReader("y")}}})
	require.ErrorIs(t, err, repo.ErrParentMismatch)

	_, err = f.Commit(ctx, repo.CommitParams{Repo: modelID, Summary: "x", Actor: alice, Operations: []repo.Operation{{Kind: repo.OpDelete, Path: "nope"}}})
	require.ErrorIs(t, err, repo.ErrEntryNotFound)
	require.Contains(t, err.Error(), "A file with this name doesn't exist")

	_, err = f.Commit(ctx, repo.CommitParams{Repo: modelID, Summary: "x", Actor: alice, Operations: []repo.Operation{{Kind: repo.OpAddLFS, Path: "w.bin", OID: strings.Repeat("b", 64), Size: 3}}})
	require.ErrorIs(t, err, repo.ErrLFSObjectMissing)

	_, err = f.Commit(ctx, repo.CommitParams{Repo: modelID, Revision: "nope", Summary: "x", Actor: alice, Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "y", Content: strings.NewReader("y")}}})
	require.ErrorIs(t, err, repo.ErrRevisionNotFound)

	_, err = f.Commit(ctx, repo.CommitParams{Repo: modelID, Summary: "x", Actor: alice, Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "../escape", Content: strings.NewReader("y")}}})
	require.ErrorIs(t, err, repo.ErrInvalid)

	// A pull request ref is created from a base Commit.
	pr, err := f.Commit(ctx, repo.CommitParams{Repo: modelID, Revision: "refs/pr/1", CreateFrom: res.SHA, Summary: "PR", Actor: alice,
		Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "pr.txt", Content: strings.NewReader("p")}}})
	require.NoError(t, err)
	require.Equal(t, "refs/pr/1", pr.Ref)
	got, err := f.Resolve(ctx, modelID, "refs/pr/1")
	require.NoError(t, err)
	require.Equal(t, pr.SHA, got)
}

func TestLogDiffAndFileDiff(t *testing.T) {
	ctx := context.Background()
	f, _ := newRepo(t)
	_, err := f.Commit(ctx, repo.CommitParams{Repo: modelID, Summary: "one", Actor: alice, Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "a.txt", Content: strings.NewReader("line 1\n")}}})
	require.NoError(t, err)
	two, err := f.Commit(ctx, repo.CommitParams{Repo: modelID, Summary: "two", Actor: alice, Operations: []repo.Operation{
		{Kind: repo.OpAdd, Path: "a.txt", Content: strings.NewReader("line 1\nline 2\n")},
		{Kind: repo.OpAdd, Path: "bin.dat", Content: bytes.NewReader([]byte{0, 1, 2})},
	}})
	require.NoError(t, err)

	log, err := f.Log(ctx, modelID, "main", repo.LogOpts{})
	require.NoError(t, err)
	require.Len(t, log, 3)
	require.Equal(t, "two", log[0].Title)
	require.Equal(t, "initial commit", log[2].Title)
	page, err := f.Log(ctx, modelID, "main", repo.LogOpts{Skip: 1, Limit: 1})
	require.NoError(t, err)
	require.Equal(t, "one", page[0].Title)
	byPath, err := f.Log(ctx, modelID, "main", repo.LogOpts{Path: "bin.dat"})
	require.NoError(t, err)
	require.Len(t, byPath, 1)

	changes, err := f.Diff(ctx, modelID, two.SHA)
	require.NoError(t, err)
	require.Len(t, changes, 2)
	status := map[string]repo.ChangeStatus{}
	for _, c := range changes {
		status[c.Path] = c.Status
		if c.Path == "bin.dat" {
			require.True(t, c.Binary)
			require.Empty(t, c.OldOID)
		}
	}
	require.Equal(t, map[string]repo.ChangeStatus{"a.txt": repo.ChangeModified, "bin.dat": repo.ChangeAdded}, status)

	fd, err := f.FileDiff(ctx, modelID, two.SHA, "a.txt")
	require.NoError(t, err)
	require.Contains(t, fd.Patch, "+line 2")
	fd, err = f.FileDiff(ctx, modelID, two.SHA, "bin.dat")
	require.NoError(t, err)
	require.Empty(t, fd.Patch, "no text diff for binary files")
	_, err = f.FileDiff(ctx, modelID, two.SHA, "README.md")
	require.ErrorIs(t, err, repo.ErrEntryNotFound)

	// The root Commit diffs against the empty tree.
	initial := log[2].SHA
	rootChanges, err := f.Diff(ctx, modelID, initial)
	require.NoError(t, err)
	require.Len(t, rootChanges, 2)
}

func TestRefsBranchesTagsAndCAS(t *testing.T) {
	ctx := context.Background()
	f, _ := newRepo(t)
	main, err := f.Resolve(ctx, modelID, "main")
	require.NoError(t, err)

	require.NoError(t, f.CreateBranch(ctx, modelID, "feature/x", "main", alice))
	require.ErrorIs(t, f.CreateBranch(ctx, modelID, "feature/x", "main", alice), repo.ErrRefExists)
	require.NoError(t, f.CreateTag(ctx, repo.TagParams{Repo: modelID, Tag: "v1.0", Revision: "main", Message: "Release", Actor: alice}))
	require.NoError(t, f.CreateTag(ctx, repo.TagParams{Repo: modelID, Tag: "light", Revision: string(main), Actor: alice}))
	require.ErrorIs(t, f.CreateTag(ctx, repo.TagParams{Repo: modelID, Tag: "v1.0", Revision: "main", Actor: alice}), repo.ErrRefExists)

	refs, err := f.Refs(ctx, modelID)
	require.NoError(t, err)
	require.Len(t, refs.Branches, 2)
	require.Len(t, refs.Tags, 2)
	for _, tag := range refs.Tags {
		require.Equal(t, main, tag.Target, "annotated tags are peeled to the Commit")
	}
	tagSHA, err := f.Resolve(ctx, modelID, "v1.0")
	require.NoError(t, err)
	require.Equal(t, main, tagSHA)

	// Compare-and-swap: a stale old value fails and changes nothing.
	stale := model.CommitSHA(strings.Repeat("1", 40))
	err = f.UpdateRefs(ctx, modelID, []repo.RefUpdate{
		{Name: "refs/heads/a", New: main},
		{Name: "refs/heads/main", Old: stale, New: main},
	})
	require.ErrorIs(t, err, repo.ErrRefConflict)
	_, err = f.Resolve(ctx, modelID, "a")
	require.ErrorIs(t, err, repo.ErrRevisionNotFound, "all or nothing")
	require.ErrorIs(t, f.UpdateRefs(ctx, modelID, []repo.RefUpdate{{Name: "refs/heads/feature/x", New: main}}), repo.ErrRefConflict, "create of an existing ref")
	require.ErrorIs(t, f.UpdateRefs(ctx, modelID, []repo.RefUpdate{{Name: "bad"}}), repo.ErrInvalid)

	require.ErrorIs(t, f.DeleteBranch(ctx, modelID, "main", alice), repo.ErrProtectedRef)
	require.NoError(t, f.DeleteBranch(ctx, modelID, "feature/x", alice))
	require.ErrorIs(t, f.DeleteBranch(ctx, modelID, "feature/x", alice), repo.ErrRevisionNotFound)
	require.NoError(t, f.DeleteTag(ctx, modelID, "v1.0", alice))
	require.ErrorIs(t, f.DeleteTag(ctx, modelID, "v1.0", alice), repo.ErrRevisionNotFound)

	_, err = f.Resolve(ctx, modelID, string(main))
	require.NoError(t, err)
	_, err = f.Resolve(ctx, modelID, strings.Repeat("c", 40))
	require.ErrorIs(t, err, repo.ErrRevisionNotFound)
	_, err = f.Resolve(ctx, modelID, "refs/pr/9")
	require.ErrorIs(t, err, repo.ErrRevisionNotFound)
}

func TestApplyPackAndOpenView(t *testing.T) {
	ctx := context.Background()
	f, _ := newRepo(t)
	view, err := f.OpenView(ctx, modelID)
	require.NoError(t, err)
	defer view.Close()

	// Clone the view, commit locally, and push the new objects as a pack.
	work := filepath.Join(t.TempDir(), "work")
	gitCmd(t, filepath.Dir(work), "clone", "-q", view.Dir, work)
	require.NoError(t, os.WriteFile(filepath.Join(work, "pushed.txt"), []byte("hi\n"), 0o600))
	gitCmd(t, work, "add", "pushed.txt")
	gitCmd(t, work, "-c", "user.name=Bob", "-c", "user.email=bob@example.com", "commit", "-q", "-m", "pushed")
	newSHA := model.CommitSHA(gitCmd(t, work, "rev-parse", "HEAD"))
	oldSHA, err := f.Resolve(ctx, modelID, "main")
	require.NoError(t, err)
	cmd := exec.Command("git", "pack-objects", "--stdout", "--thin", "--revs")
	cmd.Dir = work
	cmd.Stdin = strings.NewReader(string(newSHA) + "\n^" + string(oldSHA) + "\n")
	pack, err := cmd.Output()
	require.NoError(t, err)

	require.NoError(t, f.ApplyPack(ctx, repo.ApplyPackParams{Repo: modelID, Source: repo.SourcePush, Actor: alice, Pack: bytes.NewReader(pack),
		Updates: []repo.RefUpdate{{Name: "refs/heads/main", Old: oldSHA, New: newSHA}}}))
	got, err := f.Resolve(ctx, modelID, "main")
	require.NoError(t, err)
	require.Equal(t, newSHA, got)
	dir, _ := f.Dir(modelID)
	gitCmd(t, dir, "fsck", "--strict")

	// Policy: the default branch cannot be deleted or rewritten by a push.
	err = f.ApplyPack(ctx, repo.ApplyPackParams{Repo: modelID, Source: repo.SourcePush, Updates: []repo.RefUpdate{{Name: "refs/heads/main", Old: newSHA}}})
	var pe *repo.PolicyError
	require.ErrorAs(t, err, &pe)
	require.Contains(t, pe.Error(), "refs/heads/main")
	err = f.ApplyPack(ctx, repo.ApplyPackParams{Repo: modelID, Source: repo.SourcePush, Updates: []repo.RefUpdate{{Name: "refs/heads/main", Old: newSHA, New: oldSHA}}})
	require.ErrorAs(t, err, &pe, "non-fast-forward")
	require.NoError(t, f.ApplyPack(ctx, repo.ApplyPackParams{Repo: modelID, Source: repo.SourcePush, AllowForce: true, Updates: []repo.RefUpdate{{Name: "refs/heads/main", Old: newSHA, New: oldSHA}}}))
	err = f.ApplyPack(ctx, repo.ApplyPackParams{Repo: modelID, Source: repo.SourcePush, Updates: []repo.RefUpdate{{Name: "refs/pr/1", New: oldSHA}}})
	require.ErrorAs(t, err, &pe)
	err = f.ApplyPack(ctx, repo.ApplyPackParams{Repo: modelID, Source: repo.SourcePush, Pack: strings.NewReader("not a pack")})
	require.ErrorIs(t, err, repo.ErrInvalid)

	// Mirrors refuse pushes and direct commits, but accept mirror syncs.
	require.NoError(t, f.SetMirror(modelID, true))
	err = f.ApplyPack(ctx, repo.ApplyPackParams{Repo: modelID, Source: repo.SourcePush})
	require.ErrorIs(t, err, repo.ErrReadOnly)
	_, err = f.Commit(ctx, repo.CommitParams{Repo: modelID, Summary: "x", Operations: []repo.Operation{{Kind: repo.OpAdd, Path: "y", Content: strings.NewReader("y")}}})
	require.ErrorIs(t, err, repo.ErrReadOnly)
	require.ErrorIs(t, f.CreateBranch(ctx, modelID, "b", "main", alice), repo.ErrReadOnly)
	require.NoError(t, f.ApplyPack(ctx, repo.ApplyPackParams{Repo: modelID, Source: repo.SourceMirror, Updates: []repo.RefUpdate{{Name: "refs/convert/parquet", New: oldSHA}}}))
}

func TestPartialMirrorPresence(t *testing.T) {
	ctx := context.Background()
	f := repotest.NewFake(t)
	id := model.RepoID{Type: model.RepoTypeModel, Namespace: "bartowski", Name: "gguf"}
	_, err := f.Create(ctx, repo.CreateParams{ID: id, Mirror: true})
	require.NoError(t, err)
	missing := strings.Repeat("d", 64)
	res, err := f.Commit(ctx, repo.CommitParams{Repo: id, Summary: "mirror", AllowMirror: true, Operations: []repo.Operation{
		{Kind: repo.OpAddLFS, Path: "q4.gguf", OID: missing, Size: 1 << 30},
	}})
	require.NoError(t, err)
	e, err := f.Entry(ctx, id, res.SHA, "q4.gguf")
	require.NoError(t, err)
	require.False(t, e.Present)
	_, _, err = f.OpenFile(ctx, id, res.SHA, "q4.gguf", nil)
	require.ErrorIs(t, err, repo.ErrNotPresent)

	content := []byte("gguf bytes")
	sum := sha256.Sum256(content)
	oid := hex.EncodeToString(sum[:])
	require.NoError(t, f.SetLFSPresence(ctx, id, []repo.LFSRef{{OID: oid, Size: int64(len(content)), Present: false}}))
	refs, err := f.LFSRefs(ctx, id, []string{missing, oid, strings.Repeat("e", 64)})
	require.NoError(t, err)
	require.Len(t, refs, 2, "objects the Repo does not reference are not listed")
	require.False(t, refs[missing].Present)
	require.Equal(t, int64(1<<30), refs[missing].Size)
	require.ErrorIs(t, f.SetLFSPresence(ctx, id, []repo.LFSRef{{OID: "bad"}}), repo.ErrInvalid)
}

func TestRenameTransferDeleteRestorePurge(t *testing.T) {
	ctx := context.Background()
	f, r := newRepo(t)
	renamed, err := f.Rename(ctx, modelID, "tiny-v2", alice)
	require.NoError(t, err)
	require.Equal(t, "tiny-v2", renamed.ID.Name)
	got, err := f.Get(ctx, modelID)
	require.NoError(t, err, "the old ID redirects")
	require.Equal(t, renamed.ID, got.ID)

	moved, err := f.Transfer(ctx, renamed.ID, "ml-team", alice)
	require.NoError(t, err)
	require.Equal(t, "ml-team", moved.ID.Namespace)
	got, err = f.Get(ctx, modelID)
	require.NoError(t, err)
	require.Equal(t, moved.ID, got.ID, "redirects follow every move")

	other := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "other"}
	_, err = f.Create(ctx, repo.CreateParams{ID: other, Actor: alice})
	require.NoError(t, err)
	_, err = f.Rename(ctx, other, "tiny-v2", alice)
	require.NoError(t, err, "the name is free in alice's Namespace after the transfer")
	_, err = f.Transfer(ctx, moved.ID, "alice", alice)
	require.ErrorIs(t, err, repo.ErrExists, "alice/tiny-v2 is now taken")

	require.NoError(t, f.Delete(ctx, moved.ID, alice))
	_, err = f.Get(ctx, moved.ID)
	require.ErrorIs(t, err, repo.ErrNotFound)
	byUUID, err := f.GetByUUID(ctx, r.UUID)
	require.NoError(t, err)
	require.NotNil(t, byUUID.DeletedAt)
	restored, err := f.Restore(ctx, moved.ID, alice)
	require.NoError(t, err)
	require.Nil(t, restored.DeletedAt)

	require.ErrorIs(t, f.Purge(ctx, r.UUID), repo.ErrInvalid, "delete first")
	require.NoError(t, f.Delete(ctx, moved.ID, alice))
	_, err = f.Create(ctx, repo.CreateParams{ID: moved.ID, Actor: alice})
	require.NoError(t, err)
	_, err = f.Restore(ctx, moved.ID, alice)
	require.ErrorIs(t, err, repo.ErrExists, "the name was reused")
	require.NoError(t, f.Purge(ctx, r.UUID))
	_, err = f.GetByUUID(ctx, r.UUID)
	require.ErrorIs(t, err, repo.ErrNotFound)
	_, err = f.Restore(ctx, model.RepoID{Type: model.RepoTypeModel, Namespace: "x", Name: "y"}, alice)
	require.ErrorIs(t, err, repo.ErrNotFound)
}

func TestSettingsAndList(t *testing.T) {
	ctx := context.Background()
	f, _ := newRepo(t)
	priv := model.VisibilityPrivate
	desc := "A tiny model"
	tags := []string{"bert", "test"}
	gating := repo.Gating{Mode: "manual"}
	require.NoError(t, f.CreateBranch(ctx, modelID, "dev", "main", alice))
	dev := "dev"
	updated, err := f.UpdateSettings(ctx, modelID, repo.SettingsUpdate{Visibility: &priv, Description: &desc, Tags: &tags, Gating: &gating, DefaultBranch: &dev}, alice)
	require.NoError(t, err)
	require.Equal(t, priv, updated.Visibility)
	require.True(t, updated.Gating.Enabled())
	require.Equal(t, "refs/heads/dev", updated.HeadRef)
	bad := "nope"
	_, err = f.UpdateSettings(ctx, modelID, repo.SettingsUpdate{DefaultBranch: &bad}, alice)
	require.ErrorIs(t, err, repo.ErrRevisionNotFound)
	badGate := repo.Gating{Mode: "sometimes"}
	_, err = f.UpdateSettings(ctx, modelID, repo.SettingsUpdate{Gating: &badGate}, alice)
	require.ErrorIs(t, err, repo.ErrInvalid)

	for _, name := range []string{"b-public", "a-public"} {
		_, err := f.Create(ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "bob", Name: name}})
		require.NoError(t, err)
	}
	res, err := f.List(ctx, repo.ListOptions{Viewer: model.Anonymous()})
	require.NoError(t, err)
	require.Len(t, res.Repos, 2, "anonymous users cannot see alice's private Repo")
	require.Equal(t, "a-public", res.Repos[0].ID.Name)
	res, err = f.List(ctx, repo.ListOptions{Viewer: alice, Search: "TINY"})
	require.NoError(t, err)
	require.Len(t, res.Repos, 1)
	res, err = f.List(ctx, repo.ListOptions{Viewer: alice, Tags: []string{"bert"}})
	require.NoError(t, err)
	require.Len(t, res.Repos, 1)
	res, err = f.List(ctx, repo.ListOptions{Viewer: alice, Limit: 2, Sort: "created", Desc: true})
	require.NoError(t, err)
	require.Len(t, res.Repos, 2)
	require.Equal(t, "2", res.NextCursor)
	res, err = f.List(ctx, repo.ListOptions{Viewer: alice, Cursor: res.NextCursor})
	require.NoError(t, err)
	require.Len(t, res.Repos, 1)
	notMirror := false
	res, err = f.List(ctx, repo.ListOptions{Viewer: alice, Namespaces: []string{"BOB"}, Mirror: &notMirror, Type: model.RepoTypeModel})
	require.NoError(t, err)
	require.Len(t, res.Repos, 2)
	_, err = f.List(ctx, repo.ListOptions{Formats: []string{"gguf"}})
	require.Error(t, err, "the fake refuses filters it cannot apply")
	_, err = f.List(ctx, repo.ListOptions{Sort: "downloads"})
	require.Error(t, err)
	_, err = f.List(ctx, repo.ListOptions{Cursor: "x"})
	require.ErrorIs(t, err, repo.ErrInvalid)
}

func TestFakeLFSUploads(t *testing.T) {
	ctx := context.Background()
	f := repotest.NewFake(t)
	content := bytes.Repeat([]byte("abcdefgh"), 3) // 24 bytes
	sum := sha256.Sum256(content)
	oid := hex.EncodeToString(sum[:])

	_, err := f.LFS.Ingest(ctx, oid, int64(len(content)), bytes.NewReader([]byte("wrong")))
	require.ErrorIs(t, err, repo.ErrChecksumMismatch)
	_, err = f.LFS.Ingest(ctx, oid, int64(len(content)), bytes.NewReader(bytes.Repeat([]byte("x"), 24)))
	require.ErrorIs(t, err, repo.ErrChecksumMismatch)
	_, err = f.LFS.Stat(ctx, oid)
	require.ErrorIs(t, err, repo.ErrNotFound, "a corrupt upload is never visible")

	up, err := f.LFS.CreateUpload(ctx, repo.UploadParams{OID: oid, Size: 24, Multipart: true, PartSize: 10})
	require.NoError(t, err)
	require.Equal(t, 3, up.PartCount)
	_, err = f.LFS.PresignPart(ctx, up.ID, 1, time.Hour)
	require.ErrorIs(t, err, blob.ErrNotSupported, "the default blob fake behaves like the filesystem backend")
	var parts []blob.Part
	for i, chunk := range [][]byte{content[:10], content[10:20], content[20:]} {
		etag, err := f.LFS.PutPart(ctx, up.ID, i+1, bytes.NewReader(chunk), int64(len(chunk)))
		require.NoError(t, err)
		parts = append(parts, blob.Part{Number: i + 1, ETag: etag})
	}
	_, err = f.LFS.PutPart(ctx, up.ID, 4, strings.NewReader("x"), 1)
	require.ErrorIs(t, err, repo.ErrInvalid)
	state, err := f.LFS.GetUpload(ctx, up.ID)
	require.NoError(t, err)
	require.Len(t, state.Done, 3)
	obj, err := f.LFS.CompleteUpload(ctx, up.ID, parts)
	require.NoError(t, err)
	require.True(t, obj.Verified)
	rc, _, err := f.LFS.Open(ctx, oid, nil)
	require.NoError(t, err)
	got, _ := io.ReadAll(rc)
	require.Equal(t, content, got)
	many, err := f.LFS.StatMany(ctx, []string{oid, strings.Repeat("e", 64)})
	require.NoError(t, err)
	require.Len(t, many, 1)
	_, err = f.LFS.GetUpload(ctx, up.ID)
	require.ErrorIs(t, err, repo.ErrNotFound, "completed uploads are gone")

	single, err := f.LFS.CreateUpload(ctx, repo.UploadParams{OID: oid, Size: 24})
	require.NoError(t, err)
	_, err = f.LFS.PutPart(ctx, single.ID, 1, strings.NewReader("x"), 1)
	require.ErrorIs(t, err, repo.ErrInvalid)
	require.NoError(t, f.LFS.AbortUpload(ctx, single.ID))
	_, err = f.LFS.CreateUpload(ctx, repo.UploadParams{OID: "nope"})
	require.ErrorIs(t, err, repo.ErrInvalid)
	_, err = f.LFS.CreateUpload(ctx, repo.UploadParams{OID: oid, Size: 24, Multipart: true})
	require.ErrorIs(t, err, repo.ErrInvalid)

	_, err = f.LFS.PresignPut(ctx, oid, 24, time.Hour)
	require.ErrorIs(t, err, blob.ErrNotSupported)
	_, err = f.LFS.PresignGet(ctx, oid, time.Hour, "x")
	require.ErrorIs(t, err, blob.ErrNotSupported)
	obj, err = f.LFS.Verify(ctx, oid, 24)
	require.NoError(t, err)
	require.True(t, obj.Verified)
	_, err = f.LFS.Verify(ctx, oid, 25)
	require.ErrorIs(t, err, repo.ErrChecksumMismatch)
	_, err = f.LFS.Verify(ctx, strings.Repeat("f", 64), 1)
	require.ErrorIs(t, err, repo.ErrNotFound)
}

func TestAddsFollowGitAttributes(t *testing.T) {
	ctx := context.Background()
	f, _ := newRepo(t)
	weights := []byte("pickle bytes")
	sum := sha256.Sum256(weights)
	oid := hex.EncodeToString(sum[:])
	res, err := f.Commit(ctx, repo.CommitParams{Repo: modelID, Summary: "add", Actor: alice, Operations: []repo.Operation{
		{Kind: repo.OpAdd, Path: "pytorch_model.bin", Content: bytes.NewReader(weights)},
		{Kind: repo.OpAdd, Path: "config.json", Content: strings.NewReader("{}")},
	}})
	require.NoError(t, err)

	// *.bin is in the default .gitattributes, so the file is an LFS object
	// and git holds its pointer.
	e, err := f.Entry(ctx, modelID, res.SHA, "pytorch_model.bin")
	require.NoError(t, err)
	require.NotNil(t, e.LFS)
	require.Equal(t, oid, e.LFS.OID)
	require.True(t, e.Present)
	dir, err := f.Dir(modelID)
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(string(model.LFSPointer{OID: oid, Size: int64(len(weights))}.Bytes())),
		gitCmd(t, dir, "cat-file", "blob", string(res.SHA)+":pytorch_model.bin"))
	rc, _, err := f.OpenFile(ctx, modelID, res.SHA, "pytorch_model.bin", nil)
	require.NoError(t, err)
	got, _ := io.ReadAll(rc)
	rc.Close()
	require.Equal(t, weights, got)
	obj, err := f.LFS.Stat(ctx, oid)
	require.NoError(t, err)
	require.True(t, obj.Verified)
	c, err := f.Entry(ctx, modelID, res.SHA, "config.json")
	require.NoError(t, err)
	require.Nil(t, c.LFS)

	// A .gitattributes in the Repo replaces the defaults from the next
	// Commit on.
	_, err = f.Commit(ctx, repo.CommitParams{Repo: modelID, Summary: "attrs", Actor: alice, Operations: []repo.Operation{
		{Kind: repo.OpAdd, Path: ".gitattributes", Content: strings.NewReader("*.json filter=lfs diff=lfs merge=lfs -text\n")},
	}})
	require.NoError(t, err)
	res, err = f.Commit(ctx, repo.CommitParams{Repo: modelID, Summary: "more", Actor: alice, Operations: []repo.Operation{
		{Kind: repo.OpAdd, Path: "other.bin", Content: strings.NewReader("plain")},
		{Kind: repo.OpAdd, Path: "big.json", Content: strings.NewReader("{\"lfs\":true}")},
	}})
	require.NoError(t, err)
	e, err = f.Entry(ctx, modelID, res.SHA, "other.bin")
	require.NoError(t, err)
	require.Nil(t, e.LFS)
	e, err = f.Entry(ctx, modelID, res.SHA, "big.json")
	require.NoError(t, err)
	require.NotNil(t, e.LFS)
}

func TestMapNamespace(t *testing.T) {
	ctx := context.Background()
	f := repotest.NewFake(t)
	org, other := uuid.New(), uuid.New()
	f.MapNamespace("Acme", org)
	f.MapNamespace("other", other)
	actor := alice
	actor.NamespaceID = uuid.New()
	r, err := f.Create(ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "acme", Name: "m"}, Actor: actor})
	require.NoError(t, err)
	require.Equal(t, org, r.NamespaceID)
	r, err = f.Create(ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "m"}, Actor: actor})
	require.NoError(t, err)
	require.Equal(t, actor.NamespaceID, r.NamespaceID, "unmapped Namespaces use the actor's")
	r, err = f.Transfer(ctx, r.ID, "other", actor)
	require.NoError(t, err)
	require.Equal(t, other, r.NamespaceID)
}

func TestFakeLFSResumeUpload(t *testing.T) {
	ctx := context.Background()
	f := repotest.NewFake(t)
	oid := strings.Repeat("c", 64)
	user, r1, r2 := uuid.New(), uuid.New(), uuid.New()
	params := repo.UploadParams{OID: oid, Size: 24, Multipart: true, PartSize: 10, UserID: user, RepoUUID: r1, Resume: true}
	up, err := f.LFS.CreateUpload(ctx, params)
	require.NoError(t, err)
	again, err := f.LFS.CreateUpload(ctx, params)
	require.NoError(t, err)
	require.Equal(t, up.ID, again.ID, "the user's upload in this Repo continues")
	params.RepoUUID = r2
	other, err := f.LFS.CreateUpload(ctx, params)
	require.NoError(t, err)
	require.NotEqual(t, up.ID, other.ID, "another Repo gets its own upload")
}
