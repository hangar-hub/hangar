package gitstore_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/jamesagarside/hangar/internal/blob/blobtest"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/db/dbtest"
	"github.com/jamesagarside/hangar/internal/gitstore"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/telemetry"
)

type fixture struct {
	ctx   context.Context
	d     *db.DB
	blobs *blobtest.Fake
	s     *gitstore.Store
	repo  uuid.UUID
}

func newFixture(t *testing.T, d *db.DB, cacheSize int64) *fixture {
	t.Helper()
	ctx := context.Background()
	blobs := blobtest.NewFake()
	inst, err := telemetry.NewInstruments(noop.NewMeterProvider().Meter("test"))
	require.NoError(t, err)
	s, err := gitstore.New(gitstore.Options{DB: d, Blobs: blobs, CacheDir: t.TempDir(), CacheSize: cacheSize, Instruments: inst})
	require.NoError(t, err)
	return &fixture{ctx: ctx, d: d, blobs: blobs, s: s, repo: newRepoRow(t, d)}
}

// newRepoRow inserts a Namespace and a Repo so packs and refs rows can
// reference it.
func newRepoRow(t *testing.T, d *db.DB) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	ns := &db.Namespace{ID: db.NewID(), Name: "ns-" + uuid.NewString()[:8], Kind: "user"}
	_, err := d.NewInsert().Model(ns).Exec(ctx)
	require.NoError(t, err)
	r := &db.Repo{ID: db.NewID(), NamespaceID: ns.ID, Name: "r", Type: "model", Visibility: "public", DefaultBranch: "main",
		HeadRef: "refs/heads/main", Tags: []string{}, Settings: map[string]any{}, Gating: map[string]any{}}
	_, err = d.NewInsert().Model(r).Exec(ctx)
	require.NoError(t, err)
	return r.ID
}

// commit writes a Commit with one file into a staging repository, stores
// its pack and makes it live under ref.
func (f *fixture) commit(t *testing.T, repoID uuid.UUID, ref, file string, old model.CommitSHA) model.CommitSHA {
	t.Helper()
	st, err := f.s.NewStaging(f.ctx, repoID)
	require.NoError(t, err)
	defer st.Close()
	blobOID, err := st.Run(f.ctx, strings.NewReader(file), nil, "hash-object", "-w", "--stdin")
	require.NoError(t, err)
	tree, err := st.Run(f.ctx, strings.NewReader("100644 blob "+strings.TrimSpace(string(blobOID))+"\t"+file+"\n"), nil, "mktree")
	require.NoError(t, err)
	args := []string{"commit-tree", strings.TrimSpace(string(tree)), "-m", file}
	if old != "" {
		args = append(args, "-p", string(old))
	}
	sha, err := st.Run(f.ctx, nil, []string{"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@b", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@b"}, args...)
	require.NoError(t, err)
	pack, ok, err := st.PackLoose(f.ctx)
	require.NoError(t, err)
	require.True(t, ok)
	info, err := st.StorePack(f.ctx, pack)
	require.NoError(t, err)
	require.Equal(t, int64(3), info.ObjectCount)
	newSHA := model.CommitSHA(strings.TrimSpace(string(sha)))
	require.NoError(t, f.d.InTx(f.ctx, func(ctx context.Context, tx bun.Tx) error {
		if err := gitstore.UpdateRefs(ctx, tx, repoID, []gitstore.RefUpdate{{Name: ref, Old: old, New: newSHA}}); err != nil {
			return err
		}
		return gitstore.MarkLive(ctx, tx, repoID, pack)
	}))
	return newSHA
}

func TestViewServesSnapshotOfRefs(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		f := newFixture(t, d, 0)
		one := f.commit(t, f.repo, "refs/heads/main", "a.txt", "")
		two := f.commit(t, f.repo, "refs/heads/main", "b.txt", one)
		f.commit(t, f.repo, "refs/pr/1", "c.txt", "")

		v, err := f.s.OpenView(f.ctx, f.repo, "refs/heads/main")
		require.NoError(t, err)
		require.Len(t, v.Refs, 2)
		out, err := f.s.Git().Output(f.ctx, v.Dir, "for-each-ref", "--format=%(refname) %(objectname)")
		require.NoError(t, err)
		require.Equal(t, "refs/heads/main "+string(two)+"\nrefs/pr/1 "+string(v.Refs[1].Target), out)
		head, err := f.s.Git().Output(f.ctx, v.Dir, "rev-parse", "HEAD")
		require.NoError(t, err)
		require.Equal(t, string(two), head)
		_, err = f.s.Git().Output(f.ctx, v.Dir, "fsck", "--strict", "--no-dangling")
		require.NoError(t, err)
		require.NoError(t, v.Close())
		_, err = os.Stat(v.Dir)
		require.True(t, os.IsNotExist(err))
		var nilView *gitstore.View
		require.NoError(t, nilView.Close())
		var nilStaging *gitstore.Staging
		require.NoError(t, nilStaging.Close())
		var nilObjs *gitstore.Objects
		nilObjs.Release()

		target, ok, err := gitstore.LookupRef(f.ctx, d, f.repo, "refs/heads/main")
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, two, target)
		_, ok, err = gitstore.LookupRef(f.ctx, d, f.repo, "refs/heads/none")
		require.NoError(t, err)
		require.False(t, ok)
	})
}

