package hfapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/community"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/repo"
)

// likers answers GET /api/{type}s/{repo}/likers (list_repo_likers): the
// users who like a Repo the caller can read, in name order, as one page.
func (h *Handler) likers(w http.ResponseWriter, r *http.Request, ref repoRef) {
	rp, ok := h.load(w, r, ref, auth.ActionRead)
	if !ok {
		return
	}
	names, err := h.d.Community.Likers(r.Context(), rp.UUID)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		fullname := name
		if ns, err := h.d.Orgs.Get(r.Context(), name); err == nil && ns.DisplayName != "" {
			fullname = ns.DisplayName
		}
		out = append(out, map[string]any{"user": name, "fullname": fullname, "avatarUrl": ""})
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// userLikes answers GET /api/users/{user}/likes (list_liked_repos): the
// Repos a user likes, newest like first, leaving out those the caller
// cannot read.
func (h *Handler) userLikes(w http.ResponseWriter, r *http.Request, user string) {
	ctx := r.Context()
	ns, err := h.d.Orgs.Get(ctx, user)
	if err == nil && ns.UserID == uuid.Nil {
		err = org.ErrNotFound
	}
	if errors.Is(err, org.ErrNotFound) {
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusNotFound, Message: "User " + user + " not found."})
		return
	}
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	liked, err := h.d.Community.LikedBy(ctx, ns.UserID)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	p := httpx.Principal(ctx)
	out := make([]map[string]any, 0, len(liked))
	for _, id := range liked {
		rp, err := h.d.Repos.GetByUUID(ctx, id)
		if errors.Is(err, repo.ErrNotFound) {
			continue
		}
		if err != nil {
			h.internalError(w, r, err)
			return
		}
		if rp.DeletedAt != nil {
			continue
		}
		if err := h.d.Authz.Can(ctx, p, auth.ActionRead, target(rp)); err != nil {
			if errors.Is(err, auth.ErrNotFound) || errors.Is(err, auth.ErrForbidden) || errors.Is(err, auth.ErrGated) {
				continue
			}
			h.internalError(w, r, err)
			return
		}
		out = append(out, map[string]any{"repo": map[string]any{"name": rp.ID.String(), "type": string(rp.ID.Type)}})
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// commentChange answers POST .../discussions/{num}/comment/{id}/edit and
// .../hide (edit_discussion_comment, hide_discussion_comment). The author
// of a comment and the Repo's admins may edit it; its author and anyone who
// can write to the Repo may hide it, which removes its text for good.
func (h *Handler) commentChange(w http.ResponseWriter, r *http.Request, ref repoRef, num int, rawID, change string) {
	rp, ok := h.load(w, r, ref, auth.ActionDiscuss)
	if !ok {
		return
	}
	id, err := uuid.Parse(rawID)
	if err != nil {
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusNotFound, Message: "Comment not found."})
		return
	}
	d, err := h.d.Community.Get(r.Context(), rp.UUID, num)
	if err != nil {
		h.communityError(w, r, rp, err)
		return
	}
	var ev *community.Event
	for i := range d.Events {
		if d.Events[i].ID == id && d.Events[i].Type == community.EventComment {
			ev = &d.Events[i]
		}
	}
	if ev == nil {
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusNotFound, Message: "Comment not found."})
		return
	}
	p := httpx.Principal(r.Context())
	if p.IsAnonymous() || !strings.EqualFold(ev.Author, p.Username) {
		need := auth.ActionAdmin
		if change == "hide" {
			need = auth.ActionWrite
		}
		if !h.can(w, r, rp, need) {
			return
		}
	}
	var updated *community.Event
	if change == "edit" {
		var req struct {
			Content string `json:"content"`
		}
		if !h.decodeJSON(w, r, &req) {
			return
		}
		updated, err = h.d.Community.EditComment(r.Context(), rp.UUID, num, id, req.Content, p)
	} else {
		updated, err = h.d.Community.HideComment(r.Context(), rp.UUID, num, id, p)
	}
	if err != nil {
		h.communityError(w, r, rp, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{"updatedComment": eventJSON(*updated)})
}
