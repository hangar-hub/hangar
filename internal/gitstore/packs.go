package gitstore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/telemetry"
)

// PackInfo describes a stored pack.
type PackInfo struct {
	SHA         string
	Size        int64
	ObjectCount int64
	// Objects lists the object IDs in the pack.
	Objects []string
}

var packLineRe = func(line string) (string, bool) {
	kind, sha, ok := strings.Cut(strings.TrimSpace(line), "\t")
	if !ok || (kind != "pack" && kind != "keep") || len(sha) != 40 {
		return "", false
	}
	return sha, true
}

// IndexPack reads a pack from r into the staging repository with `git
// index-pack --stdin --fix-thin`, so thin packs are completed from the
// Repo's existing objects, and returns its SHA. strict also checks every
// object for corruption and every link for connectivity.
func (st *Staging) IndexPack(ctx context.Context, r io.Reader, strict bool) (string, error) {
	args := []string{"-c", "pack.writeReverseIndex=true", "index-pack", "--stdin", "--fix-thin", "--rev-index"}
	if strict {
		args = append(args, "--strict")
	}
	start := time.Now()
	out, err := st.Run(ctx, r, nil, args...)
	st.store.recordPackDuration(ctx, "receive-pack", start)
	if err != nil {
		return "", err
	}
	sha, ok := packLineRe(string(out))
	if !ok {
		return "", fmt.Errorf("gitstore: unexpected index-pack output %q", out)
	}
	return sha, nil
}

// PackLoose packs every loose object in the staging repository (the
// objects a server-side Commit just wrote) into one pack and returns its
// SHA. ok is false when there are no new objects.
func (st *Staging) PackLoose(ctx context.Context) (sha string, ok bool, err error) {
	var oids []string
	objDir := filepath.Join(st.Dir, "objects")
	entries, err := os.ReadDir(objDir)
	if err != nil {
		return "", false, err
	}
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) != 2 {
			continue
		}
		files, err := os.ReadDir(filepath.Join(objDir, e.Name()))
		if err != nil {
			return "", false, err
		}
		for _, f := range files {
			if len(f.Name()) == 38 {
				oids = append(oids, e.Name()+f.Name())
			}
		}
	}
	if len(oids) == 0 {
		return "", false, nil
	}
	start := time.Now()
	out, err := st.Run(ctx, strings.NewReader(strings.Join(oids, "\n")+"\n"), nil,
		"-c", "pack.writeReverseIndex=true", "pack-objects", "-q", filepath.Join(st.PackDir(), "pack"))
	st.store.recordPackDuration(ctx, "commit", start)
	if err != nil {
		return "", false, err
	}
	sha = strings.TrimSpace(string(out))
	if len(sha) != 40 {
		return "", false, fmt.Errorf("gitstore: unexpected pack-objects output %q", out)
	}
	return sha, true, nil
}

// PackObjects lists the objects in a pack of the staging repository, in
// the order of its index. A version 2 index, which is what git writes, is
// read directly; any other version goes through `git show-index`.
func (st *Staging) PackObjects(ctx context.Context, sha string) ([]string, error) {
	path := filepath.Join(st.PackDir(), "pack-"+sha+".idx")
	b, err := os.ReadFile(path) //nolint:gosec // a pack in the staging directory.
	if err != nil {
		return nil, err
	}
	if oids, ok, err := indexV2Objects(b); ok || err != nil {
		return oids, err
	}
	out, err := st.Run(ctx, bytes.NewReader(b), nil, "show-index")
	if err != nil {
		return nil, err
	}
	var oids []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) >= 2 {
			oids = append(oids, f[1])
		}
	}
	return oids, sc.Err()
}

// indexV2Magic starts a version 2 pack index.
var indexV2Magic = []byte{0xff, 't', 'O', 'c', 0, 0, 0, 2}

// indexV2Objects lists the SHA-1 object IDs in a version 2 pack index
// (gitformat-pack(5)): an 8-byte header, a 256-entry fan-out table whose
// last entry is the object count, then the sorted object IDs. ok is false
// when b is not a version 2 index.
func indexV2Objects(b []byte) (oids []string, ok bool, err error) {
	const header = 8 + 256*4
	if len(b) < header || !bytes.Equal(b[:8], indexV2Magic) {
		return nil, false, nil
	}
	n := int64(binary.BigEndian.Uint32(b[header-4 : header]))
	// Object IDs, CRC32s and 4-byte offsets, then the pack and index checksums.
	if int64(len(b)) < header+n*(sha1Size+4+4)+2*sha1Size {
		return nil, true, errors.New("gitstore: truncated pack index")
	}
	oids = make([]string, n)
	for i := range oids {
		off := header + i*sha1Size
		oids[i] = hex.EncodeToString(b[off : off+sha1Size])
	}
	return oids, true, nil
}