func TestUpdateRefsCompareAndSwap(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		ctx := context.Background()
		repoID := newRepoRow(t, d)
		a, b := model.CommitSHA(strings.Repeat("a", 40)), model.CommitSHA(strings.Repeat("b", 40))
		run := func(us ...gitstore.RefUpdate) error {
			return d.InTx(ctx, func(ctx context.Context, tx bun.Tx) error { return gitstore.UpdateRefs(ctx, tx, repoID, us) })
		}
		require.NoError(t, run(gitstore.RefUpdate{Name: "refs/heads/main", New: a}))
		require.ErrorIs(t, run(gitstore.RefUpdate{Name: "refs/heads/main", New: b}), gitstore.ErrRefConflict, "create of an existing ref")
		require.ErrorIs(t, run(gitstore.RefUpdate{Name: "refs/heads/main", Old: b, New: a}), gitstore.ErrRefConflict)
		require.ErrorIs(t, run(gitstore.RefUpdate{Name: "refs/heads/main", Old: b}), gitstore.ErrRefConflict, "delete with a stale value")
		require.ErrorIs(t, run(gitstore.RefUpdate{Name: "refs/heads/x", Old: a, New: b}), gitstore.ErrRefConflict, "update of a missing ref")
		require.NoError(t, run(gitstore.RefUpdate{Name: "refs/heads/main", Old: a, New: b}, gitstore.RefUpdate{Name: "refs/tags/v1", New: a}))
		// All or nothing.
		require.ErrorIs(t, run(gitstore.RefUpdate{Name: "refs/heads/new", New: a}, gitstore.RefUpdate{Name: "refs/heads/main", Old: a, New: b}), gitstore.ErrRefConflict)
		refs, err := gitstore.Refs(ctx, d, repoID)
		require.NoError(t, err)
		require.Equal(t, []gitstore.Ref{{Name: "refs/heads/main", Target: b}, {Name: "refs/tags/v1", Target: a}}, refs)
		require.NoError(t, run(gitstore.RefUpdate{Name: "refs/tags/v1", Old: a}))

		for _, u := range []gitstore.RefUpdate{{Name: "main", New: a}, {Name: "refs/heads/x"}, {Name: "refs/heads/x", New: "abc"}} {
			require.ErrorIs(t, run(u), gitstore.ErrInvalidUpdate)
		}
		require.ErrorIs(t, run(gitstore.RefUpdate{Name: "refs/heads/d", New: a}, gitstore.RefUpdate{Name: "refs/heads/d", New: b}), gitstore.ErrInvalidUpdate)
	})
}

