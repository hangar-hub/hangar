package gitserver

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

func TestConfigFrom(t *testing.T) {
	c := config.Default()
	c.Server.PublicURL = "https://hangar.example.com"
	got := ConfigFrom(&c)
	require.Equal(t, "https://hangar.example.com", got.PublicURL)
	require.Equal(t, int64(10<<20), got.MaxBlobSize)
	require.Equal(t, int64(2<<30), got.MaxPackSize)
	require.Equal(t, int64(64<<20), got.MultipartThreshold)
	require.Equal(t, 24*time.Hour, got.UploadURLTTL)
	require.Equal(t, 10*time.Minute, got.VerifyWait)
	require.Equal(t, time.Hour, got.Timeout)
	require.Equal(t, config.DownloadRedir, got.DownloadMode)
	require.Equal(t, time.Hour, got.DownloadURLTTL)
}

func TestNewChecksDependencies(t *testing.T) {
	_, err := New(Config{PublicURL: "http://x"}, Deps{})
	require.Error(t, err)
}

func TestParseRepoPath(t *testing.T) {
	for _, tc := range []struct {
		path string
		id   string
		typ  model.RepoType
		rest []string
		ok   bool
	}{
		{"/alice/tiny.git/info/refs", "alice/tiny", model.RepoTypeModel, []string{"info", "refs"}, true},
		{"/alice/tiny/git-upload-pack", "alice/tiny", model.RepoTypeModel, []string{"git-upload-pack"}, true},
		{"/datasets/alice/data.git/info/lfs/objects/batch", "alice/data", model.RepoTypeDataset, []string{"info", "lfs", "objects", "batch"}, true},
		{"/alice", "", "", nil, false},
		{"/datasets/alice", "", "", nil, false},
		{"/alice/bad%2Fname.git/info/refs", "", "", nil, false},
		{"/alice/tiny.git/info/%2E%2E", "", "", nil, false},
		{"/alice/tiny.git/a%2Fb", "", "", nil, false},
		{"/alice/tiny.git//x", "", "", nil, false},
		{"/alice/tiny.git/%zz", "", "", nil, false},
		{"/-bad/tiny.git/info/refs", "", "", nil, false},
	} {
		rp, err := parseRepoPath(tc.path)
		if !tc.ok {
			require.Error(t, err, tc.path)
			continue
		}
		require.NoError(t, err, tc.path)
		require.Equal(t, tc.id, rp.ID.String())
		require.Equal(t, tc.typ, rp.ID.Type)
		require.Equal(t, tc.rest, rp.Rest)
	}
}

func TestReadViewRefs(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		p := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
	}
	a, b, c := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	_, err := readViewRefs(dir)
	require.Error(t, err, "HEAD is required")
	write("HEAD", "ref: refs/heads/main\n")
	write("packed-refs", "# pack-refs with: peeled fully-peeled sorted\n"+a+" refs/heads/main\n"+b+" refs/tags/v1\n^"+c+"\n"+c+" refs/pr/1\nnot a ref line\n"+strings.ToUpper(c)+" refs/heads/upper\n")
	write("refs/heads/main", b+"\n")
	write("refs/heads/feature", c+"\n")
	write("refs/heads/sym", "ref: refs/heads/main\n")
	write("refs/heads/x.lock", a+"\n")
	v, err := readViewRefs(dir)
	require.NoError(t, err)
	require.Equal(t, "refs/heads/main", v.head)
	require.Equal(t, map[string]string{
		"refs/heads/main": b, "refs/tags/v1": b, "refs/pr/1": c, "refs/heads/feature": c, "refs/heads/upper": c,
	}, v.refs)

	refs := v.pushRefs()
	require.Contains(t, refs, plumbing.HEAD)
	require.NotContains(t, refs, plumbing.ReferenceName("refs/pr/1"))
	m := &memoryRefs{refs: refs}
	n, err := m.CountLooseRefs()
	require.NoError(t, err)
	require.Equal(t, len(refs), n)
	require.NoError(t, m.PackRefs())
	it, err := m.IterReferences()
	require.NoError(t, err)
	count := 0
	require.NoError(t, it.ForEach(func(*plumbing.Reference) error { count++; return nil }))
	require.Equal(t, len(refs), count)

	// reset puts refs back to their old values or removes created ones.
	main := plumbing.ReferenceName("refs/heads/main")
	created := plumbing.ReferenceName("refs/heads/new")
	require.NoError(t, m.SetReference(plumbing.NewHashReference(created, plumbing.NewHash(a))))
	m.reset([]plumbing.ReferenceName{main, created}, []plumbing.Hash{plumbing.NewHash(a), plumbing.ZeroHash})
	got, err := m.Reference(main)
	require.NoError(t, err)
	require.Equal(t, a, got.Hash().String())
	_, err = m.Reference(created)
	require.ErrorIs(t, err, plumbing.ErrReferenceNotFound)
	require.NoError(t, m.RemoveReference(main))

	// A detached HEAD and a missing branch give no HEAD symref.
	write("HEAD", a+"\n")
	v, err = readViewRefs(dir)
	require.NoError(t, err)
	require.Empty(t, v.head)
	require.NotContains(t, v.pushRefs(), plumbing.HEAD)

	// packed-refs that is a directory cannot be read.
	require.NoError(t, os.Remove(filepath.Join(dir, "packed-refs")))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "packed-refs"), 0o755))
	_, err = readViewRefs(dir)
	require.Error(t, err)
}