const sha1Size = 20

// StorePack records a pack written in the staging repository as pending
// and uploads it to the Blob store. The caller marks it live in the same
// transaction that moves refs, or orphaned if that fails.
//
// Packs are named by their checksum, so storing a pack again with the same
// contents, as a push sent again after a failed attempt does, finds the row
// of the earlier copy. An orphaned, retired or pending row becomes pending
// again, newly updated, so no sweep selects it during the grace period and
// a sweep that selected it before cannot claim it (DeletePack). StorePack
// records the row before it uploads the files, and waits while a sweep is
// deleting the earlier copy, so the files it uploads are never the ones a
// sweep deletes.
func (st *Staging) StorePack(ctx context.Context, sha string) (PackInfo, error) {
	s := st.store
	info := PackInfo{SHA: sha}
	objs, err := st.PackObjects(ctx, sha)
	if err != nil {
		return info, err
	}
	info.Objects = objs
	info.ObjectCount = int64(len(objs))
	fi, err := os.Stat(filepath.Join(st.PackDir(), "pack-"+sha+".pack"))
	if err != nil {
		return info, err
	}
	info.Size = fi.Size()
	if err := s.recordPending(ctx, st.repoID, info); err != nil {
		return info, err
	}
	for _, ext := range packExts {
		path := filepath.Join(st.PackDir(), "pack-"+sha+"."+ext)
		f, err := os.Open(path) //nolint:gosec // a pack in the staging directory.
		if err != nil {
			if optionalExt(ext) && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return info, err
		}
		fi, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return info, err
		}
		err = s.blobs.Put(ctx, PackKey(st.repoID, sha, ext), f, fi.Size())
		_ = f.Close()
		if err != nil {
			return info, fmt.Errorf("gitstore: upload pack: %w", err)
		}
	}
	s.addLocal(st.repoID, st.PackDir(), sha)
	return info, nil
}

// ErrPackDeleting means a sweep is still deleting an earlier copy of a
// pack being stored again. It is temporary: the store can be tried again.
var ErrPackDeleting = errors.New("gitstore: an earlier copy of the pack is being deleted; try again")

// ErrPackNotPending means MarkLive found the pack neither pending nor
// live: it was marked orphaned or claimed by a sweep after StorePack
// recorded it. Storing the pack again recovers.
var ErrPackNotPending = errors.New("gitstore: the pack is no longer pending; store it again")

// packDeletingWait bounds how long StorePack waits for a sweep that is
// deleting an earlier copy of the pack. A sweep deletes a pack's few files
// and then its row, so it is normally done well within this.
const packDeletingWait = 30 * time.Second

// recordPendingSQL inserts a pack row as pending or, when the pack is
// already recorded, makes the row pending again unless it is live. A row
// a sweep has claimed is left alone, so no row is affected.
const recordPendingSQL = `INSERT INTO packs (repo_id, pack_sha1, size, object_count, state, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (repo_id, pack_sha1) DO UPDATE SET
	state = CASE WHEN packs.state = ? THEN packs.state ELSE EXCLUDED.state END,
	retired_at = NULL,
	updated_at = EXCLUDED.updated_at
WHERE packs.state <> ?`

