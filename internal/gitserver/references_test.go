package gitserver_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/repo/repotest"
)

// TestLFSBatchAsksForObjectsTheCallerCannotUse covers the decision James
// made on 2026-10-18 ("check at Commit") on the git-lfs API: an object
// stored in a Repo the caller cannot read is answered as if it were
// missing, with upload actions and no word of its size; verify does not
// accept it until the caller has sent its bytes; the upload stores no
// second copy; and a push of a pointer to it is refused until then. A
// caller who can read the Repo that holds it is told it exists, as before.
func TestLFSBatchAsksForObjectsTheCallerCannotUse(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.repos.CheckLFSReferences = true
	f.repos.CanReadFiles = repotest.CanReadFilesWith(f.authz)
	ctx := context.Background()

	// alice's private Repo holds an object; bob cannot read it, carol can.
	content, oid := randomContent(t, 4000)
	_, err := f.repos.LFS.Put(ctx, content)
	require.NoError(t, err)
	require.NoError(t, f.repos.LFS.RecordUploader(ctx, oid, alice.UserID))
	_, err = f.repos.Commit(ctx, repo.CommitParams{Repo: privateID, Summary: "weights", Actor: alice,
		Operations: []repo.Operation{{Kind: repo.OpAddLFS, Path: "w.bin", OID: oid, Size: int64(len(content))}}})
	require.NoError(t, err)
	bobID := model.RepoID{Type: model.RepoTypeModel, Namespace: "bob", Name: "grab"}
	_, err = f.repos.Create(ctx, repo.CreateParams{ID: bobID, Visibility: model.VisibilityPublic, Actor: bob, InitialReadme: true})
	require.NoError(t, err)
	_, unknown := randomContent(t, 10)

	// bob: the stored object and an unknown one get the same answer, for
	// the right size and a wrong one.
	for _, size := range []int{len(content), len(content) + 1} {
		_, b := f.batch(t, bobID, tokBob, batchReq("upload", []string{"basic"}, obj(oid, size), obj(unknown, size)))
		require.Len(t, b.Objects, 2)
		for _, o := range b.Objects {
			require.Nil(t, o.Error, "size %d", size)
			require.Contains(t, o.Actions, "upload", "size %d: the object must be uploaded", size)
			require.Contains(t, o.Actions, "verify")
		}
	}
	// carol can read alice's private Repo, so the object needs no upload,
	// and a wrong size is reported with the stored one.
	_, b := f.batch(t, publicID, tokCarol, batchReq("upload", []string{"basic"}, obj(oid, len(content))))
	require.Empty(t, b.Objects[0].Actions, "deduplicated")
	_, b = f.batch(t, publicID, tokCarol, batchReq("upload", []string{"basic"}, obj(oid, 1)))
	require.Contains(t, b.Objects[0].Error.Message, "the stored object has 4000 bytes")

	// bob's verify before an upload: not found, as for a missing object.
	_, b = f.batch(t, bobID, tokBob, batchReq("upload", []string{"basic"}, obj(oid, len(content))))
	up, verify := b.Objects[0].Actions["upload"], b.Objects[0].Actions["verify"]
	require.Equal(t, http.StatusNotFound, call(t, http.MethodPost, verify, verifyBody(oid, len(content))).StatusCode)

	// bob's push of a pointer without the upload is refused.
	dir := f.clone(t, bobID, tokBob)
	writeFile(t, dir, "w.bin", model.LFSPointer{OID: oid, Size: int64(len(content))}.Bytes())
	f.git.Must(t, dir, "add", "-A")
	f.git.Must(t, dir, "commit", "-q", "-m", "pointer")
	out, err := f.git.Run(ctx, dir, "push", "origin", "main")
	require.Error(t, err)
	require.Contains(t, out, "which has not been uploaded")

	// Other bytes under the SHA-256 prove nothing.
	wrong := append([]byte{}, content...)
	wrong[0] ^= 1
	require.Equal(t, http.StatusUnprocessableEntity, call(t, http.MethodPut, up, wrong).StatusCode)
	require.Equal(t, http.StatusNotFound, call(t, http.MethodPost, verify, verifyBody(oid, len(content))).StatusCode)

	// The real bytes: checked, not stored again, then verify and the push
	// succeed.
	puts := f.blobs.Calls("Put") + f.blobs.Calls("Copy")
	require.Equal(t, http.StatusOK, call(t, http.MethodPut, up, content).StatusCode)
	require.Equal(t, puts, f.blobs.Calls("Put")+f.blobs.Calls("Copy"), "no second copy is stored")
	require.Equal(t, http.StatusOK, call(t, http.MethodPost, verify, verifyBody(oid, len(content))).StatusCode)
	f.git.Must(t, dir, "push", "-q", "origin", "main")
	_, b = f.batch(t, bobID, tokBob, batchReq("upload", []string{"basic"}, obj(oid, len(content))))
	require.Empty(t, b.Objects[0].Actions, "bob uploaded it, so it is deduplicated for him now")
}
