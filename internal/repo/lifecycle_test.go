package repo_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/webhook"
)

func TestCreateAndSettings(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "Tiny-Model"}
		r := e.create(repo.CreateParams{ID: id, Visibility: model.VisibilityPrivate, Description: "d", Licence: "mit", Actor: alice, InitialReadme: true})
		require.Equal(t, id, r.ID)
		require.Equal(t, model.VisibilityPrivate, r.Visibility)
		require.Equal(t, "main", r.DefaultBranch)
		require.Equal(t, "refs/heads/main", r.HeadRef)
		require.Equal(t, "mit", r.Licence)
		require.Equal(t, alice.UserID, r.CreatedBy)
		require.Equal(t, alice.NamespaceID, r.NamespaceID)
		require.Positive(t, r.SizeBytes)
		require.Contains(t, e.read(id, "", "README.md"), "license: mit")
		require.Equal(t, string(repo.DefaultGitAttributes(model.RepoTypeModel)), e.read(id, "", ".gitattributes"))

		// Lookups ignore case; a dataset may share the name.
		got, err := e.svc.Get(e.ctx, model.RepoID{Type: model.RepoTypeModel, Namespace: "ALICE", Name: "tiny-model"})
		require.NoError(t, err)
		require.Equal(t, r.UUID, got.UUID)
		ds := e.create(repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeDataset, Namespace: "alice", Name: "Tiny-Model"}, Actor: alice})
		require.NotEqual(t, r.UUID, ds.UUID)
		require.Equal(t, string(repo.DefaultGitAttributes(model.RepoTypeDataset)), e.read(ds.ID, "", ".gitattributes"))

		_, err = e.svc.Create(e.ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "TINY-model"}, Actor: alice})
		require.ErrorIs(t, err, repo.ErrExists)
		_, err = e.svc.Create(e.ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "nobody", Name: "x"}, Actor: alice})
		require.ErrorIs(t, err, repo.ErrInvalid)
		_, err = e.svc.Create(e.ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "bad--name"}, Actor: alice})
		require.ErrorIs(t, err, repo.ErrInvalid)
		_, err = e.svc.Create(e.ctx, repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "v"}, Visibility: "secret", Actor: alice})
		require.ErrorIs(t, err, repo.ErrInvalid)
		_, err = e.svc.Get(e.ctx, model.RepoID{Type: "space", Namespace: "alice", Name: "x"})
		require.ErrorIs(t, err, repo.ErrNotFound)

		byUUID, err := e.svc.GetByUUID(e.ctx, r.UUID)
		require.NoError(t, err)
		require.Equal(t, r.ID, byUUID.ID)

		// Settings (REPO-9).
		e.commit(id, alice, add("x.txt", "x"))
		require.NoError(t, e.svc.CreateBranch(e.ctx, id, "dev", "main", alice))
		pub := model.VisibilityPublic
		desc, lic, branch := "new", "apache-2.0", "dev"
		tags := []string{"llm", "tiny"}
		gating := repo.Gating{Mode: "manual", Prompt: "Why?", Fields: []repo.GatingField{{Name: "reason", Type: "text", Required: true}}}
		up, err := e.svc.UpdateSettings(e.ctx, id, repo.SettingsUpdate{Visibility: &pub, Description: &desc, Licence: &lic,
			DefaultBranch: &branch, Tags: &tags, Gating: &gating}, alice)
		require.NoError(t, err)
		require.Equal(t, model.VisibilityPublic, up.Visibility)
		require.Equal(t, "new", up.Description)
		require.Equal(t, "apache-2.0", up.Licence)
		require.Equal(t, "dev", up.DefaultBranch)
		require.Equal(t, "refs/heads/dev", up.HeadRef)
		require.Equal(t, tags, up.Tags)
		require.Equal(t, gating, up.Gating)
		require.True(t, up.Gating.Enabled())
		empty := ""
		up, err = e.svc.UpdateSettings(e.ctx, id, repo.SettingsUpdate{Licence: &empty}, alice)
		require.NoError(t, err)
		require.Empty(t, up.Licence)
		nope := "nope"
		_, err = e.svc.UpdateSettings(e.ctx, id, repo.SettingsUpdate{DefaultBranch: &nope}, alice)
		require.ErrorIs(t, err, repo.ErrRevisionNotFound)
		bad := model.Visibility("x")
		_, err = e.svc.UpdateSettings(e.ctx, id, repo.SettingsUpdate{Visibility: &bad}, alice)
		require.ErrorIs(t, err, repo.ErrInvalid)
		_, err = e.svc.UpdateSettings(e.ctx, id, repo.SettingsUpdate{Gating: &repo.Gating{Mode: "sometimes"}}, alice)
		require.ErrorIs(t, err, repo.ErrInvalid)

		require.Contains(t, e.audit.Actions(), audit.RepoCreated)
		require.Contains(t, e.audit.Actions(), audit.RepoVisibilityChanged)
		require.Contains(t, e.hooks.Names(), webhook.RepoCreated)
		require.Contains(t, e.hooks.Names(), webhook.RepoVisibilityChanged)
	})
}