// recordPending records a pack as pending, waiting while a sweep deletes an
// earlier copy of it (see StorePack).
func (s *Store) recordPending(ctx context.Context, repoID uuid.UUID, info PackInfo) error {
	deadline := time.Now().Add(packDeletingWait)
	wait := 20 * time.Millisecond
	for {
		now := db.Now()
		res, err := s.db.ExecContext(ctx, recordPendingSQL, repoID, info.SHA, info.Size, info.ObjectCount, PackPending, now, now,
			PackLive, PackDeleting)
		if err != nil {
			return fmt.Errorf("gitstore: record pack: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return ErrPackDeleting
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		wait = min(wait*2, time.Second)
	}
}

// MarkLive makes a pending pack live inside the transaction that moves
// refs. It returns ErrPackNotPending unless the pack is pending or already
// live, so refs never move onto a pack a sweep may delete.
func MarkLive(ctx context.Context, tx bun.IDB, repoID uuid.UUID, sha string) error {
	res, err := tx.NewUpdate().Model((*db.Pack)(nil)).Set("state = ?", PackLive).Set("updated_at = ?", db.Now()).
		Where("repo_id = ?", repoID).Where("pack_sha1 = ?", sha).Where("state IN (?)", bun.List([]string{PackPending, PackLive})).Exec(ctx)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("%w: %s", ErrPackNotPending, sha)
	}
	return nil
}

// MarkOrphaned records that a pending pack lost its ref update, so the
// sweeper deletes it after the grace period.
func (s *Store) MarkOrphaned(ctx context.Context, repoID uuid.UUID, sha string) error {
	_, err := s.db.NewUpdate().Model((*db.Pack)(nil)).Set("state = ?", PackOrphaned).Set("retired_at = ?", db.Now()).
		Set("updated_at = ?", db.Now()).
		Where("repo_id = ?", repoID).Where("pack_sha1 = ?", sha).Where("state = ?", PackPending).Exec(ctx)
	return err
}

// LivePacks lists a Repo's live packs.
func LivePacks(ctx context.Context, idb bun.IDB, repoID uuid.UUID) ([]db.Pack, error) {
	var packs []db.Pack
	err := idb.NewSelect().Model(&packs).Where("repo_id = ?", repoID).Where("state = ?", PackLive).Order("created_at").Scan(ctx)
	return packs, err
}

// Compact repacks a Repo's live packs into one pack with a reachability
// bitmap, when it has more than threshold live packs (or always, when
// threshold is negative), and retires the old packs in one transaction
// (docs/spec/04-git.md "Compaction and garbage collection"). Unreachable
// objects are kept, so a ref that appears while compaction runs can never
// point at a deleted object. It reports whether it compacted.
func (s *Store) Compact(ctx context.Context, repoID uuid.UUID, headRef string, threshold int) (bool, error) {
	live, err := LivePacks(ctx, s.db, repoID)
	if err != nil {
		return false, err
	}
	if len(live) <= 1 || (threshold >= 0 && len(live) <= threshold) {
		return false, nil
	}
	objs, err := s.Acquire(ctx, repoID)
	if err != nil {
		return false, err
	}
	defer objs.Release()
	refs, err := Refs(ctx, s.db, repoID)
	if err != nil {
		return false, err
	}
	dir, err := s.tempDir("staging")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(dir)
	if err := writeView(dir, objs.Dir, headRef, refs); err != nil {
		return false, err
	}
	// The packs being compacted are linked in as local packs (not through
	// alternates), so repack rewrites them and --keep-unreachable keeps
	// every object they hold.
	if err := os.Remove(filepath.Join(dir, "objects", "info", "alternates")); err != nil {
		return false, err
	}
	old := make([]string, 0, len(live))
	for _, p := range live {
		for _, ext := range []string{"pack", "rev", "idx"} {
			src := filepath.Join(objs.Dir, "objects", "pack", "pack-"+p.PackSHA1+"."+ext)
			dst := filepath.Join(dir, "objects", "pack", "pack-"+p.PackSHA1+"."+ext)
			if err := os.Link(src, dst); err != nil && (!optionalExt(ext) || !errors.Is(err, os.ErrNotExist)) {
				return false, fmt.Errorf("gitstore: link pack for compaction: %w", err)
			}
		}
		old = append(old, p.PackSHA1)
	}
	start := time.Now()
	if _, err := s.git.Run(ctx, dir, nil, nil, "-c", "pack.writeReverseIndex=true", "-c", "repack.writeBitmaps=true",
		"repack", "-a", "-d", "-q", "--keep-unreachable"); err != nil {
		return false, err
	}
	s.recordPackDuration(ctx, "compact", start)
	packs, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "pack-*.pack"))
	if err != nil {
		return false, err
	}
	if len(packs) != 1 {
		return false, fmt.Errorf("gitstore: repack left %d packs", len(packs))
	}
	sha := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(packs[0]), "pack-"), ".pack")
	st := &Staging{Dir: dir, Objects: objs, store: s, repoID: repoID}
	// The bitmap is uploaded with the pack, so every replica serves with it.
	info, err := st.StorePack(ctx, sha)
	if err != nil {
		return false, err
	}
	now := db.Now()
	err = s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if err := MarkLive(ctx, tx, repoID, sha); err != nil {
			return err
		}
		retire := make([]string, 0, len(old))
		for _, o := range old {
			if o != sha {
				retire = append(retire, o)
			}
		}
		if len(retire) == 0 {
			return nil
		}
		_, err := tx.NewUpdate().Model((*db.Pack)(nil)).Set("state = ?", PackRetired).Set("retired_at = ?", now).Set("updated_at = ?", now).
			Where("repo_id = ?", repoID).Where("pack_sha1 IN (?)", bun.List(retire)).Where("state = ?", PackLive).Exec(ctx)
		return err
	})
	if err != nil {
		_ = s.MarkOrphaned(context.WithoutCancel(ctx), repoID, sha)
		return false, err
	}
	s.recordPacks(ctx, repoID)
	s.log.InfoContext(ctx, "git packs compacted", "repo.uuid", repoID.String(), "packs", len(old), "objects", info.ObjectCount)
	return true, nil
}

// packExts are the files of a pack, in upload order: the .idx goes last,
// because git only sees a pack once its index exists.
var packExts = []string{"pack", "rev", "bitmap", "idx"}

