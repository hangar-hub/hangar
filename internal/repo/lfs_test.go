package repo_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

func TestLFSIngestDeduplicatesAndVerifies(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		content := []byte("some weights")
		oid := sha256Hex(content)
		obj, err := e.lfs.Ingest(e.ctx, oid, int64(len(content)), bytes.NewReader(content))
		require.NoError(t, err)
		require.Equal(t, repo.LFSObject{OID: oid, Size: int64(len(content)), StorageKey: model.LFSObjectKey(oid), Verified: true}, obj)
		puts := e.blobs.Calls("Put")

		// UP-6: the same object is never stored twice. The bytes are still
		// read and checked, so a successful Ingest proves the caller sent
		// them (decided by James on 2026-10-18, "check at Commit").
		again, err := e.lfs.Ingest(e.ctx, oid, int64(len(content)), bytes.NewReader(content))
		require.NoError(t, err)
		require.Equal(t, obj, again)
		require.Equal(t, puts, e.blobs.Calls("Put"))
		_, err = e.lfs.Ingest(e.ctx, oid, int64(len(content)), strings.NewReader("never read"))
		require.ErrorIs(t, err, repo.ErrChecksumMismatch, "other bytes under a stored object's SHA-256 prove nothing")
		require.NotContains(t, err.Error(), "stored", "the answer does not describe the stored object")
		_, err = e.lfs.Ingest(e.ctx, oid, int64(len(content)), strings.NewReader("some weights and more"))
		require.ErrorIs(t, err, repo.ErrChecksumMismatch, "longer content is not the object")
		_, err = e.lfs.Ingest(e.ctx, oid, 3, strings.NewReader("abc"))
		require.ErrorIs(t, err, repo.ErrChecksumMismatch)
		require.Equal(t, puts, e.blobs.Calls("Put"))

		// Corrupt or short content is never stored.
		other := sha256Hex([]byte("other"))
		_, err = e.lfs.Ingest(e.ctx, other, 5, strings.NewReader("wrong"))
		require.ErrorIs(t, err, repo.ErrChecksumMismatch)
		_, err = e.lfs.Ingest(e.ctx, other, 5, strings.NewReader("oth"))
		require.ErrorIs(t, err, repo.ErrChecksumMismatch)
		_, err = e.lfs.Stat(e.ctx, other)
		require.ErrorIs(t, err, repo.ErrNotFound)
		_, ok := e.blobs.Bytes(model.LFSObjectKey(other))
		require.False(t, ok)
		_, err = e.lfs.Ingest(e.ctx, "nope", 1, strings.NewReader("x"))
		require.ErrorIs(t, err, repo.ErrInvalid)
		_, err = e.lfs.Stat(e.ctx, "nope")
		require.ErrorIs(t, err, repo.ErrInvalid)

		many, err := e.lfs.StatMany(e.ctx, []string{oid, other})
		require.NoError(t, err)
		require.Len(t, many, 1)

		rc, got, err := e.lfs.Open(e.ctx, oid, &blob.Range{Start: 5, End: -1})
		require.NoError(t, err)
		b := new(bytes.Buffer)
		_, _ = b.ReadFrom(rc)
		rc.Close()
		require.Equal(t, "weights", b.String())
		require.Equal(t, obj, got)
		_, _, err = e.lfs.Open(e.ctx, other, nil)
		require.ErrorIs(t, err, repo.ErrNotFound)
		_, err = e.lfs.PresignGet(e.ctx, oid, time.Hour, "w.bin")
		require.ErrorIs(t, err, blob.ErrNotSupported)

		// Verify re-hashes objects written by presigned PUTs.
		pv := []byte("presigned")
		pvOID := sha256Hex(pv)
		_, err = e.lfs.Verify(e.ctx, pvOID, int64(len(pv)), uuid.Nil)
		require.ErrorIs(t, err, repo.ErrNotFound)
		require.NoError(t, e.blobs.Put(e.ctx, model.LFSObjectKey(pvOID), bytes.NewReader(pv), int64(len(pv))))
		v, err := e.lfs.Verify(e.ctx, pvOID, int64(len(pv)), uuid.Nil)
		require.NoError(t, err)
		require.True(t, v.Verified)
		// Stored, with no upload of it to check: not found, whatever the
		// size, so Verify never gives away an object's size.
		_, err = e.lfs.Verify(e.ctx, pvOID, 1, uuid.Nil)
		require.ErrorIs(t, err, repo.ErrNotFound)
		_, err = e.lfs.Verify(e.ctx, pvOID, int64(len(pv)), uuid.Nil)
		require.ErrorIs(t, err, repo.ErrNotFound)
		bad := sha256Hex([]byte("claimed"))
		require.NoError(t, e.blobs.Put(e.ctx, model.LFSObjectKey(bad), strings.NewReader("actual!"), 7))
		_, err = e.lfs.Verify(e.ctx, bad, 7, uuid.Nil)
		require.ErrorIs(t, err, repo.ErrChecksumMismatch)
		_, ok = e.blobs.Bytes(model.LFSObjectKey(bad))
		require.False(t, ok, "a corrupt presigned upload is deleted")
		_, err = e.lfs.PresignPut(e.ctx, "zz", 1, time.Hour, uuid.Nil)
		require.ErrorIs(t, err, repo.ErrInvalid)
		_, err = e.lfs.PresignPut(e.ctx, oid, 1, time.Hour, uuid.Nil)
		require.ErrorIs(t, err, blob.ErrNotSupported)
	})
}