// TestRenameAndTransferRedirect checks REPO-4: the old ID keeps working.
func TestRenameAndTransferRedirect(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		e.addUser("bob")
		old := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "first"}
		r := e.create(repo.CreateParams{ID: old, Actor: alice})

		renamed, err := e.svc.Rename(e.ctx, old, "second", alice)
		require.NoError(t, err)
		require.Equal(t, "second", renamed.ID.Name)
		got, err := e.svc.Get(e.ctx, old)
		require.NoError(t, err)
		require.Equal(t, renamed.ID, got.ID, "the old ID redirects to the canonical ID")

		moved, err := e.svc.Transfer(e.ctx, renamed.ID, "bob", alice)
		require.NoError(t, err)
		require.Equal(t, model.RepoID{Type: model.RepoTypeModel, Namespace: "bob", Name: "second"}, moved.ID)

		// Both moves are audited with the old and new IDs (EVT-2).
		renames := e.auditEvents(audit.RepoRenamed)
		require.Len(t, renames, 1)
		require.Equal(t, alice.UserID, renames[0].Actor.UserID)
		require.Equal(t, "repo", renames[0].TargetType)
		require.Equal(t, "model:alice/second", renames[0].TargetID)
		require.Equal(t, map[string]any{"from": "alice/first", "to": "alice/second"}, renames[0].Data)
		transfers := e.auditEvents(audit.RepoTransferred)
		require.Len(t, transfers, 1)
		require.Equal(t, alice.UserID, transfers[0].Actor.UserID)
		require.Equal(t, "model:bob/second", transfers[0].TargetID)
		require.Equal(t, map[string]any{"from": "alice/second", "to": "bob/second"}, transfers[0].Data)
		for _, id := range []model.RepoID{old, renamed.ID, {Type: model.RepoTypeModel, Namespace: "BOB", Name: "SECOND"}} {
			got, err := e.svc.Get(e.ctx, id)
			require.NoError(t, err, id.String())
			require.Equal(t, r.UUID, got.UUID)
			require.Equal(t, moved.ID, got.ID)
		}
		// Git data follows the Repo.
		require.Equal(t, string(repo.DefaultGitAttributes(model.RepoTypeModel)), e.read(old, "", ".gitattributes"))

		// A new Repo at an old ID replaces the redirect.
		fresh := e.create(repo.CreateParams{ID: old, Actor: alice})
		got, err = e.svc.Get(e.ctx, old)
		require.NoError(t, err)
		require.Equal(t, fresh.UUID, got.UUID)

		_, err = e.svc.Rename(e.ctx, moved.ID, "bad..name", alice)
		require.ErrorIs(t, err, repo.ErrInvalid)
		_, err = e.svc.Transfer(e.ctx, moved.ID, "nobody", alice)
		require.ErrorIs(t, err, repo.ErrInvalid)
		e.create(repo.CreateParams{ID: model.RepoID{Type: model.RepoTypeModel, Namespace: "bob", Name: "first"}, Actor: alice})
		_, err = e.svc.Transfer(e.ctx, old, "bob", alice)
		require.ErrorIs(t, err, repo.ErrExists)
		require.Len(t, e.auditEvents(audit.RepoRenamed), 1, "a refused rename is not audited")
		require.Len(t, e.auditEvents(audit.RepoTransferred), 1, "a refused transfer is not audited")
		// A change of case keeps the same ID working.
		cased, err := e.svc.Rename(e.ctx, old, "FIRST", alice)
		require.NoError(t, err)
		require.Equal(t, "FIRST", cased.ID.Name)
		got, err = e.svc.Get(e.ctx, old)
		require.NoError(t, err)
		require.Equal(t, fresh.UUID, got.UUID)
	})
}

