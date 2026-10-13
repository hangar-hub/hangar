package model_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/model"
)

func TestParseRepoType(t *testing.T) {
	for in, want := range map[string]model.RepoType{
		"": model.RepoTypeModel, "model": model.RepoTypeModel, "models": model.RepoTypeModel,
		"dataset": model.RepoTypeDataset, "Datasets": model.RepoTypeDataset,
	} {
		got, err := model.ParseRepoType(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	_, err := model.ParseRepoType("space")
	require.ErrorIs(t, err, model.ErrInvalidRepoType)
	require.Equal(t, "datasets", model.RepoTypeDataset.Plural())
	require.Equal(t, "datasets/", model.RepoTypeDataset.URLPrefix())
	require.Empty(t, model.RepoTypeModel.URLPrefix())
}

func TestParseRepoID(t *testing.T) {
	id, err := model.ParseRepoID(model.RepoTypeDataset, "HuggingFaceFW/fineweb")
	require.NoError(t, err)
	require.Equal(t, "HuggingFaceFW/fineweb", id.String())
	require.Equal(t, "datasets/HuggingFaceFW/fineweb", id.Path())
	require.True(t, id.Equal(model.RepoID{Type: model.RepoTypeDataset, Namespace: "huggingfacefw", Name: "FineWeb"}))

	for _, bad := range []string{"gpt2", "a/b/c", "ns/", "/name", "ns/na me", "ns/a--b", "ns/a..b", "ns/.x", "ns/x.git", "n/x", "ns/" + strings.Repeat("a", 97)} {
		_, err := model.ParseRepoID(model.RepoTypeModel, bad)
		require.Error(t, err, bad)
	}
	for _, good := range []string{"jinaai/jina-embeddings-v3", "bartowski/Llama-3.2-3B-Instruct-GGUF", "hf-internal-testing/tiny-random-bert", "ab/x_y.z"} {
		_, err := model.ParseRepoID(model.RepoTypeModel, good)
		require.NoError(t, err, good)
	}
}

func TestNamespaceNames(t *testing.T) {
	require.NoError(t, model.ValidateNewNamespaceName("james"))
	require.Error(t, model.ValidateNewNamespaceName("api"))
	require.Error(t, model.ValidateNewNamespaceName("Datasets-Server"))
	require.Error(t, model.ValidateNewNamespaceName("a"))
	require.Error(t, model.ValidateNewNamespaceName(strings.Repeat("a", 43)))
	require.Error(t, model.ValidateNewNamespaceName("-x"))
	require.NoError(t, model.ValidateNamespaceName("api"), "syntax check alone allows reserved names")
	require.True(t, model.IsReservedNamespace(".hidden"))
	require.Contains(t, model.ReservedNamespaces(), "xet-cas")
}

func TestVisibilityRoleScope(t *testing.T) {
	v, err := model.ParseVisibility("Internal")
	require.NoError(t, err)
	require.True(t, v.Private())
	require.False(t, model.VisibilityPublic.Private())
	_, err = model.ParseVisibility("secret")
	require.Error(t, err)

	require.True(t, model.RoleAdmin.Includes(model.RoleWrite))
	require.True(t, model.RoleWrite.Includes(model.RoleRead))
	require.False(t, model.RoleRead.Includes(model.RoleWrite))
	require.False(t, model.Role("").Includes(model.Role("")))
	_, err = model.ParseRole("owner")
	require.Error(t, err)

	s, err := model.ParseScope("WRITE")
	require.NoError(t, err)
	require.True(t, s.Includes(model.ScopeRead))
	require.False(t, s.Includes(model.ScopeAdmin))
	_, err = model.ParseScope("fineGrained")
	require.Error(t, err)
}

func TestCommitSHAAndRefs(t *testing.T) {
	sha, err := model.ParseCommitSHA("607A30D783DFA663CAF39E06633721C8D4CFCD7E")
	require.NoError(t, err)
	require.Equal(t, model.CommitSHA("607a30d783dfa663caf39e06633721c8d4cfcd7e"), sha)
	require.Equal(t, "607a30d7", sha.Short())
	require.True(t, model.ZeroSHA.IsZero())
	require.True(t, model.CommitSHA("").IsZero())
	_, err = model.ParseCommitSHA("607a30d")
	require.ErrorIs(t, err, model.ErrInvalidSHA)
	require.True(t, model.IsFullSHA(string(sha)))

	require.True(t, model.IsSpecialRef("refs/pr/12"))
	require.True(t, model.IsSpecialRef("refs/convert/parquet"))
	require.False(t, model.IsSpecialRef("refs/heads/main"))
	require.Equal(t, "refs/pr/3", model.PRRef(3))
	n, ok := model.ParsePRRef("refs/pr/3")
	require.True(t, ok)
	require.Equal(t, 3, n)
	_, ok = model.ParsePRRef("refs/pr/x")
	require.False(t, ok)
	require.Equal(t, "refs/heads/main", model.BranchRef("main"))
	require.Equal(t, "refs/tags/v1", model.TagRef("v1"))
	require.Equal(t, "refs/pr/1", model.BranchRef("refs/pr/1"))
	require.Equal(t, "feature/x", model.ShortRefName("refs/heads/feature/x"))
	require.Equal(t, "v1", model.ShortRefName("refs/tags/v1"))

	for _, good := range []string{"refs/heads/main", "refs/heads/feature/x", "refs/tags/v1.0"} {
		require.NoError(t, model.ValidateRefName(good), good)
	}
	for _, bad := range []string{"main", "refs/heads/", "refs/heads/a..b", "refs/heads/x.lock", "refs/heads/a b", "refs/heads/.x", "refs/heads/a@{1}", "refs//x"} {
		require.Error(t, model.ValidateRefName(bad), bad)
	}
}

func TestGitBlobIDMatchesGit(t *testing.T) {
	content := []byte(`{"model_type": "bert"}` + "\n")
	dir := t.TempDir()
	f := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(f, content, 0o600))
	out, err := exec.Command("git", "hash-object", f).Output()
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(string(out)), model.GitBlobID(content))
	// The empty blob has a well-known ID.
	require.Equal(t, "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391", model.GitBlobID(nil))
}

