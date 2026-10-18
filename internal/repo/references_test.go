package repo_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// TestCommitsReferenceOnlyObjectsTheActorUploadedOrCanRead covers the
// decision James made on 2026-10-18 ("check at Commit"): LFS objects stay
// stored once per Instance, but a Commit or push may add a pointer to an
// object only when the actor uploaded its bytes, or can read a Repo that
// holds it. Someone who knows the SHA-256 and size of an object in a
// private Repo gets the same refusal as for an object that does not
// exist, and uploading the bytes stores no second copy.
func TestCommitsReferenceOnlyObjectsTheActorUploadedOrCanRead(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		// reader may read bob's private Repo, as a collaborator would.
		bob, mallory, reader := e.addUser("bob"), e.addUser("mallory"), e.addUser("reader")
		secretID := model.RepoID{Type: model.RepoTypeModel, Namespace: "bob", Name: "secret"}
		secretRepo := e.create(repo.CreateParams{ID: secretID, Actor: bob, Visibility: model.VisibilityPrivate})
		weights := []byte("private weights that only bob holds")
		oid := e.putLFS(weights, bob)
		size := int64(len(weights))
		e.commit(secretID, bob, repo.Operation{Kind: repo.OpAddLFS, Path: "model.safetensors", OID: oid, Size: size})

		malloryID := model.RepoID{Type: model.RepoTypeModel, Namespace: "mallory", Name: "grab"}
		malloryRepo := e.create(repo.CreateParams{ID: malloryID, Actor: mallory, InitialReadme: true})
		readerID := model.RepoID{Type: model.RepoTypeModel, Namespace: "reader", Name: "copy"}
		readerRepo := e.create(repo.CreateParams{ID: readerID, Actor: reader, InitialReadme: true})

		ok, err := e.svc.Referable(e.ctx, bob, secretRepo, []string{oid})
		require.NoError(t, err)
		require.True(t, ok[oid], "the Repo holds it")
		ok, err = e.svc.Referable(e.ctx, mallory, malloryRepo, []string{oid, "not-an-oid"})
		require.NoError(t, err)
		require.False(t, ok[oid])
		ok, err = e.svc.Referable(e.ctx, model.System(), malloryRepo, []string{oid})
		require.NoError(t, err)
		require.True(t, ok[oid], "Hangar itself may reference any object")
		ok, err = e.svc.Referable(e.ctx, reader, readerRepo, []string{oid})
		require.NoError(t, err)
		require.False(t, ok[oid], "without CanReadFiles only uploads and the Repo's own objects count")

		// A Commit through the Hub or native API: refused exactly as an
		// object nobody stored, with or without a size.
		unknown := sha256Hex([]byte("nobody uploaded this"))
		refusal := func(actor model.Principal, id model.RepoID, o string, sz int64) string {
			t.Helper()
			_, err := e.svc.Commit(e.ctx, repo.CommitParams{Repo: id, Summary: "grab", Actor: actor,
				Operations: []repo.Operation{{Kind: repo.OpAddLFS, Path: "w.safetensors", OID: o, Size: sz}}})
			require.ErrorIs(t, err, repo.ErrLFSObjectMissing)
			return strings.ReplaceAll(err.Error(), o, "<oid>")
		}
		for _, sz := range []int64{size, 0, size + 1} {
			require.Equal(t, refusal(mallory, malloryID, unknown, sz), refusal(mallory, malloryID, oid, sz),
				"the refusal for a stored object says no more than for a missing one (size %d)", sz)
		}

		// A push of a pointer: the same refusal as for a missing object.
		push := func(actor model.Principal, id model.RepoID) error {
			t.Helper()
			base, err := e.svc.Resolve(e.ctx, id, "main")
			require.NoError(t, err)
			w := e.clone(id)
			w.write("pointer.bin", string(model.LFSPointer{OID: oid, Size: size}.Bytes()))
			tip := w.commit("pointer")
			return e.svc.ApplyPack(e.ctx, repo.ApplyPackParams{Repo: id, Source: repo.SourcePush, Actor: actor,
				Pack: bytes.NewReader(w.pack(tip, base)), Updates: []repo.RefUpdate{{Name: "refs/heads/main", Old: base, New: tip}}})
		}
		var pe *repo.PolicyError
		require.ErrorAs(t, push(mallory, malloryID), &pe)
		require.Contains(t, pe.Message, "the push refers to LFS object "+oid+" (35 bytes), which has not been uploaded")
		_, err = e.svc.Entry(e.ctx, malloryID, "main", "pointer.bin")
		require.Error(t, err, "nothing was pushed")

		// A User who can read a Repo that holds the object may reference
		// it without uploading it.
		readable, err := repo.NewStore(withOpts(e.opts, func(o *repo.Options) {
			o.CanReadFiles = func(_ context.Context, p model.Principal, r *repo.Repo) (bool, error) {
				return r.UUID == secretRepo.UUID && (p.UserID == bob.UserID || p.UserID == reader.UserID), nil
			}
		}))
		require.NoError(t, err)
		ok, err = readable.Referable(e.ctx, reader, readerRepo, []string{oid})
		require.NoError(t, err)
		require.True(t, ok[oid])
		ok, err = readable.Referable(e.ctx, mallory, malloryRepo, []string{oid})
		require.NoError(t, err)
		require.False(t, ok[oid])
		_, err = readable.Commit(e.ctx, repo.CommitParams{Repo: readerID, Summary: "copy", Actor: reader,
			Operations: []repo.Operation{{Kind: repo.OpAddLFS, Path: "w.safetensors", OID: oid, Size: size}}})
		require.NoError(t, err)
		require.Equal(t, string(weights), e.read(readerID, "main", "w.safetensors"))

		// A failing read check fails the Commit rather than allowing it.
		failing, err := repo.NewStore(withOpts(e.opts, func(o *repo.Options) {
			o.CanReadFiles = func(context.Context, model.Principal, *repo.Repo) (bool, error) {
				return false, errors.New("authz unavailable")
			}
		}))
		require.NoError(t, err)
		_, err = failing.Commit(e.ctx, repo.CommitParams{Repo: malloryID, Summary: "grab", Actor: mallory,
			Operations: []repo.Operation{{Kind: repo.OpAddLFS, Path: "w.safetensors", OID: oid, Size: size}}})
		require.ErrorContains(t, err, "authz unavailable")

		// Uploading the bytes makes the object mallory's to reference, and
		// stores no second copy.
		puts := e.blobs.Calls("Put") + e.blobs.Calls("Copy")
		obj, err := e.lfs.Ingest(e.ctx, oid, size, bytes.NewReader(weights))
		require.NoError(t, err)
		require.NoError(t, e.lfs.RecordUploader(e.ctx, obj.OID, mallory.UserID))
		require.Equal(t, puts, e.blobs.Calls("Put")+e.blobs.Calls("Copy"), "the bytes are checked, not stored again")
		require.NoError(t, push(mallory, malloryID))
		require.Equal(t, string(weights), e.read(malloryID, "main", "pointer.bin"))
		e.commit(malloryID, mallory, repo.Operation{Kind: repo.OpAddLFS, Path: "again.safetensors", OID: oid})

		// Bytes sent in a Commit, for a path .gitattributes stores in LFS,
		// count as an upload by its author.
		inline := []byte("weights sent inline in a commit")
		e.commit(readerID, reader, repo.Operation{Kind: repo.OpAdd, Path: "inline.safetensors", Content: bytes.NewReader(inline)})
		up, err := e.lfs.Uploaded(e.ctx, reader.UserID, []string{sha256Hex(inline)})
		require.NoError(t, err)
		require.True(t, up[sha256Hex(inline)])
	})
}