func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	d := dbtest.SQLite(t)
	f := newFixture(t, d, 1)
	other := newRepoRow(t, d)
	f.commit(t, f.repo, "refs/heads/main", "a.txt", "")
	f.commit(t, other, "refs/heads/main", "b.txt", "")
	// Everything over the 1 byte limit that nobody holds is evicted.
	require.Zero(t, f.s.CacheBytes())

	objs, err := f.s.Acquire(f.ctx, f.repo)
	require.NoError(t, err)
	require.Positive(t, f.s.CacheBytes(), "packs are downloaded again on demand")
	held := f.s.CacheBytes()
	objs2, err := f.s.Acquire(f.ctx, other)
	require.NoError(t, err)
	objs2.Release()
	require.Equal(t, held, f.s.CacheBytes(), "the Repo in use is kept; the other is evicted")
	_, err = f.s.Git().Output(f.ctx, objs.Dir, "cat-file", "--batch-all-objects", "--batch-check")
	require.NoError(t, err)
	objs.Release()
	objs.Release()
	require.Zero(t, f.s.CacheBytes())

	// A restart counts what the cache already holds.
	dir := t.TempDir()
	s1, err := gitstore.New(gitstore.Options{DB: d, Blobs: f.blobs, CacheDir: dir})
	require.NoError(t, err)
	o, err := s1.Acquire(f.ctx, f.repo)
	require.NoError(t, err)
	o.Release()
	s2, err := gitstore.New(gitstore.Options{DB: d, Blobs: f.blobs, CacheDir: dir})
	require.NoError(t, err)
	require.Equal(t, s1.CacheBytes(), s2.CacheBytes())
	s2.Forget(f.repo)
	require.Zero(t, s2.CacheBytes())

	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	_, err = s2.Acquire(ctx, uuid.New())
	require.Error(t, err)
}

func TestSweepAndDeleteRepo(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		f := newFixture(t, d, 0)
		f.commit(t, f.repo, "refs/heads/main", "a.txt", "")
		st, err := f.s.NewStaging(f.ctx, f.repo)
		require.NoError(t, err)
		_, err = st.Run(f.ctx, strings.NewReader("orphan"), nil, "hash-object", "-w", "--stdin")
		require.NoError(t, err)
		pack, ok, err := st.PackLoose(f.ctx)
		require.NoError(t, err)
		require.True(t, ok)
		_, err = st.StorePack(f.ctx, pack)
		require.NoError(t, err)
		require.NoError(t, st.Close())
		require.NoError(t, f.s.MarkOrphaned(f.ctx, f.repo, pack))

		n, err := f.s.Sweep(f.ctx, time.Hour)
		require.NoError(t, err)
		require.Zero(t, n, "inside the grace period")
		time.Sleep(10 * time.Millisecond)
		n, err = f.s.Sweep(f.ctx, time.Millisecond)
		require.NoError(t, err)
		require.Equal(t, 1, n)
		_, ok = f.blobs.Bytes(gitstore.PackKey(f.repo, pack, "pack"))
		require.False(t, ok)
		live, err := gitstore.LivePacks(f.ctx, d, f.repo)
		require.NoError(t, err)
		require.Len(t, live, 1)

		// Compaction is skipped below the threshold.
		did, err := f.s.Compact(f.ctx, f.repo, "refs/heads/main", 5)
		require.NoError(t, err)
		require.False(t, did)

		require.NoError(t, f.s.DeleteRepo(f.ctx, f.repo))
		for _, k := range f.blobs.Keys() {
			require.False(t, strings.HasPrefix(k, gitstore.RepoPrefix(f.repo)), k)
		}
		empty, ok, err := st.PackLoose(f.ctx)
		require.Error(t, err, "a closed staging directory is gone")
		require.False(t, ok)
		require.Empty(t, empty)
	})
}