func TestLFSMultipartUploads(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		r := e.create(repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "up"}, Actor: alice})
		content := bytes.Repeat([]byte("0123456789"), 25)
		oid := sha256Hex(content)
		u, err := e.lfs.CreateUpload(e.ctx, repo.UploadParams{OID: oid, Size: int64(len(content)), RepoUUID: r.UUID, UserID: alice.UserID, Multipart: true, PartSize: 100, TTL: time.Hour})
		require.NoError(t, err)
		require.Equal(t, 3, u.PartCount)
		owner, err := e.lfs.UploadRepo(e.ctx, u.ID)
		require.NoError(t, err)
		require.Equal(t, r.UUID, owner)

		_, err = e.lfs.PutPart(e.ctx, u.ID, 4, bytes.NewReader(content[:10]), 10)
		require.ErrorIs(t, err, repo.ErrInvalid)
		_, err = e.lfs.PutPart(e.ctx, u.ID, 1, bytes.NewReader(content[:10]), 10)
		require.ErrorIs(t, err, repo.ErrInvalid, "parts must be the part size")
		_, err = e.lfs.PresignPart(e.ctx, u.ID, 1, time.Hour)
		require.ErrorIs(t, err, blob.ErrNotSupported)
		var parts []blob.Part
		for i := range 3 {
			chunk := content[i*100 : min((i+1)*100, len(content))]
			etag, err := e.lfs.PutPart(e.ctx, u.ID, i+1, bytes.NewReader(chunk), int64(len(chunk)))
			require.NoError(t, err)
			parts = append(parts, blob.Part{Number: i + 1, ETag: etag})
			got, err := e.lfs.GetUpload(e.ctx, u.ID)
			require.NoError(t, err)
			require.Len(t, got.Done, i+1, "resumable: parts done so far")
		}
		obj, err := e.lfs.CompleteUpload(e.ctx, u.ID, parts)
		require.NoError(t, err)
		require.Equal(t, int64(len(content)), obj.Size)
		_, err = e.lfs.GetUpload(e.ctx, u.ID)
		require.ErrorIs(t, err, repo.ErrNotFound)

		// A corrupt multipart upload is refused and nothing is stored.
		wrong := sha256Hex([]byte("something else"))
		u2, err := e.lfs.CreateUpload(e.ctx, repo.UploadParams{OID: wrong, Size: 10, Multipart: true, PartSize: 10})
		require.NoError(t, err)
		etag, err := e.lfs.PutPart(e.ctx, u2.ID, 1, strings.NewReader("0123456789"), 10)
		require.NoError(t, err)
		_, err = e.lfs.CompleteUpload(e.ctx, u2.ID, []blob.Part{{Number: 1, ETag: etag}})
		require.ErrorIs(t, err, repo.ErrChecksumMismatch)
		_, err = e.lfs.Stat(e.ctx, wrong)
		require.ErrorIs(t, err, repo.ErrNotFound)

		// Uploads of objects stored meanwhile complete at once.
		u3, err := e.lfs.CreateUpload(e.ctx, repo.UploadParams{OID: oid, Size: int64(len(content)), Multipart: true, PartSize: 100})
		require.NoError(t, err)
		obj, err = e.lfs.CompleteUpload(e.ctx, u3.ID, nil)
		require.NoError(t, err)
		require.Equal(t, oid, obj.OID)

		// Single uploads and aborts.
		single, err := e.lfs.CreateUpload(e.ctx, repo.UploadParams{OID: oid, Size: 1})
		require.NoError(t, err)
		_, err = e.lfs.PutPart(e.ctx, single.ID, 1, strings.NewReader("x"), 1)
		require.ErrorIs(t, err, repo.ErrInvalid)
		_, err = e.lfs.CompleteUpload(e.ctx, single.ID, nil)
		require.ErrorIs(t, err, repo.ErrInvalid)
		require.NoError(t, e.lfs.AbortUpload(e.ctx, single.ID))
		require.ErrorIs(t, e.lfs.AbortUpload(e.ctx, single.ID), repo.ErrNotFound)
		_, err = e.lfs.CreateUpload(e.ctx, repo.UploadParams{OID: oid, Size: 10, Multipart: true})
		require.ErrorIs(t, err, repo.ErrInvalid)
		_, err = e.lfs.CreateUpload(e.ctx, repo.UploadParams{OID: oid, Size: 1 << 40, Multipart: true, PartSize: 1})
		require.ErrorIs(t, err, repo.ErrInvalid)
		_, err = e.lfs.CreateUpload(e.ctx, repo.UploadParams{OID: "x", Size: 1})
		require.ErrorIs(t, err, repo.ErrInvalid)

		// upload.expire removes uploads past their expiry.
		old, err := e.lfs.CreateUpload(e.ctx, repo.UploadParams{OID: wrong, Size: 10, Multipart: true, PartSize: 10, TTL: time.Nanosecond})
		require.NoError(t, err)
		time.Sleep(time.Millisecond)
		n, err := e.lfs.ExpireUploads(e.ctx)
		require.NoError(t, err)
		require.GreaterOrEqual(t, n, 1)
		count, err := e.d.NewSelect().Model((*db.Upload)(nil)).Where("id = ?", old.ID).Count(e.ctx)
		require.NoError(t, err)
		require.Zero(t, count)
		_, err = e.lfs.GetUpload(e.ctx, uuid.New())
		require.ErrorIs(t, err, repo.ErrNotFound)
	})
}