func TestFormatSize(t *testing.T) {
	require.Equal(t, "512 B", formatSize(512))
	require.Equal(t, "1 KiB", formatSize(1024))
	require.Equal(t, "1.5 MiB", formatSize(3<<19))
	require.Equal(t, "10 MiB", formatSize(10<<20))
	require.Equal(t, "2 GiB", formatSize(2<<30))
	require.Equal(t, "4 PiB", formatSize(4<<50))
	require.Equal(t, "4096 PiB", formatSize(4<<60))
}

func TestPartSize(t *testing.T) {
	s := &Server{}
	require.Equal(t, int64(16<<20), s.partSize(100))
	require.Equal(t, int64(16<<20), s.partSize(100<<30))
	// 5 TiB needs parts over 16 MiB to stay within 10,000 parts.
	p := s.partSize(5 << 40)
	require.LessOrEqual(t, (int64(5<<40)+p-1)/p, int64(maxParts))
	require.Zero(t, p%(1<<20))
	s.cfg.PartSize = 1 << 10
	require.Equal(t, int64(1<<10), s.partSize(100))
	require.Equal(t, int64(2000), s.partSize(10000*2000))
}

func TestLimitReader(t *testing.T) {
	l := &limitReader{r: strings.NewReader(strings.Repeat("x", 100)), max: 10}
	_, err := io.ReadAll(l)
	var tooLarge errPackTooLarge
	require.ErrorAs(t, err, &tooLarge)
	require.Contains(t, err.Error(), "10 B limit")
	_, err = l.Read(make([]byte, 1))
	require.ErrorAs(t, err, &tooLarge)
	b, err := io.ReadAll(&limitReader{r: strings.NewReader("abc")})
	require.NoError(t, err)
	require.Equal(t, "abc", string(b))
}

func TestApplyErrorMapping(t *testing.T) {
	check := func(err error, outcome, msg string) {
		t.Helper()
		var rf *refusal
		require.ErrorAs(t, applyError(err), &rf)
		require.Equal(t, outcome, rf.outcome)
		require.Contains(t, rf.Error(), msg)
	}
	check(&repo.PolicyError{Ref: "refs/heads/main", Message: "no"}, outcomeRejected, "refs/heads/main: no")
	check(errors.Join(errors.New("x"), repo.ErrRefConflict), outcomeConflict, "fetch first")
	check(repo.ErrReadOnly, outcomeRejected, "read-only")
	check(repo.ErrLFSObjectMissing, outcomeRejected, "LFS object")
	other := errors.New("disk on fire")
	require.Equal(t, other, applyError(other))
	require.Equal(t, "a b c", oneLine("a\nb  \t c\n"))
}

func TestGitProtocolHeader(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	require.Empty(t, gitProtocol(r))
	r.Header.Set("Git-Protocol", "version=2:object-format=sha1")
	require.Equal(t, "version=2:object-format=sha1", gitProtocol(r))
	require.True(t, isV2(gitProtocol(r)))
	r.Header.Set("Git-Protocol", "version=1")
	require.False(t, isV2(gitProtocol(r)))
	r.Header.Set("Git-Protocol", "version=2\x01")
	require.Empty(t, gitProtocol(r))
	r.Header.Set("Git-Protocol", strings.Repeat("a", 300))
	require.Empty(t, gitProtocol(r))
}

func TestLimitedBuffer(t *testing.T) {
	var b limitedBuffer
	n, err := b.Write(make([]byte, maxStderr+10))
	require.NoError(t, err)
	require.Equal(t, maxStderr+10, n)
	_, _ = b.Write([]byte("more"))
	require.Equal(t, maxStderr, b.Len())
}

func TestQuarantineFlattensAlternates(t *testing.T) {
	view := t.TempDir()
	cache := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(view, "objects", "info"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(view, "objects", "info", "alternates"),
		[]byte("# comment\n\n"+cache+"\n../../relative/objects\n"), 0o644))
	s := &Server{cfg: Config{TempDir: t.TempDir()}}
	q, err := s.newQuarantine(view)
	require.NoError(t, err)
	b, err := os.ReadFile(filepath.Join(q.dir, "objects", "info", "alternates"))
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	require.Len(t, lines, 3)
	require.Equal(t, absPath(filepath.Join(view, "objects")), lines[0])
	require.Equal(t, absPath(cache), lines[1])
	require.True(t, filepath.IsAbs(lines[2]))
	p, err := q.pack()
	require.NoError(t, err)
	require.Empty(t, p)
	for _, name := range []string{"pack-1.pack", "pack-2.pack"} {
		require.NoError(t, os.WriteFile(filepath.Join(q.dir, "objects", "pack", name), nil, 0o644))
	}
	_, err = q.pack()
	require.Error(t, err)
	_, err = packObjects(filepath.Join(q.dir, "objects", "pack", "pack-1.pack"))
	require.Error(t, err, "no index")
	require.NoError(t, os.WriteFile(filepath.Join(q.dir, "objects", "pack", "pack-1.idx"), []byte("bad"), 0o644))
	_, err = packObjects(filepath.Join(q.dir, "objects", "pack", "pack-1.pack"))
	require.Error(t, err)

	s.cfg.TempDir = filepath.Join(q.dir, "objects", "pack", "pack-1.idx", "x")
	_, err = s.newQuarantine(view)
	require.Error(t, err)
}