func TestGitRunner(t *testing.T) {
	ctx := context.Background()
	g := gitstore.Git{Home: t.TempDir(), Timeout: time.Minute}
	dir := t.TempDir()
	_, err := g.Run(ctx, dir, nil, nil, "init", "-q", "--bare", ".")
	require.NoError(t, err)
	_, err = g.Run(ctx, dir, nil, nil, "rev-parse", "--verify", "nope")
	var ge *gitstore.Error
	require.True(t, errors.As(err, &ge))
	require.Contains(t, ge.Error(), "rev-parse")
	require.Error(t, errors.Unwrap(ge))

	out, err := g.Run(ctx, dir, strings.NewReader("hello"), nil, "hash-object", "-w", "--stdin")
	require.NoError(t, err)
	hello := strings.TrimSpace(string(out))

	info, err := g.BatchCheck(ctx, dir, []string{hello, strings.Repeat("f", 40)})
	require.NoError(t, err)
	require.Equal(t, gitstore.ObjectInfo{OID: hello, Type: "blob", Size: 5}, info[0])
	require.True(t, info[1].Missing)
	empty, err := g.BatchCheck(ctx, dir, nil)
	require.NoError(t, err)
	require.Empty(t, empty)

	var got []string
	require.NoError(t, g.BatchRead(ctx, dir, []string{hello, strings.Repeat("f", 40)}, func(i gitstore.ObjectInfo, c []byte) error {
		got = append(got, string(c)+"/"+i.Type)
		return nil
	}))
	require.Equal(t, []string{"hello/blob", "/"}, got)
	stop := errors.New("stop")
	require.ErrorIs(t, g.BatchRead(ctx, dir, []string{hello, hello}, func(gitstore.ObjectInfo, []byte) error { return stop }), stop)
	require.NoError(t, g.BatchRead(ctx, dir, nil, nil))

	// BatchReadHead keeps a prefix of each object and stays in step with
	// the stream, so objects after a long one are read correctly.
	out, err = g.Run(ctx, dir, strings.NewReader(strings.Repeat("x", 100000)), nil, "hash-object", "-w", "--stdin")
	require.NoError(t, err)
	long := strings.TrimSpace(string(out))
	var heads []string
	require.NoError(t, g.BatchReadHead(ctx, dir, []string{long, hello, strings.Repeat("f", 40), long}, 3, func(i gitstore.ObjectInfo, c []byte) error {
		heads = append(heads, fmt.Sprintf("%s/%d/%t", c, i.Size, i.Missing))
		return nil
	}))
	require.Equal(t, []string{"xxx/100000/false", "hel/5/false", "/0/true", "xxx/100000/false"}, heads)
	heads = nil
	require.NoError(t, g.BatchReadHead(ctx, dir, []string{hello}, 0, func(i gitstore.ObjectInfo, c []byte) error {
		heads = append(heads, fmt.Sprintf("%q/%d", c, i.Size))
		return nil
	}))
	require.Equal(t, []string{`""/5`}, heads)

	rc, err := g.OpenBlob(ctx, dir, hello)
	require.NoError(t, err)
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.Equal(t, "hello", string(b))
	require.NoError(t, rc.Close())
	rc, err = g.OpenBlob(ctx, dir, strings.Repeat("f", 40))
	require.NoError(t, err)
	_, err = io.ReadAll(rc)
	require.Error(t, err)
	require.NoError(t, rc.Close())

	var sb strings.Builder
	require.NoError(t, g.Stream(ctx, dir, nil, &sb, nil, "cat-file", "blob", hello))
	require.Equal(t, "hello", sb.String())
	require.Error(t, g.Stream(ctx, dir, nil, &sb, nil, "cat-file", "blob", "nope"))

	short := gitstore.Git{Home: g.Home, Timeout: time.Nanosecond}
	_, err = short.Run(ctx, dir, nil, nil, "cat-file", "-t", hello)
	require.Error(t, err)
	require.Contains(t, gitstore.PackKey(uuid.Nil, "abc", "idx"), "/pack/pack-abc.idx")
	require.Equal(t, filepath.Join("git", uuid.Nil.String())+"/", gitstore.RepoPrefix(uuid.Nil))
}

func TestNewValidatesOptions(t *testing.T) {
	_, err := gitstore.New(gitstore.Options{})
	require.Error(t, err)
	_, err = gitstore.New(gitstore.Options{DB: dbtest.SQLite(t), Blobs: blobtest.NewFake()})
	require.Error(t, err)
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "views", "stale"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "repos", "not-a-uuid"), 0o700))
	_, err = gitstore.New(gitstore.Options{DB: dbtest.SQLite(t), Blobs: blobtest.NewFake(), CacheDir: dir})
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(dir, "views", "stale"))
	require.True(t, os.IsNotExist(err), "views from a crashed run are removed")
}