// TestDeleteRestoreAndPurge checks REPO-5.
func TestDeleteRestoreAndPurge(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		require.NoError(t, e.svc.RegisterJobs(e.reg, e.lfs, repo.JobOptions{Blobs: e.blobs}))
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "doomed"}
		r := e.create(repo.CreateParams{ID: id, Actor: alice})
		weights := []byte("weights")
		oid := e.putLFS(weights)
		e.commit(id, alice, repo.Operation{Kind: repo.OpAddLFS, Path: "w.bin", OID: oid, Size: int64(len(weights))})

		require.NoError(t, e.svc.Delete(e.ctx, id, alice))
		_, err := e.svc.Get(e.ctx, id)
		require.ErrorIs(t, err, repo.ErrNotFound)
		deleted, err := e.svc.GetByUUID(e.ctx, r.UUID)
		require.NoError(t, err)
		require.NotNil(t, deleted.DeletedAt)
		require.ErrorIs(t, e.svc.Delete(e.ctx, id, alice), repo.ErrNotFound)

		restored, err := e.svc.Restore(e.ctx, id, alice)
		require.NoError(t, err)
		require.Nil(t, restored.DeletedAt)
		require.Equal(t, "weights", e.read(id, "", "w.bin"))
		_, err = e.svc.Restore(e.ctx, id, alice)
		require.ErrorIs(t, err, repo.ErrNotFound)

		// The first deletion's purge Job does nothing once restored.
		e.clock.Add(time.Minute)
		require.NoError(t, e.svc.Delete(e.ctx, id, alice))
		e.clock.Add(8 * 24 * time.Hour)
		_, err = e.svc.Restore(e.ctx, id, alice)
		require.ErrorIs(t, err, repo.ErrNotFound, "past the restore window")

		// A new Repo can take the name while the old one waits for its purge.
		e.create(repo.CreateParams{ID: id, Actor: alice})
		e.jobs.SetClock(e.clock.Now)
		require.Positive(t, e.jobs.RunPending(e.ctx))
		js, _, err := e.jobs.List(e.ctx, jobs.Filter{Kinds: []jobs.Kind{jobs.KindRepoPurge}})
		require.NoError(t, err)
		require.Len(t, js, 2)
		for _, j := range js {
			require.Equal(t, jobs.StatusSucceeded, j.Status, j.Error)
		}
		_, err = e.svc.GetByUUID(e.ctx, r.UUID)
		require.ErrorIs(t, err, repo.ErrNotFound)
		for _, k := range e.blobs.Keys() {
			require.False(t, strings.HasPrefix(k, "git/"+r.UUID.String()), "git data is purged: %s", k)
		}
		_, ok := e.blobs.Bytes("lfs/sha256/" + oid[:2] + "/" + oid[2:4] + "/" + oid)
		require.True(t, ok, "LFS objects are left for garbage collection")
		require.Contains(t, e.audit.Actions(), audit.RepoPurged)
		require.Contains(t, e.audit.Actions(), audit.RepoRestored)

		require.ErrorIs(t, e.svc.Purge(e.ctx, r.UUID), repo.ErrNotFound)
		live, err := e.svc.Get(e.ctx, id)
		require.NoError(t, err)
		require.ErrorIs(t, e.svc.Purge(e.ctx, live.UUID), repo.ErrInvalid)

		// Restoring over a reused ID is refused.
		require.NoError(t, e.svc.Delete(e.ctx, id, alice))
		e.create(repo.CreateParams{ID: id, Actor: alice})
		_, err = e.svc.Restore(e.ctx, id, alice)
		require.ErrorIs(t, err, repo.ErrExists)
	})
}

