package gitstore_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/gitstore"
)

// stagePack writes one object into a new staging repository and packs it.
// The staging repository is closed when the test ends.
func (f *fixture) stagePack(t *testing.T, content string) (*gitstore.Staging, string) {
	t.Helper()
	st, err := f.s.NewStaging(f.ctx, f.repo)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	_, err = st.Run(f.ctx, strings.NewReader(content), nil, "hash-object", "-w", "--stdin")
	require.NoError(t, err)
	pack, ok, err := st.PackLoose(f.ctx)
	require.NoError(t, err)
	require.True(t, ok)
	return st, pack
}

func (f *fixture) markLive(sha string) error {
	return f.d.InTx(f.ctx, func(ctx context.Context, tx bun.Tx) error {
		return gitstore.MarkLive(ctx, tx, f.repo, sha)
	})
}

func (f *fixture) packRow(t *testing.T, sha string) db.Pack {
	t.Helper()
	var p db.Pack
	require.NoError(t, f.d.NewSelect().Model(&p).Where("repo_id = ? AND pack_sha1 = ?", f.repo, sha).Scan(f.ctx))
	return p
}

func (f *fixture) hasPackFiles(sha string) bool {
	_, ok := f.blobs.Bytes(gitstore.PackKey(f.repo, sha, "pack"))
	_, idx := f.blobs.Bytes(gitstore.PackKey(f.repo, sha, "idx"))
	return ok && idx
}

// TestStoringAnOrphanedPackAgainRevivesIt is the retry of a push whose
// first attempt failed after storing its pack: the retry stores the same
// pack, byte for byte. The pack must go live, and no sweep may delete it.
func TestStoringAnOrphanedPackAgainRevivesIt(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		f := newFixture(t, d, 0)
		st, pack := f.stagePack(t, "retried")
		_, err := st.StorePack(f.ctx, pack)
		require.NoError(t, err)
		// The first attempt's transaction failed.
		require.NoError(t, f.s.MarkOrphaned(f.ctx, f.repo, pack))
		require.ErrorIs(t, f.markLive(pack), gitstore.ErrPackNotPending, "an orphaned pack must not go live")

		_, err = st.StorePack(f.ctx, pack)
		require.NoError(t, err)
		row := f.packRow(t, pack)
		require.Equal(t, gitstore.PackPending, row.State)
		require.Nil(t, row.RetiredAt)
		require.NoError(t, f.markLive(pack))

		time.Sleep(5 * time.Millisecond)
		n, err := f.s.Sweep(f.ctx, 0)
		require.NoError(t, err)
		require.Zero(t, n)
		require.Equal(t, gitstore.PackLive, f.packRow(t, pack).State)
		require.True(t, f.hasPackFiles(pack))

		// Storing a live pack again leaves it live.
		_, err = st.StorePack(f.ctx, pack)
		require.NoError(t, err)
		require.Equal(t, gitstore.PackLive, f.packRow(t, pack).State)
	})
}

// TestSweepLeavesAPackStoredAgainSinceItWasSelected checks that a sweep
// that selected an orphaned pack cannot delete it once the pack has been
// stored again, and that it deletes the files and the row otherwise.
func TestSweepLeavesAPackStoredAgainSinceItWasSelected(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		f := newFixture(t, d, 0)
		st, pack := f.stagePack(t, "selected")
		_, err := st.StorePack(f.ctx, pack)
		require.NoError(t, err)
		require.NoError(t, f.s.MarkOrphaned(f.ctx, f.repo, pack))
		const orphaned = `state = 'orphaned' AND retired_at < ?`
		cutoff := db.Now().Add(time.Hour)

		// The sweep selected the pack; then a retry stored it again.
		_, err = st.StorePack(f.ctx, pack)
		require.NoError(t, err)
		deleted, err := gitstore.DeletePack(f.ctx, d, f.blobs, f.repo, pack, orphaned, cutoff)
		require.NoError(t, err)
		require.False(t, deleted)
		require.Equal(t, gitstore.PackPending, f.packRow(t, pack).State)
		require.True(t, f.hasPackFiles(pack))

		require.NoError(t, f.s.MarkOrphaned(f.ctx, f.repo, pack))
		deleted, err = gitstore.DeletePack(f.ctx, d, f.blobs, f.repo, pack, orphaned, cutoff)
		require.NoError(t, err)
		require.True(t, deleted)
		require.False(t, f.hasPackFiles(pack))
		n, err := d.NewSelect().Model((*db.Pack)(nil)).Where("pack_sha1 = ?", pack).Count(f.ctx)
		require.NoError(t, err)
		require.Zero(t, n)
	})
}