func TestLFSPointerRoundTripAndGitLFS(t *testing.T) {
	content := []byte("hello hangar\n")
	sum := sha256.Sum256(content)
	p := model.LFSPointer{OID: hex.EncodeToString(sum[:]), Size: int64(len(content))}
	require.NoError(t, p.Validate())
	parsed, err := model.ParseLFSPointer(p.Bytes())
	require.NoError(t, err)
	require.Equal(t, p, parsed)
	require.Equal(t, model.GitBlobID(p.Bytes()), p.GitOID())
	require.Equal(t, len(p.Bytes()), p.PointerSize())

	if _, err := exec.LookPath("git-lfs"); err == nil {
		dir := t.TempDir()
		f := filepath.Join(dir, "weights.bin")
		require.NoError(t, os.WriteFile(f, content, 0o600))
		cmd := exec.Command("git", "lfs", "pointer", "--file="+f)
		cmd.Dir = dir
		out, err := cmd.Output()
		require.NoError(t, err)
		require.Equal(t, string(out), string(p.Bytes()), "pointer must be byte-identical with git lfs pointer")
	}
}

func TestParseLFSPointerRejects(t *testing.T) {
	oid := strings.Repeat("a", 64)
	valid := "version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\nsize 12\n"
	_, err := model.ParseLFSPointer([]byte(valid))
	require.NoError(t, err)
	withExt := "version https://git-lfs.github.com/spec/v1\next-0-foo sha256:" + oid + "\noid sha256:" + oid + "\nsize 12\n"
	_, err = model.ParseLFSPointer([]byte(withExt))
	require.NoError(t, err)

	for name, bad := range map[string]string{
		"empty":       "",
		"no newline":  strings.TrimSuffix(valid, "\n"),
		"bad version": strings.Replace(valid, "spec/v1", "spec/v2", 1),
		"unsorted":    "version https://git-lfs.github.com/spec/v1\nsize 12\noid sha256:" + oid + "\n",
		"upper oid":   strings.Replace(valid, oid, strings.ToUpper(oid), 1),
		"short oid":   strings.Replace(valid, oid, "abc", 1),
		"neg size":    strings.Replace(valid, "size 12", "size -1", 1),
		"plus size":   strings.Replace(valid, "size 12", "size +12", 1),
		"no size":     "version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\n",
		"unknown key": "version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\nsize 12\nzzz 1\n",
		"too long":    valid + strings.Repeat("x", model.MaxLFSPointerSize),
		"regular":     "{\"a\": 1}\n",
	} {
		_, err := model.ParseLFSPointer([]byte(bad))
		require.ErrorIs(t, err, model.ErrNotLFSPointer, name)
	}
	require.Equal(t, "lfs/sha256/aa/aa/"+oid, model.LFSObjectKey(oid))
}