// TestUploadsOfStoredObjectsProveTheBytes checks every upload path of the
// LFS store when the object is already stored: a presigned PUT and a
// multipart upload are checked against the SHA-256 and size, record the
// User as an uploader when they match, and are discarded either way, so
// the stored object is never written twice and other bytes prove nothing.
func TestUploadsOfStoredObjectsProveTheBytes(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		servePresigned(t, e)
		owner, viaPut, viaParts, liar := e.addUser("owner"), e.addUser("viaput"), e.addUser("viaparts"), e.addUser("liar")
		content := bytes.Repeat([]byte("stored once "), 4)
		oid := e.putLFS(content, owner)
		size := int64(len(content))
		uploaded := func(p model.Principal) bool {
			t.Helper()
			got, err := e.lfs.Uploaded(e.ctx, p.UserID, []string{oid})
			require.NoError(t, err)
			return got[oid]
		}
		require.True(t, uploaded(owner))
		writes := func() int { return e.blobs.Calls("Copy") + e.blobs.Calls("CompleteMultipart") }

		// A presigned PUT of other bytes proves nothing.
		req, err := e.lfs.PresignPut(e.ctx, oid, size, time.Hour, liar.UserID)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, putURL(t, req, bytes.Repeat([]byte("x"), int(size)), false))
		_, err = e.lfs.Verify(e.ctx, oid, size, liar.UserID)
		require.ErrorIs(t, err, repo.ErrChecksumMismatch)
		require.False(t, uploaded(liar))
		_, err = e.lfs.Verify(e.ctx, oid, size, liar.UserID)
		require.ErrorIs(t, err, repo.ErrNotFound, "with nothing staged, a stored object is not found")

		// The right bytes through a presigned PUT: recorded, not copied.
		before := writes()
		req, err = e.lfs.PresignPut(e.ctx, oid, size, time.Hour, viaPut.UserID)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, putURL(t, req, content, false))
		_, err = e.lfs.Verify(e.ctx, oid, size, owner.UserID)
		require.ErrorIs(t, err, repo.ErrNotFound, "another User's staged upload proves nothing for owner's request")
		obj, err := e.lfs.Verify(e.ctx, oid, size, viaPut.UserID)
		require.NoError(t, err)
		require.True(t, obj.Verified)
		require.True(t, uploaded(viaPut))
		require.Equal(t, before, writes(), "the staged bytes are not copied over the stored object")
		require.Empty(t, stagedUploads(t, e, oid), "and are discarded")

		// The right bytes through a multipart upload: recorded, not stored
		// again; the wrong ones are refused.
		multipart := func(p model.Principal, body []byte) error {
			t.Helper()
			up, err := e.lfs.CreateUpload(e.ctx, repo.UploadParams{OID: oid, Size: size, Multipart: true, PartSize: size, UserID: p.UserID})
			require.NoError(t, err)
			etag, err := e.lfs.PutPart(e.ctx, up.ID, 1, bytes.NewReader(body), int64(len(body)))
			require.NoError(t, err)
			_, err = e.lfs.CompleteUpload(e.ctx, up.ID, []blob.Part{{Number: 1, ETag: etag, Size: int64(len(body))}})
			return err
		}
		require.ErrorIs(t, multipart(liar, bytes.Repeat([]byte("y"), int(size))), repo.ErrChecksumMismatch)
		require.False(t, uploaded(liar))
		copies := e.blobs.Calls("Copy")
		require.NoError(t, multipart(viaParts, content))
		require.True(t, uploaded(viaParts))
		require.Equal(t, copies, e.blobs.Calls("Copy"), "the assembled parts are not copied over the stored object")
		stored, ok := e.blobs.Bytes(model.LFSObjectKey(oid))
		require.True(t, ok)
		require.Equal(t, content, stored)
		// An upload by a User who already uploaded the object completes
		// without checking the parts again.
		require.NoError(t, multipart(viaParts, content))
	})
}