// TestStorePackWaitsForASweepDeletingIt checks that storing a pack a sweep
// is deleting waits until the sweep has finished, and only then uploads
// the files, so the sweep never deletes the new copy.
func TestStorePackWaitsForASweepDeletingIt(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		f := newFixture(t, d, 0)
		st, pack := f.stagePack(t, "deleting")
		_, err := st.StorePack(f.ctx, pack)
		require.NoError(t, err)
		// A sweep has claimed the pack and is deleting its files.
		_, err = d.NewUpdate().Model((*db.Pack)(nil)).Set("state = ?", gitstore.PackDeleting).
			Where("repo_id = ? AND pack_sha1 = ?", f.repo, pack).Exec(f.ctx)
		require.NoError(t, err)
		puts := f.blobs.Calls("Put")

		done := make(chan error, 1)
		go func() {
			_, err := st.StorePack(f.ctx, pack)
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("StorePack returned while the pack was being deleted: %v", err)
		case <-time.After(300 * time.Millisecond):
		}
		require.Equal(t, puts, f.blobs.Calls("Put"), "nothing is uploaded while the sweep runs")

		// The sweep finishes: files first, then the row.
		for _, ext := range []string{"pack", "rev", "bitmap", "idx"} {
			require.NoError(t, f.blobs.Delete(f.ctx, gitstore.PackKey(f.repo, pack, ext)))
		}
		_, err = d.NewDelete().Model((*db.Pack)(nil)).Where("repo_id = ? AND pack_sha1 = ?", f.repo, pack).Exec(f.ctx)
		require.NoError(t, err)

		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("StorePack did not finish after the sweep")
		}
		require.Equal(t, gitstore.PackPending, f.packRow(t, pack).State)
		require.True(t, f.hasPackFiles(pack))
		require.NoError(t, f.markLive(pack))
	})
}

// TestCompactionRetryAfterAFailedAttempt repeats a compaction whose first
// attempt stored its pack and then failed, leaving the pack orphaned and
// the old packs live. The retry writes the same pack. It must go live, so
// that retiring the old packs never leaves the Repo without its objects.
func TestCompactionRetryAfterAFailedAttempt(t *testing.T) {
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		f := newFixture(t, d, 0)
		one := f.commit(t, f.repo, "refs/heads/main", "a.txt", "")
		f.commit(t, f.repo, "refs/heads/main", "b.txt", one)
		before, err := gitstore.LivePacks(f.ctx, d, f.repo)
		require.NoError(t, err)
		require.Len(t, before, 2)

		did, err := f.s.Compact(f.ctx, f.repo, "refs/heads/main", -1)
		require.NoError(t, err)
		require.True(t, did)
		live, err := gitstore.LivePacks(f.ctx, d, f.repo)
		require.NoError(t, err)
		require.Len(t, live, 1)
		compacted := live[0].PackSHA1

		// Put the rows back as a failed first attempt leaves them.
		now := db.Now()
		_, err = d.NewUpdate().Model((*db.Pack)(nil)).Set("state = ?", gitstore.PackOrphaned).Set("retired_at = ?", now).
			Where("repo_id = ? AND pack_sha1 = ?", f.repo, compacted).Exec(f.ctx)
		require.NoError(t, err)
		_, err = d.NewUpdate().Model((*db.Pack)(nil)).Set("state = ?", gitstore.PackLive).Set("retired_at = NULL").
			Where("repo_id = ? AND pack_sha1 IN (?)", f.repo, bun.List([]string{before[0].PackSHA1, before[1].PackSHA1})).Exec(f.ctx)
		require.NoError(t, err)

		did, err = f.s.Compact(f.ctx, f.repo, "refs/heads/main", -1)
		require.NoError(t, err)
		require.True(t, did)
		live, err = gitstore.LivePacks(f.ctx, d, f.repo)
		require.NoError(t, err)
		require.Len(t, live, 1)
		require.Equal(t, compacted, live[0].PackSHA1, "the retry wrote the same pack")

		time.Sleep(5 * time.Millisecond)
		_, err = f.s.Sweep(f.ctx, 0)
		require.NoError(t, err)
		require.True(t, f.hasPackFiles(compacted))

		// A replica with a cold cache still has every object.
		s2, err := gitstore.New(gitstore.Options{DB: d, Blobs: f.blobs, CacheDir: t.TempDir()})
		require.NoError(t, err)
		v, err := s2.OpenView(f.ctx, f.repo, "refs/heads/main")
		require.NoError(t, err)
		defer v.Close()
		_, err = s2.Git().Output(f.ctx, v.Dir, "fsck", "--strict", "--no-dangling")
		require.NoError(t, err)
	})
}
