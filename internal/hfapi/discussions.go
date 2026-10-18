package hfapi

import (
	"errors"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/community"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

// discussionsPageSize is the page size of the discussion listing.
const discussionsPageSize = 50

// serveDiscussions routes /api/{type}s/{repo}/discussions[/{num}[/{action}]]
// (REPO-6), the endpoints huggingface_hub's discussion and pull request
// functions call.
func (h *Handler) serveDiscussions(w http.ResponseWriter, r *http.Request, ref repoRef, prefix string, args reqPath) {
	switch args.len() {
	case 0:
		h.route(w, r, prefix, true, methods{
			http.MethodGet:  func(w http.ResponseWriter, r *http.Request) { h.listDiscussions(w, r, ref) },
			http.MethodPost: func(w http.ResponseWriter, r *http.Request) { h.createDiscussion(w, r, ref) },
		})
		return
	}
	num, err := strconv.Atoi(args.at(0))
	if err != nil || num <= 0 {
		h.notFound(w, r)
		return
	}
	switch {
	case args.len() == 1:
		h.route(w, r, prefix+"/{num}", true, methods{http.MethodGet: func(w http.ResponseWriter, r *http.Request) { h.getDiscussion(w, r, ref, num) }})
	case args.len() == 2 && args.at(1) == "comment":
		h.route(w, r, prefix+"/{num}/comment", true, methods{http.MethodPost: func(w http.ResponseWriter, r *http.Request) { h.commentDiscussion(w, r, ref, num) }})
	case args.len() == 2 && args.at(1) == "status":
		h.route(w, r, prefix+"/{num}/status", true, methods{http.MethodPost: func(w http.ResponseWriter, r *http.Request) { h.discussionStatus(w, r, ref, num) }})
	case args.len() == 2 && args.at(1) == "title":
		h.route(w, r, prefix+"/{num}/title", true, methods{http.MethodPost: func(w http.ResponseWriter, r *http.Request) { h.renameDiscussion(w, r, ref, num) }})
	case args.len() == 2 && args.at(1) == "merge":
		h.route(w, r, prefix+"/{num}/merge", true, methods{http.MethodPost: func(w http.ResponseWriter, r *http.Request) { h.mergeDiscussion(w, r, ref, num) }})
	case args.len() == 4 && args.at(1) == "comment" && (args.at(3) == "edit" || args.at(3) == "hide"):
		change := args.at(3)
		h.route(w, r, prefix+"/{num}/comment/{id}/"+change, true, methods{http.MethodPost: func(w http.ResponseWriter, r *http.Request) {
			h.commentChange(w, r, ref, num, args.at(2), change)
		}})
	default:
		h.notFound(w, r)
	}
}

// communityError maps a community.Service error to the Hub's response.
func (h *Handler) communityError(w http.ResponseWriter, r *http.Request, rp *repo.Repo, err error) {
	switch {
	case errors.Is(err, community.ErrNotFound):
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusNotFound, Message: "Discussion not found in " + rp.ID.Path() + "."})
	case errors.Is(err, community.ErrNotOpen):
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusConflict, Message: errMessage(err)})
	case errors.Is(err, community.ErrNotPullRequest), errors.Is(err, community.ErrInvalid):
		h.writeBadRequest(w, r, errMessage(err))
	default:
		h.serviceError(w, r, rp.ID, err)
	}
}

func (h *Handler) discussionJSON(rp *repo.Repo, d *community.Discussion, details bool) map[string]any {
	comments := 0
	for _, e := range d.Events {
		if e.Type == community.EventComment {
			comments++
		}
	}
	out := map[string]any{
		"num": d.Num, "title": d.Title, "status": d.Status, "createdAt": model.NewHubTime(d.CreatedAt),
		"isPullRequest": d.IsPullRequest(), "author": map[string]any{"name": d.Author},
		"repo": map[string]any{"name": rp.ID.String(), "type": string(rp.ID.Type)}, "pinned": false, "numComments": comments,
	}
	if !details {
		return out
	}
	events := make([]map[string]any, 0, len(d.Events))
	// A title change without a recorded previous title takes the one the
	// change before it set.
	prev := ""
	for _, e := range d.Events {
		ev := eventJSON(e)
		if e.Type == community.EventTitleChange {
			data := ev["data"].(map[string]any)
			if _, ok := data["from"]; !ok {
				data["from"] = prev
			}
			prev, _ = data["to"].(string)
		}
		events = append(events, ev)
	}
	out["events"] = events
	if d.IsPullRequest() {
		changes := map[string]any{"base": model.BranchRef(orDefault(d.PRBase, rp.DefaultBranch))}
		for _, e := range d.Events {
			if s, _ := e.Data["status"].(string); s == community.StatusMerged {
				if oid, ok := e.Data["oid"].(string); ok {
					changes["mergeCommitId"] = oid
				}
			}
		}
		out["changes"] = changes
		out["filesWithConflicts"] = []string{}
	}
	return out
}

