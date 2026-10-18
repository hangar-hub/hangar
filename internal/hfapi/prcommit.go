package hfapi

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/community"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// defaultVerifyWait is lfs.verify_wait's default.
const defaultVerifyWait = 10 * time.Minute

// checkPRCommit runs the checks that repo.Service.Commit would fail on
// before a create_pr=1 commit opens its pull request, so a commit that
// cannot succeed leaves no pull request behind. It reads the base Commit
// the pull request ref is created from, applies the operations in order,
// and is never stricter than repo.Service: an operation it cannot judge
// from the base tree is left to the Commit.
func (h *Handler) checkPRCommit(ctx context.Context, rp *repo.Repo, base model.CommitSHA, ops []repo.Operation) error {
	if rp.Mirror {
		return fmt.Errorf("%w: %s is a Mirror; open pull requests on the Upstream", repo.ErrReadOnly, rp.ID.Path())
	}
	// written and removed are the paths earlier operations of this commit
	// added or deleted, which the base tree does not show.
	written := map[string]bool{}
	removed := map[string]bool{}
	for _, op := range ops {
		clean, err := model.CleanRepoPath(op.Path)
		if err != nil {
			return fmt.Errorf("%w: %w", repo.ErrInvalid, err)
		}
		switch op.Kind {
		case repo.OpAdd, repo.OpCopy:
			written[clean], removed[clean] = true, false
		case repo.OpAddLFS:
			if err := h.checkLFSObject(ctx, rp, op); err != nil {
				return err
			}
			written[clean], removed[clean] = true, false
		case repo.OpDelete:
			if err := h.checkDeleteFile(ctx, rp, base, clean, written, removed); err != nil {
				return err
			}
			written[clean], removed[clean] = false, true
		case repo.OpDeleteFolder:
			if err := h.checkDeleteFolder(ctx, rp, base, clean, written); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkDeleteFile fails when a deletedFile names a path that is neither in
// the base tree as a file nor written earlier in the commit.
func (h *Handler) checkDeleteFile(ctx context.Context, rp *repo.Repo, base model.CommitSHA, path string, written, removed map[string]bool) error {
	if written[path] {
		return nil
	}
	if removed[path] {
		return fmt.Errorf("%w: %s: %s", repo.ErrEntryNotFound, MsgFileMissing, path)
	}
	e, err := h.d.Repos.Entry(ctx, rp.ID, base, path)
	switch {
	case errors.Is(err, repo.ErrEntryNotFound):
		return fmt.Errorf("%w: %s: %s", repo.ErrEntryNotFound, MsgFileMissing, path)
	case err != nil:
		return err
	case e.Type == model.EntryDirectory:
		return fmt.Errorf("%w: %s is a folder; delete it as a folder", repo.ErrEntryNotFound, path)
	}
	return nil
}

// checkDeleteFolder fails when a deletedFolder names a folder that is not
// in the base tree and gets no file earlier in the commit. Deleting the
// root is left to the Commit.
func (h *Handler) checkDeleteFolder(ctx context.Context, rp *repo.Repo, base model.CommitSHA, path string, written map[string]bool) error {
	if path == "" {
		return nil
	}
	for p, ok := range written {
		if ok && strings.HasPrefix(p, path+"/") {
			return nil
		}
	}
	e, err := h.d.Repos.Entry(ctx, rp.ID, base, path)
	switch {
	case errors.Is(err, repo.ErrEntryNotFound):
		return fmt.Errorf("%w: folder %s does not exist", repo.ErrEntryNotFound, path)
	case err != nil:
		return err
	case e.Type != model.EntryDirectory:
		return fmt.Errorf("%w: folder %s does not exist", repo.ErrEntryNotFound, path)
	}
	return nil
}

// checkLFSObject fails when an lfsFile names an object that is not stored,
// that the caller may not reference without uploading it (Referable), or
// whose size differs. The caller's own upload through a presigned PUT that
// is not recorded yet is verified now, as the Commit would, waiting at
// most lfs.verify_wait.
func (h *Handler) checkLFSObject(ctx context.Context, rp *repo.Repo, op repo.Operation) error {
	p := httpx.Principal(ctx)
	referable, err := h.d.Repos.Referable(ctx, p, rp, []string{op.OID})
	if err != nil {
		return err
	}
	if !referable[op.OID] {
		if op.Size <= 0 {
			return fmt.Errorf("%w: %s (%s)", repo.ErrLFSObjectMissing, op.OID, op.Path)
		}
		if _, err := h.verifyLFS(ctx, op.OID, op.Size, p.UserID); err != nil {
			if errors.Is(err, repo.ErrNotFound) || errors.Is(err, repo.ErrChecksumMismatch) || errors.Is(err, repo.ErrInvalid) {
				return fmt.Errorf("%w: %s (%s)", repo.ErrLFSObjectMissing, op.OID, op.Path)
			}
			return err
		}
	}
	obj, err := h.d.LFS.Stat(ctx, op.OID)
	if errors.Is(err, repo.ErrNotFound) && op.Size > 0 {
		obj, err = h.verifyLFS(ctx, op.OID, op.Size, p.UserID)
	}
	switch {
	case err == nil:
		if op.Size != 0 && op.Size != obj.Size {
			return fmt.Errorf("%w: %s has size %d, not %d", repo.ErrInvalid, op.OID, obj.Size, op.Size)
		}
		return nil
	case errors.Is(err, repo.ErrNotFound), errors.Is(err, repo.ErrChecksumMismatch):
		return fmt.Errorf("%w: %s (%s)", repo.ErrLFSObjectMissing, op.OID, op.Path)
	default:
		return err
	}
}

// verifyLFS verifies an LFS object userID wrote through a presigned PUT.
// The verification carries on when the wait runs out, and concurrent
// requests share it, so a retry finds the object recorded instead of
// hashing it again.
func (h *Handler) verifyLFS(ctx context.Context, oid string, size int64, userID uuid.UUID) (repo.LFSObject, error) {
	ch := h.verifies.DoChan(oid+":"+strconv.FormatInt(size, 10)+":"+userID.String(), func() (any, error) {
		return h.d.LFS.Verify(context.WithoutCancel(ctx), oid, size, userID)
	})
	timer := time.NewTimer(h.d.Settings.VerifyWait)
	defer timer.Stop()
	select {
	case res := <-ch:
		if res.Err != nil {
			return repo.LFSObject{}, res.Err
		}
		return res.Val.(repo.LFSObject), nil
	case <-timer.C:
		return repo.LFSObject{}, fmt.Errorf("%w: %s (%d bytes) was not verified within %s; try again later",
			repo.ErrLFSObjectMissing, oid, size, h.d.Settings.VerifyWait)
	case <-ctx.Done():
		return repo.LFSObject{}, ctx.Err()
	}
}

// closeFailedPR removes a pull request that create_pr=1 opened when the
// Commit to its ref then failed, as huggingface.co never shows such a pull
// request. If it cannot be removed (someone commented on it meanwhile),
// it is closed with a comment instead.
func (h *Handler) closeFailedPR(ctx context.Context, rp *repo.Repo, d *community.Discussion, p model.Principal) {
	ctx = context.WithoutCancel(ctx)
	err := h.d.Community.Discard(ctx, rp.UUID, d.Num)
	if err == nil {
		return
	}
	if !errors.Is(err, community.ErrNotDiscardable) {
		h.log.WarnContext(ctx, "discard pull request after failed commit", "error", err, "repo", rp.ID.String(), "num", d.Num)
	}
	if _, err := h.d.Community.SetStatus(ctx, rp.UUID, d.Num, community.StatusClosed,
		"Closed automatically: the commit that opened this pull request failed, so it has no changes.", p); err != nil {
		h.log.ErrorContext(ctx, "close pull request after failed commit", "error", err, "repo", rp.ID.String(), "num", d.Num)
	}
}