func FuzzParseLFSPointer(f *testing.F) {
	oid := strings.Repeat("0", 64)
	f.Add([]byte("version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\nsize 1\n"))
	f.Add([]byte("version https://git-lfs.github.com/spec/v1\n"))
	f.Add([]byte(""))
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := model.ParseLFSPointer(data)
		if err != nil {
			return
		}
		// Anything that parses must validate and re-parse to the same pointer.
		require.NoError(t, p.Validate())
		again, err := model.ParseLFSPointer(p.Bytes())
		require.NoError(t, err)
		require.Equal(t, p, again)
	})
}

func TestEntry(t *testing.T) {
	e := model.Entry{Path: "a.bin", Type: model.EntryFile, OID: "abc", LFS: &model.LFSInfo{OID: "def"}}
	require.True(t, e.IsLFS())
	require.Equal(t, "def", e.ETag())
	e.LFS = nil
	require.Equal(t, "abc", e.ETag())
}

func TestCleanRepoPath(t *testing.T) {
	for in, want := range map[string]string{"": "", ".": "", "logs/": "logs", "a/b.txt": "a/b.txt"} {
		got, err := model.CleanRepoPath(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	for _, bad := range []string{"/etc/passwd", "../x", "a/../b", "a//b", ".git/config", "a/.git/x", "a\\b", "a\x00b", "./a"} {
		_, err := model.CleanRepoPath(bad)
		require.ErrorIs(t, err, model.ErrInvalidPath, bad)
	}
}

func TestPrincipal(t *testing.T) {
	anon := model.Anonymous()
	require.True(t, anon.IsAnonymous())
	require.Equal(t, model.ScopeRead, anon.MaxScope())
	require.Empty(t, anon.EndUserID())

	id := uuid.New()
	p := model.Principal{Kind: model.PrincipalUser, UserID: id, Username: "james"}
	require.True(t, p.IsUser())
	require.Equal(t, model.ScopeAdmin, p.MaxScope())
	require.Equal(t, id.String(), p.EndUserID())

	repo := model.RepoID{Type: model.RepoTypeModel, Namespace: "ml", Name: "x"}
	other := model.RepoID{Type: model.RepoTypeModel, Namespace: "other", Name: "y"}
	p.Token = &model.TokenInfo{Scope: model.ScopeRead, Restrictions: model.TokenRestrictions{Namespaces: []string{"ML"}}}
	require.Equal(t, model.ScopeRead, p.MaxScope())
	require.True(t, p.AllowsRepo(repo))
	require.False(t, p.AllowsRepo(other))
	p.Token.Restrictions = model.TokenRestrictions{Repos: []string{"model:other/y"}}
	require.True(t, p.AllowsRepo(other))
	require.False(t, p.AllowsRepo(repo))

	sys := model.System()
	require.Equal(t, model.SystemUsername, sys.Username)
	require.False(t, sys.IsAnonymous())
}

func TestHubTime(t *testing.T) {
	ts := time.Date(2024, 11, 16, 3, 16, 16, 0, time.FixedZone("x", 3600))
	b, err := json.Marshal(struct {
		T model.HubTime `json:"t"`
	}{model.NewHubTime(ts)})
	require.NoError(t, err)
	require.JSONEq(t, `{"t":"2024-11-16T02:16:16.000Z"}`, string(b))
	var back struct {
		T model.HubTime `json:"t"`
	}
	require.NoError(t, json.Unmarshal(b, &back))
	require.True(t, back.T.Equal(ts))
	require.Error(t, json.Unmarshal([]byte(`{"t":"nope"}`), &back))
}