func TestIndexPackAndCompact(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		f := newFixture(t, d, 0)
		one := f.commit(t, f.repo, "refs/heads/main", "a.txt", "")
		two := f.commit(t, f.repo, "refs/heads/main", "b.txt", one)
		three := f.commit(t, f.repo, "refs/heads/main", "c.txt", two)

		// A pack built elsewhere is indexed, thin packs included.
		v, err := f.s.OpenView(f.ctx, f.repo, "refs/heads/main")
		require.NoError(t, err)
		thin, err := f.s.Git().Run(f.ctx, v.Dir, strings.NewReader(string(three)+"\n^"+string(two)+"\n"), nil, "pack-objects", "--revs", "--thin", "--stdout", "-q")
		require.NoError(t, err)
		require.NoError(t, v.Close())
		st, err := f.s.NewStaging(f.ctx, f.repo)
		require.NoError(t, err)
		sha, err := st.IndexPack(f.ctx, strings.NewReader(string(thin)), true)
		require.NoError(t, err)
		objs, err := st.PackObjects(f.ctx, sha)
		require.NoError(t, err)
		require.Contains(t, objs, string(three))
		// The index is read without git; the list matches git show-index.
		idx, err := os.Open(filepath.Join(st.PackDir(), "pack-"+sha+".idx"))
		require.NoError(t, err)
		shown, err := st.Run(f.ctx, idx, nil, "show-index")
		require.NoError(t, idx.Close())
		require.NoError(t, err)
		var want []string
		for _, line := range strings.Split(strings.TrimSpace(string(shown)), "\n") {
			want = append(want, strings.Fields(line)[1])
		}
		require.Equal(t, want, objs)
		// A truncated index is an error, not a short list.
		idxPath := filepath.Join(st.PackDir(), "pack-"+sha+".idx")
		full, err := os.ReadFile(idxPath)
		require.NoError(t, err)
		require.NoError(t, os.Chmod(idxPath, 0o600))
		require.NoError(t, os.WriteFile(idxPath, full[:len(full)-50], 0o600))
		_, err = st.PackObjects(f.ctx, sha)
		require.ErrorContains(t, err, "truncated pack index")
		require.NoError(t, os.WriteFile(idxPath, full, 0o600))
		_, err = st.IndexPack(f.ctx, strings.NewReader("garbage"), false)
		require.Error(t, err)
		require.NoError(t, st.Close())

		did, err := f.s.Compact(f.ctx, f.repo, "refs/heads/main", -1)
		require.NoError(t, err)
		require.True(t, did)
		live, err := gitstore.LivePacks(f.ctx, d, f.repo)
		require.NoError(t, err)
		require.Len(t, live, 1)
		require.Equal(t, int64(9), live[0].ObjectCount)
		_, ok := f.blobs.Bytes(gitstore.PackKey(f.repo, live[0].PackSHA1, "bitmap"))
		require.True(t, ok, "the reachability bitmap is stored with the pack")
		n, err := f.s.Sweep(f.ctx, 0)
		require.NoError(t, err)
		require.Equal(t, 3, n)

		// A replica with a cold cache serves the compacted pack with its bitmap.
		s2, err := gitstore.New(gitstore.Options{DB: d, Blobs: f.blobs, CacheDir: t.TempDir()})
		require.NoError(t, err)
		v2, err := s2.OpenView(f.ctx, f.repo, "refs/heads/main")
		require.NoError(t, err)
		defer v2.Close()
		_, err = s2.Git().Output(f.ctx, v2.Dir, "fsck", "--strict", "--no-dangling")
		require.NoError(t, err)
		bitmaps, err := filepath.Glob(filepath.Join(v2.Dir, "..", "..", "repos", f.repo.String(), "objects", "pack", "*.bitmap"))
		require.NoError(t, err)
		require.Len(t, bitmaps, 1)
	})
}

// TestGitReportsCancellation is the regression test for commit diffs that a
// client cancelled being answered with 500: git is killed when its context
// ends, and the read of its stdout failed with a bare EOF ("read cat-file
// header: EOF") that did not say the request was cancelled. Every way of
// running git now reports context.Canceled. The git on PATH here starts and
// then waits, so only the cancellation can end it.
func TestGitReportsCancellation(t *testing.T) {
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec sleep 60\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	g := gitstore.Git{Home: t.TempDir(), Timeout: time.Minute}
	dir := t.TempDir()
	oid := strings.Repeat("a", 40)

	cancelled := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		time.AfterFunc(50*time.Millisecond, cancel)
		return ctx
	}

	err := g.BatchReadHead(cancelled(), dir, []string{oid}, -1, func(gitstore.ObjectInfo, []byte) error { return nil })
	require.ErrorIs(t, err, context.Canceled)

	rc, err := g.OpenBlob(cancelled(), dir, oid)
	require.NoError(t, err)
	_, err = io.ReadAll(rc)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, rc.Close())

	err = g.Stream(cancelled(), dir, nil, io.Discard, nil, "cat-file", "blob", oid)
	require.ErrorIs(t, err, context.Canceled)

	_, err = g.Run(cancelled(), dir, nil, nil, "cat-file", "blob", oid)
	require.ErrorIs(t, err, context.Canceled)
}