// eventJSON renders a thread event in the shape huggingface_hub's
// deserialize_event reads.
// numEdits is 1 for an edited comment: Hangar keeps only the latest text.
func numEdits(e community.Event) int {
	if e.Edited {
		return 1
	}
	return 0
}

func eventJSON(e community.Event) map[string]any {
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := e.Data[k].(string); ok {
				return v
			}
		}
		return ""
	}
	var data map[string]any
	switch e.Type {
	case community.EventComment:
		data = map[string]any{
			"edited": e.Edited, "hidden": e.Hidden, "numEdits": numEdits(e), "editors": []string{},
			"latest": map[string]any{"raw": e.Body, "html": "<p>" + html.EscapeString(e.Body) + "</p>", "author": map[string]any{"name": e.Author}},
		}
	case community.EventStatusChange:
		data = map[string]any{"status": str("status")}
	case community.EventTitleChange:
		data = map[string]any{"to": str("to")}
		if from := str("from"); from != "" {
			data["from"] = from
		}
	case community.EventCommit:
		data = map[string]any{"subject": str("subject", "title"), "oid": str("oid", "sha")}
	default:
		data = map[string]any{}
		for k, v := range e.Data {
			data[k] = v
		}
	}
	return map[string]any{
		"id": e.ID.String(), "type": e.Type, "createdAt": model.NewHubTime(e.CreatedAt),
		"author": map[string]any{"name": e.Author}, "data": data,
	}
}

func (h *Handler) listDiscussions(w http.ResponseWriter, r *http.Request, ref repoRef) {
	rp, ok := h.load(w, r, ref, auth.ActionRead)
	if !ok {
		return
	}
	q := r.URL.Query()
	opts := community.ListOptions{Author: q.Get("author"), Limit: discussionsPageSize, Page: httpx.QueryInt(q, "p", 0, 0, 1<<30)}
	switch t := q.Get("type"); t {
	case "", "all":
	case community.KindDiscussion, community.KindPullRequest:
		opts.Type = t
	default:
		h.writeBadRequest(w, r, "type must be all, discussion or pull_request.")
		return
	}
	switch s := q.Get("status"); s {
	case "", "all":
	case community.StatusOpen, community.StatusClosed:
		opts.Status = s
	default:
		h.writeBadRequest(w, r, "status must be all, open or closed.")
		return
	}
	list, total, err := h.d.Community.List(r.Context(), rp.UUID, opts)
	if err != nil {
		h.communityError(w, r, rp, err)
		return
	}
	items := make([]map[string]any, 0, len(list))
	for i := range list {
		items = append(items, h.discussionJSON(rp, &list[i], false))
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{"discussions": items, "count": total, "start": opts.Page * discussionsPageSize})
}

func (h *Handler) createDiscussion(w http.ResponseWriter, r *http.Request, ref repoRef) {
	rp, ok := h.load(w, r, ref, auth.ActionDiscuss)
	if !ok {
		return
	}
	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		PullRequest bool   `json:"pullRequest"`
	}
	if !h.decodeJSON(w, r, &req) {
		return
	}
	d, err := h.d.Community.Create(r.Context(), community.CreateParams{
		RepoUUID: rp.UUID, Repo: rp.ID, Title: strings.TrimSpace(req.Title), Description: req.Description,
		PullRequest: req.PullRequest, Base: rp.DefaultBranch, Author: httpx.Principal(r.Context()),
	})
	if err != nil {
		h.communityError(w, r, rp, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, h.discussionJSON(rp, d, true))
}