// TestPartialMirrorPresence checks Partial mirror presence flags (PULL-3).
func TestPartialMirrorPresence(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "mirror"}
		r := e.create(repo.CreateParams{ID: id, Actor: alice, Mirror: true})
		require.True(t, r.Mirror)
		refs, err := e.svc.Refs(e.ctx, id)
		require.NoError(t, err)
		require.Empty(t, refs.Branches, "a Mirror starts empty")

		have := []byte("quant q4")
		haveOID := e.putLFS(have)
		missing := []byte("quant q8, not pulled")
		missingOID := sha256Hex(missing)
		ops := []repo.Operation{
			{Kind: repo.OpAddLFS, Path: "m-Q4.gguf", OID: haveOID, Size: int64(len(have))},
			{Kind: repo.OpAddLFS, Path: "m-Q8.gguf", OID: missingOID, Size: int64(len(missing))},
		}
		_, err = e.svc.Commit(e.ctx, repo.CommitParams{Repo: id, Summary: "mirror", Operations: ops, Actor: alice})
		require.ErrorIs(t, err, repo.ErrReadOnly)
		res, err := e.svc.Commit(e.ctx, repo.CommitParams{Repo: id, Summary: "mirror", Operations: ops, Actor: model.System(), AllowMirror: true})
		require.NoError(t, err)

		q4, err := e.svc.Entry(e.ctx, id, res.SHA, "m-Q4.gguf")
		require.NoError(t, err)
		require.True(t, q4.Present)
		q8, err := e.svc.Entry(e.ctx, id, res.SHA, "m-Q8.gguf")
		require.NoError(t, err)
		require.False(t, q8.Present)
		require.Equal(t, int64(len(missing)), q8.Size)
		_, _, err = e.svc.OpenFile(e.ctx, id, res.SHA, "m-Q8.gguf", nil)
		require.ErrorIs(t, err, repo.ErrNotPresent)
		lrefs, err := e.svc.LFSRefs(e.ctx, id, []string{haveOID, missingOID, sha256Hex([]byte("other"))})
		require.NoError(t, err)
		require.Len(t, lrefs, 2)
		require.True(t, lrefs[haveOID].Present)
		require.False(t, lrefs[missingOID].Present)

		// A Pull fetches the file later.
		e.putLFS(missing)
		require.NoError(t, e.svc.SetLFSPresence(e.ctx, id, []repo.LFSRef{{OID: missingOID, Size: int64(len(missing)), Present: true}}))
		q8, err = e.svc.Entry(e.ctx, id, res.SHA, "m-Q8.gguf")
		require.NoError(t, err)
		require.True(t, q8.Present)
		require.Equal(t, string(missing), e.read(id, "", "m-Q8.gguf"))
		got, err := e.svc.Get(e.ctx, id)
		require.NoError(t, err)
		require.Equal(t, int64(len(have)+len(missing)), got.LFSBytes)
		require.ErrorIs(t, e.svc.SetLFSPresence(e.ctx, id, []repo.LFSRef{{OID: "bad"}}), repo.ErrInvalid)

		// Mirrors refuse branch and tag changes until detached.
		require.ErrorIs(t, e.svc.CreateBranch(e.ctx, id, "dev", "main", alice), repo.ErrReadOnly)
		require.NoError(t, e.svc.SetMirrorState(e.ctx, id, false))
		require.NoError(t, e.svc.CreateBranch(e.ctx, id, "dev", "main", alice))
		require.NoError(t, e.svc.SetMirrorState(e.ctx, id, true))
		got, err = e.svc.Get(e.ctx, id)
		require.NoError(t, err)
		require.True(t, got.Mirror)
	})
}