func TestPartSize(t *testing.T) {
	require.Equal(t, int64(16<<20), repo.PartSize(1))
	require.Equal(t, int64(16<<20), repo.PartSize(100<<30))
	// 5 TiB needs parts larger than 16 MiB to fit in 10,000 parts.
	ps := repo.PartSize(5 << 40)
	require.LessOrEqual(t, (int64(5<<40)+ps-1)/ps, int64(10000))
	require.Zero(t, ps%(1<<20))
}

func TestLFSRules(t *testing.T) {
	rules := repo.ParseLFSRules([]byte(strings.Join([]string{
		"# comment",
		"*.bin filter=lfs diff=lfs merge=lfs -text",
		"saved_model/**/* filter=lfs",
		"/root.dat filter=lfs",
		"docs/*.pdf filter=lfs",
		"keep.bin -filter",
		"*tfevents* filter=lfs",
		`"with space.txt" filter=lfs`,
		"data/[ab].csv filter=lfs",
		"!negated filter=lfs",
		"other.txt text",
		"",
	}, "\n")))
	for p, want := range map[string]bool{
		"model.bin": true, "a/b/model.bin": true, "keep.bin": false, "a/keep.bin": false,
		"saved_model/x/y.pb": true, "saved_model/y.pb": true, "x/saved_model/y.pb": false,
		"root.dat": true, "sub/root.dat": false, "docs/a.pdf": true, "docs/x/a.pdf": false,
		"events.out.tfevents.1": true, "with space.txt": true, "data/a.csv": true, "data/c.csv": false,
		"other.txt": false, "negated": false,
	} {
		require.Equal(t, want, rules.IsLFS(p), p)
	}
	defaults := repo.ParseLFSRules(repo.DefaultGitAttributes(model.RepoTypeModel))
	require.True(t, defaults.IsLFS("model.safetensors"))
	require.True(t, defaults.IsLFS("sub/pytorch_model.bin"))
	require.False(t, defaults.IsLFS("config.json"))
	var nilRules *repo.LFSRules
	require.False(t, nilRules.IsLFS("x.bin"))
}