func (h *Handler) getDiscussion(w http.ResponseWriter, r *http.Request, ref repoRef, num int) {
	rp, ok := h.load(w, r, ref, auth.ActionRead)
	if !ok {
		return
	}
	d, err := h.d.Community.Get(r.Context(), rp.UUID, num)
	if err != nil {
		h.communityError(w, r, rp, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, h.discussionJSON(rp, d, true))
}

func (h *Handler) commentDiscussion(w http.ResponseWriter, r *http.Request, ref repoRef, num int) {
	rp, ok := h.load(w, r, ref, auth.ActionDiscuss)
	if !ok {
		return
	}
	var req struct {
		Comment string `json:"comment"`
	}
	if !h.decodeJSON(w, r, &req) {
		return
	}
	e, err := h.d.Community.Comment(r.Context(), rp.UUID, num, req.Comment, httpx.Principal(r.Context()))
	if err != nil {
		h.communityError(w, r, rp, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{"newMessage": eventJSON(*e)})
}

// authorOrWriter loads a discussion and checks the caller opened it or may
// write to the Repo, as changing its status or title needs.
func (h *Handler) authorOrWriter(w http.ResponseWriter, r *http.Request, rp *repo.Repo, num int) (*community.Discussion, bool) {
	d, err := h.d.Community.Get(r.Context(), rp.UUID, num)
	if err != nil {
		h.communityError(w, r, rp, err)
		return nil, false
	}
	p := httpx.Principal(r.Context())
	if !p.IsAnonymous() && strings.EqualFold(d.Author, p.Username) {
		return d, true
	}
	return d, h.can(w, r, rp, auth.ActionWrite)
}

func (h *Handler) discussionStatus(w http.ResponseWriter, r *http.Request, ref repoRef, num int) {
	rp, ok := h.load(w, r, ref, auth.ActionDiscuss)
	if !ok {
		return
	}
	var req struct {
		Status  string `json:"status"`
		Comment string `json:"comment"`
	}
	if !h.decodeJSON(w, r, &req) {
		return
	}
	if req.Status != community.StatusOpen && req.Status != community.StatusClosed {
		h.writeBadRequest(w, r, "status must be open or closed.")
		return
	}
	if _, ok := h.authorOrWriter(w, r, rp, num); !ok {
		return
	}
	e, err := h.d.Community.SetStatus(r.Context(), rp.UUID, num, req.Status, req.Comment, httpx.Principal(r.Context()))
	if err != nil {
		h.communityError(w, r, rp, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{"newStatus": eventJSON(*e)})
}

func (h *Handler) renameDiscussion(w http.ResponseWriter, r *http.Request, ref repoRef, num int) {
	rp, ok := h.load(w, r, ref, auth.ActionDiscuss)
	if !ok {
		return
	}
	var req struct {
		Title string `json:"title"`
	}
	if !h.decodeJSON(w, r, &req) {
		return
	}
	d, ok := h.authorOrWriter(w, r, rp, num)
	if !ok {
		return
	}
	e, err := h.d.Community.Rename(r.Context(), rp.UUID, num, strings.TrimSpace(req.Title), httpx.Principal(r.Context()))
	if err != nil {
		h.communityError(w, r, rp, err)
		return
	}
	ev := eventJSON(*e)
	data := ev["data"].(map[string]any)
	if _, ok := data["from"]; !ok {
		data["from"] = d.Title
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{"newTitle": ev})
}

func (h *Handler) mergeDiscussion(w http.ResponseWriter, r *http.Request, ref repoRef, num int) {
	rp, ok := h.load(w, r, ref, auth.ActionWrite)
	if !ok {
		return
	}
	var req struct {
		Comment string `json:"comment"`
	}
	if !h.decodeOptionalJSON(w, r, &req) {
		return
	}
	if _, err := h.d.Community.Merge(r.Context(), rp.UUID, num, req.Comment, httpx.Principal(r.Context())); err != nil {
		h.communityError(w, r, rp, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{})
}

// like serves POST and DELETE /api/{type}s/{repo}/like (REPO-7).
func (h *Handler) like(w http.ResponseWriter, r *http.Request, ref repoRef, like bool) {
	rp, ok := h.load(w, r, ref, auth.ActionDiscuss)
	if !ok {
		return
	}
	p := httpx.Principal(r.Context())
	var err error
	if like {
		err = h.d.Community.Like(r.Context(), p.UserID, rp.UUID)
	} else {
		err = h.d.Community.Unlike(r.Context(), p.UserID, rp.UUID)
	}
	if err != nil {
		h.communityError(w, r, rp, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{})
}