func TestBranchesAndTags(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		alice := e.addUser("alice")
		id := model.RepoID{Type: model.RepoTypeModel, Namespace: "alice", Name: "refs"}
		e.create(repo.CreateParams{ID: id, Actor: alice})
		first := e.commit(id, alice, add("a", "a"))
		require.NoError(t, e.svc.CreateBranch(e.ctx, id, "dev", first.SHA.String(), alice))
		require.ErrorIs(t, e.svc.CreateBranch(e.ctx, id, "dev", "main", alice), repo.ErrRefExists)
		require.ErrorIs(t, e.svc.CreateBranch(e.ctx, id, "bad..name", "main", alice), repo.ErrInvalid)
		require.ErrorIs(t, e.svc.CreateBranch(e.ctx, id, "x", "nope", alice), repo.ErrRevisionNotFound)
		require.NoError(t, e.svc.CreateTag(e.ctx, repo.TagParams{Repo: id, Tag: "v1", Revision: "dev", Actor: alice}))
		require.NoError(t, e.svc.CreateTag(e.ctx, repo.TagParams{Repo: id, Tag: "v2", Revision: "main", Message: "Release 2", Actor: alice}))
		require.ErrorIs(t, e.svc.CreateTag(e.ctx, repo.TagParams{Repo: id, Tag: "v2", Revision: "main", Actor: alice}), repo.ErrRefExists)
		second := e.commit(id, alice, add("b", "b"))

		refs, err := e.svc.Refs(e.ctx, id)
		require.NoError(t, err)
		require.Equal(t, []repo.Ref{{Name: "refs/heads/dev", Target: first.SHA}, {Name: "refs/heads/main", Target: second.SHA}}, refs.Branches)
		require.Equal(t, []repo.Ref{{Name: "refs/tags/v1", Target: first.SHA}, {Name: "refs/tags/v2", Target: first.SHA}}, refs.Tags, "annotated tags are peeled")

		// Branches win over tags; full ref names and SHAs resolve.
		require.NoError(t, e.svc.CreateBranch(e.ctx, id, "v1", "main", alice))
		for rev, want := range map[string]model.CommitSHA{"v1": second.SHA, "refs/tags/v1": first.SHA, "v2": first.SHA,
			strings.ToUpper(first.SHA.String()): first.SHA, "": second.SHA} {
			got, err := e.svc.Resolve(e.ctx, id, rev)
			require.NoError(t, err, rev)
			require.Equal(t, want, got, rev)
		}
		for _, rev := range []string{"nope", strings.Repeat("a", 40), "refs/heads/../x"} {
			_, err := e.svc.Resolve(e.ctx, id, rev)
			require.ErrorIs(t, err, repo.ErrRevisionNotFound, rev)
		}

		require.ErrorIs(t, e.svc.DeleteBranch(e.ctx, id, "main", alice), repo.ErrProtectedRef)
		require.ErrorIs(t, e.svc.DeleteBranch(e.ctx, id, "nope", alice), repo.ErrRevisionNotFound)
		require.NoError(t, e.svc.DeleteBranch(e.ctx, id, "dev", alice))
		require.NoError(t, e.svc.DeleteTag(e.ctx, id, "v2", alice))
		require.ErrorIs(t, e.svc.DeleteTag(e.ctx, id, "v2", alice), repo.ErrRevisionNotFound)

		// UpdateRefs validates its targets.
		require.ErrorIs(t, e.svc.UpdateRefs(e.ctx, id, []repo.RefUpdate{{Name: "refs/heads/x", New: model.CommitSHA(strings.Repeat("b", 40))}}), repo.ErrRevisionNotFound)
		require.ErrorIs(t, e.svc.UpdateRefs(e.ctx, id, []repo.RefUpdate{{Name: "refs/heads/x", New: "short"}}), repo.ErrInvalid)
		require.ErrorIs(t, e.svc.UpdateRefs(e.ctx, id, []repo.RefUpdate{{Name: "bad", New: first.SHA}}), repo.ErrInvalid)
		require.NoError(t, e.svc.UpdateRefs(e.ctx, id, nil))
		require.NoError(t, e.svc.UpdateRefs(e.ctx, id, []repo.RefUpdate{
			{Name: "refs/hangar/previous/1", New: first.SHA}, {Name: "refs/heads/v1", Old: second.SHA, New: first.SHA}}))
		refs, err = e.svc.Refs(e.ctx, id)
		require.NoError(t, err)
		require.Equal(t, []repo.Ref{{Name: "refs/hangar/previous/1", Target: first.SHA}}, refs.Other)
		// All or nothing: one stale update fails both.
		err = e.svc.UpdateRefs(e.ctx, id, []repo.RefUpdate{{Name: "refs/heads/a", New: first.SHA}, {Name: "refs/heads/v1", Old: second.SHA, New: first.SHA}})
		require.ErrorIs(t, err, repo.ErrRefConflict)
		_, err = e.svc.Resolve(e.ctx, id, "a")
		require.ErrorIs(t, err, repo.ErrRevisionNotFound)
	})
}