// TestLFSUploadExpiredByTheBlobStore checks a multipart upload whose parts
// the Blob store dropped, as an S3 lifecycle rule that aborts incomplete
// multipart uploads does. The upload reads as not found, which tells a
// Pull or a client to start again, and the next plan starts a new upload.
func TestLFSUploadExpiredByTheBlobStore(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		r := e.create(repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "expired"}, Actor: alice})
		content := bytes.Repeat([]byte("0123456789"), 25)
		params := repo.UploadParams{OID: sha256Hex(content), Size: int64(len(content)), RepoUUID: r.UUID, UserID: alice.UserID,
			Multipart: true, PartSize: 100, TTL: 7 * 24 * time.Hour, Resume: true}
		u, err := e.lfs.CreateUpload(e.ctx, params)
		require.NoError(t, err)
		_, err = e.lfs.PutPart(e.ctx, u.ID, 1, bytes.NewReader(content[:100]), 100)
		require.NoError(t, err)

		require.Equal(t, 1, e.blobs.ExpireUploads())
		_, err = e.lfs.GetUpload(e.ctx, u.ID)
		require.ErrorIs(t, err, repo.ErrNotFound)
		_, err = e.lfs.GetUpload(e.ctx, u.ID)
		require.ErrorIs(t, err, repo.ErrNotFound, "the upload stays gone")

		next, err := e.lfs.CreateUpload(e.ctx, params)
		require.NoError(t, err)
		require.NotEqual(t, u.ID, next.ID, "the next plan starts a new upload")
		var parts []blob.Part
		for i := 0; i*100 < len(content); i++ {
			chunk := content[i*100 : min((i+1)*100, len(content))]
			etag, err := e.lfs.PutPart(e.ctx, next.ID, i+1, bytes.NewReader(chunk), int64(len(chunk)))
			require.NoError(t, err)
			parts = append(parts, blob.Part{Number: i + 1, ETag: etag})
		}
		obj, err := e.lfs.CompleteUpload(e.ctx, next.ID, parts)
		require.NoError(t, err)
		require.Equal(t, params.OID, obj.OID)
	})
}