// optionalExt reports whether a pack file may be absent.
func optionalExt(ext string) bool { return ext == "rev" || ext == "bitmap" }

// Sweep deletes retired and orphaned packs, pending packs whose push never
// finished and packs an earlier sweep did not finish deleting, once they
// are older than grace (git.pack_grace_period, longer than the longest
// request). It returns how many packs it deleted.
func (s *Store) Sweep(ctx context.Context, grace time.Duration) (int, error) {
	cutoff := db.Now().Add(-grace)
	const cond = `((state IN (?) AND retired_at < ?) OR (state IN (?) AND updated_at < ?))`
	args := []any{bun.List([]string{PackRetired, PackOrphaned}), cutoff, bun.List([]string{PackPending, PackDeleting}), cutoff}
	var packs []db.Pack
	if err := s.db.NewSelect().Model(&packs).Column("repo_id", "pack_sha1").Where(cond, args...).Scan(ctx); err != nil {
		return 0, err
	}
	n := 0
	for _, p := range packs {
		deleted, err := DeletePack(ctx, s.db, s.blobs, p.RepoID, p.PackSHA1, cond, args...)
		if err != nil {
			return n, err
		}
		if deleted {
			n++
		}
	}
	return n, nil
}

// DeletePack deletes a pack a sweep selected because it met eligible, a
// condition on its packs row with args. It claims the pack first, by
// setting its state to deleting with an update that checks the row still
// meets eligible, so a pack stored again since the sweep selected it is
// left alone and deleted is false. StorePack waits while a pack is
// deleting, so the files deleted here are never ones it uploaded. The row
// goes once every file has. If a file delete fails, the error is returned
// with deleted true, and the row stays deleting for a later sweep: eligible
// should match a deleting row whose updated_at is older than the grace
// period, which assumes no sweep takes that long to delete one pack.
func DeletePack(ctx context.Context, d *db.DB, blobs blob.Store, repoID uuid.UUID, sha, eligible string, args ...any) (deleted bool, err error) {
	res, err := d.NewUpdate().Model((*db.Pack)(nil)).Set("state = ?", PackDeleting).Set("updated_at = ?", db.Now()).
		Where("repo_id = ? AND pack_sha1 = ?", repoID, sha).Where(eligible, args...).Exec(ctx)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, nil
	}
	var keys []string
	if err := blobs.List(ctx, PackKey(repoID, sha, ""), func(i blob.Info) error {
		keys = append(keys, i.Key)
		return nil
	}); err != nil {
		return true, fmt.Errorf("gitstore: list pack files: %w", err)
	}
	for _, k := range keys {
		if err := blobs.Delete(ctx, k); err != nil && !errors.Is(err, blob.ErrNotFound) {
			return true, fmt.Errorf("gitstore: delete pack file: %w", err)
		}
	}
	if _, err := d.NewDelete().Model((*db.Pack)(nil)).Where("repo_id = ? AND pack_sha1 = ? AND state = ?", repoID, sha, PackDeleting).
		Exec(ctx); err != nil {
		return true, err
	}
	return true, nil
}

// DeleteRepo removes every pack of a Repo from the Blob store and the local
// cache. The database rows go with the Repo row.
func (s *Store) DeleteRepo(ctx context.Context, repoID uuid.UUID) error {
	var keys []string
	if err := s.blobs.List(ctx, RepoPrefix(repoID), func(i blob.Info) error {
		keys = append(keys, i.Key)
		return nil
	}); err != nil {
		return err
	}
	for _, k := range keys {
		if err := s.blobs.Delete(ctx, k); err != nil && !errors.Is(err, blob.ErrNotFound) {
			return err
		}
	}
	s.Forget(repoID)
	return nil
}

func (s *Store) recordPackDuration(ctx context.Context, service string, start time.Time) {
	if s.inst == nil || s.inst.GitPackDuration == nil {
		return
	}
	s.inst.GitPackDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attribute.String(telemetry.MetricService, service)))
}

func (s *Store) recordPacks(ctx context.Context, repoID uuid.UUID) {
	if s.inst == nil || s.inst.GitPacks == nil {
		return
	}
	type count struct {
		State string `bun:"state"`
		N     int64  `bun:"n"`
	}
	var counts []count
	if err := s.db.NewSelect().Model((*db.Pack)(nil)).Column("state").ColumnExpr("count(*) AS n").Group("state").Scan(ctx, &counts); err != nil {
		s.log.DebugContext(ctx, "count packs", "error", err.Error(), "repo.uuid", repoID.String())
		return
	}
	for _, c := range counts {
		s.inst.GitPacks.Record(ctx, c.N, metric.WithAttributes(attribute.String(telemetry.MetricState, c.State)))
	}
}