// TestLFSResumeUpload checks UploadParams.Resume: planning the same object
// again continues the user's upload in the Repo, with its parts, while
// other users, Repos, part sizes and callers that do not ask get a new one.
func TestLFSResumeUpload(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice, bob := e.addUser("alice"), e.addUser("bob")
		r := e.create(repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "resume"}, Actor: alice})
		other := e.create(repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "other"}, Actor: alice})
		content := bytes.Repeat([]byte("0123456789"), 25)
		oid := sha256Hex(content)
		params := repo.UploadParams{OID: oid, Size: int64(len(content)), RepoUUID: r.UUID, UserID: alice.UserID, Multipart: true,
			PartSize: 100, TTL: time.Hour, Resume: true}
		u, err := e.lfs.CreateUpload(e.ctx, params)
		require.NoError(t, err)
		etag, err := e.lfs.PutPart(e.ctx, u.ID, 1, bytes.NewReader(content[:100]), 100)
		require.NoError(t, err)

		again, err := e.lfs.CreateUpload(e.ctx, params)
		require.NoError(t, err)
		require.Equal(t, u.ID, again.ID, "the plan continues the upload in progress")
		got, err := e.lfs.GetUpload(e.ctx, again.ID)
		require.NoError(t, err)
		require.Len(t, got.Done, 1, "with the part already sent")
		require.Equal(t, etag, got.Done[0].ETag)

		for name, p := range map[string]repo.UploadParams{
			"another user":      func() repo.UploadParams { q := params; q.UserID = bob.UserID; return q }(),
			"another Repo":      func() repo.UploadParams { q := params; q.RepoUUID = other.UUID; return q }(),
			"another part size": func() repo.UploadParams { q := params; q.PartSize = 125; return q }(),
			"no resume":         func() repo.UploadParams { q := params; q.Resume = false; return q }(),
		} {
			fresh, err := e.lfs.CreateUpload(e.ctx, p)
			require.NoError(t, err, name)
			require.NotEqual(t, u.ID, fresh.ID, name)
		}

		// Hangar's own uploads, with no User and no Repo, resume each other
		// and never a User's.
		system := repo.UploadParams{OID: oid, Size: int64(len(content)), Multipart: true, PartSize: 100, TTL: time.Hour, Resume: true}
		sys, err := e.lfs.CreateUpload(e.ctx, system)
		require.NoError(t, err)
		require.NotEqual(t, u.ID, sys.ID, "a system upload does not take over a User's")
		sysETag, err := e.lfs.PutPart(e.ctx, sys.ID, 1, bytes.NewReader(content[:100]), 100)
		require.NoError(t, err)
		sysAgain, err := e.lfs.CreateUpload(e.ctx, system)
		require.NoError(t, err)
		require.Equal(t, sys.ID, sysAgain.ID, "a retried transfer continues Hangar's upload")
		got, err = e.lfs.GetUpload(e.ctx, sysAgain.ID)
		require.NoError(t, err)
		require.Len(t, got.Done, 1)
		require.Equal(t, sysETag, got.Done[0].ETag)
		for name, p := range map[string]repo.UploadParams{
			"another part size": func() repo.UploadParams { q := system; q.PartSize = 125; return q }(),
			"no resume":         func() repo.UploadParams { q := system; q.Resume = false; return q }(),
			"a User only":       func() repo.UploadParams { q := system; q.UserID = alice.UserID; return q }(),
		} {
			fresh, err := e.lfs.CreateUpload(e.ctx, p)
			require.NoError(t, err, name)
			require.NotEqual(t, sys.ID, fresh.ID, name)
		}
		mine, err := e.lfs.CreateUpload(e.ctx, params)
		require.NoError(t, err)
		require.NotEqual(t, sys.ID, mine.ID, "a User's plan never resumes Hangar's upload")

		// A completed upload is gone, so the next plan starts a new one.
		var parts []blob.Part
		parts = append(parts, blob.Part{Number: 1, ETag: etag})
		for i := 1; i < 3; i++ {
			chunk := content[i*100 : min((i+1)*100, len(content))]
			etag, err := e.lfs.PutPart(e.ctx, u.ID, i+1, bytes.NewReader(chunk), int64(len(chunk)))
			require.NoError(t, err)
			parts = append(parts, blob.Part{Number: i + 1, ETag: etag})
		}
		_, err = e.lfs.CompleteUpload(e.ctx, u.ID, parts)
		require.NoError(t, err)
		next, err := e.lfs.CreateUpload(e.ctx, params)
		require.NoError(t, err)
		require.NotEqual(t, u.ID, next.ID)
	})
}
