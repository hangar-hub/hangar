package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/community"
	"github.com/jamesagarside/hangar/internal/community/communitydb"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/repo"
)

// CommunityAPI serves discussions and pull requests (REPO-6).
type CommunityAPI struct{ d MetaDeps }

// NewCommunityAPI returns the discussion handlers. Community is required.
func NewCommunityAPI(d MetaDeps) (*CommunityAPI, error) {
	if err := d.init(); err != nil {
		return nil, err
	}
	if d.Community == nil {
		return nil, errors.New("api: Community is required")
	}
	return &CommunityAPI{d: d}, nil
}

func commEvent(e community.Event) gen.DiscussionEvent {
	out := gen.DiscussionEvent{Id: e.ID, Type: gen.DiscussionEventType(e.Type), Author: e.Author, CreatedAt: e.CreatedAt}
	if e.Body != "" {
		b := e.Body
		out.Body = &b
	}
	if e.Hidden {
		h := true
		out.Hidden = &h
	}
	data := map[string]interface{}{}
	for k, v := range e.Data {
		data[k] = v
	}
	if e.Edited {
		data["edited"] = true
	}
	if len(data) > 0 {
		out.Data = &data
	}
	return out
}

func commChange(c repo.FileChange) gen.FileChange {
	out := gen.FileChange{Path: c.Path, Status: gen.FileChangeStatus(c.Status), Lfs: c.LFS, Binary: c.Binary}
	if c.OldPath != "" {
		out.OldPath = &c.OldPath
	}
	if c.OldOID != "" {
		out.OldOid = &c.OldOID
	}
	if c.NewOID != "" {
		out.NewOid = &c.NewOID
	}
	if c.LFS {
		out.OldSize, out.NewSize = &c.OldSize, &c.NewSize
	}
	return out
}

func commDiscussion(d *community.Discussion, events bool) gen.Discussion {
	out := gen.Discussion{Num: d.Num, Kind: gen.DiscussionKind(d.Kind), Title: d.Title, Status: gen.DiscussionStatus(d.Status),
		Author: d.Author, CreatedAt: d.CreatedAt}
	if d.PRRef != "" {
		ref, base := d.PRRef, d.PRBase
		out.PrRef, out.PrBase = &ref, &base
	}
	if events {
		list := make([]gen.DiscussionEvent, 0, len(d.Events))
		for _, e := range d.Events {
			list = append(list, commEvent(e))
		}
		out.Events = &list
	}
	return out
}

// ListDiscussions implements gen.ServerInterface, newest first. The cursor
// is the next page number.
func (a *CommunityAPI) ListDiscussions(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, params gen.ListDiscussionsParams) {
	limit, ok := metaLimit(params.Limit, 50)
	if !ok {
		metaBadRequest(w, r, "limit", "limit must be between 1 and 500.")
		return
	}
	page := 0
	if params.Cursor != nil && *params.Cursor != "" {
		n, err := strconv.Atoi(*params.Cursor)
		if err != nil || n < 0 {
			metaBadRequest(w, r, "cursor", "The cursor is not one this API returned.")
			return
		}
		page = n
	}
	rp, err := a.d.metaLoad(r, pType, ns, name, auth.ActionRead)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	o := community.ListOptions{Page: page, Limit: limit}
	if params.Kind != nil {
		o.Type = string(*params.Kind)
	}
	if params.Status != nil {
		o.Status = string(*params.Status)
	}
	if params.Author != nil {
		o.Author = *params.Author
	}
	list, total, err := a.d.Community.List(r.Context(), rp.UUID, o)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	out := gen.DiscussionList{Items: make([]gen.Discussion, 0, len(list)), Total: total}
	for i := range list {
		out.Items = append(out.Items, commDiscussion(&list[i], false))
	}
	if (page+1)*limit < total {
		next := strconv.Itoa(page + 1)
		out.NextCursor = &next
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// CreateDiscussion implements gen.ServerInterface.
func (a *CommunityAPI) CreateDiscussion(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name) {
	rp, err := a.d.metaLoad(r, pType, ns, name, auth.ActionDiscuss)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	var body gen.DiscussionCreate
	if !metaDecode(w, r, &body) {
		return
	}
	p := community.CreateParams{RepoUUID: rp.UUID, Repo: rp.ID, Title: body.Title, Author: httpx.Principal(r.Context())}
	if body.Description != nil {
		p.Description = *body.Description
	}
	if body.PullRequest != nil {
		p.PullRequest = *body.PullRequest
	}
	if body.Base != nil && *body.Base != "" {
		if !p.PullRequest {
			metaBadRequest(w, r, "base", "base applies to pull requests only.")
			return
		}
		if _, err := a.d.Repos.Resolve(r.Context(), rp.ID, *body.Base); err != nil {
			a.d.metaProblem(w, r, err)
			return
		}
		p.Base = strings.TrimPrefix(*body.Base, "refs/heads/")
	}
	d, err := a.d.Community.Create(r.Context(), p)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusCreated, commDiscussion(d, true))
}

func (a *CommunityAPI) discussion(w http.ResponseWriter, r *http.Request, rp *repo.Repo, num int) (*community.Discussion, bool) {
	d, err := a.d.Community.Get(r.Context(), rp.UUID, num)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return nil, false
	}
	return d, true
}

func (a *CommunityAPI) withChanges(r *http.Request, rp *repo.Repo, d *community.Discussion) (gen.Discussion, error) {
	out := commDiscussion(d, true)
	if !d.IsPullRequest() {
		return out, nil
	}
	changes, err := communitydb.PRChanges(r.Context(), a.d.Repos, rp.ID, d, rp.DefaultBranch)
	if err != nil && !errors.Is(err, community.ErrConflict) {
		return out, err
	}
	list := make([]gen.FileChange, 0, len(changes))
	for _, c := range changes {
		list = append(list, commChange(c))
	}
	out.Changes = &list
	return out, nil
}

// GetDiscussion implements gen.ServerInterface: the thread, and for pull
// requests the files changed.
func (a *CommunityAPI) GetDiscussion(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, num gen.Num) {
	rp, err := a.d.metaLoad(r, pType, ns, name, auth.ActionRead)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	d, ok := a.discussion(w, r, rp, num)
	if !ok {
		return
	}
	out, err := a.withChanges(r, rp, d)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// GetPullRequestFileDiff implements gen.ServerInterface: one file's diff
// from where the pull request's ref left its base branch to its head
// (requests W8 item 7), so multi-Commit pull requests show every change.
func (a *CommunityAPI) GetPullRequestFileDiff(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, num gen.Num,
	params gen.GetPullRequestFileDiffParams) {
	rp, err := a.d.metaLoad(r, pType, ns, name, auth.ActionReadFiles)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	d, ok := a.discussion(w, r, rp, num)
	if !ok {
		return
	}
	if !d.IsPullRequest() {
		a.d.metaProblem(w, r, community.ErrNotPullRequest)
		return
	}
	ctx := r.Context()
	head, err := a.d.Repos.Resolve(ctx, rp.ID, d.PRRef)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	base := d.PRBase
	if base == "" {
		base = rp.DefaultBranch
	}
	baseTip, err := a.d.Repos.Resolve(ctx, rp.ID, base)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	fork, err := communitydb.MergeBase(ctx, a.d.Repos, rp.ID, baseTip, head)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	fd, err := a.d.Repos.CompareFile(ctx, rp.ID, fork, head, params.Path)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	c := commChange(fd.FileChange)
	out := gen.FileDiff{Path: c.Path, OldPath: c.OldPath, Status: gen.FileDiffStatus(c.Status), OldOid: c.OldOid, NewOid: c.NewOid,
		OldSize: c.OldSize, NewSize: c.NewSize, Lfs: c.Lfs, Binary: c.Binary}
	if fd.Patch != "" {
		out.Patch = &fd.Patch
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// UpdateDiscussion implements gen.ServerInterface. The author and anyone
// who can write to the Repo may rename, close or reopen.
func (a *CommunityAPI) UpdateDiscussion(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, num gen.Num) {
	rp, err := a.d.metaLoad(r, pType, ns, name, auth.ActionDiscuss)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	var body gen.DiscussionUpdate
	if !metaDecode(w, r, &body) {
		return
	}
	if body.Title == nil && body.Status == nil {
		metaBadRequest(w, r, "", "Give a title, a status or both.")
		return
	}
	d, ok := a.discussion(w, r, rp, num)
	if !ok {
		return
	}
	p := httpx.Principal(r.Context())
	if !strings.EqualFold(d.Author, p.Username) || d.Author == "" {
		if err := a.d.Authz.Can(r.Context(), p, auth.ActionWrite, metaTarget(rp)); err != nil {
			httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusForbidden, Detail: "Only the author and people who can write to the Repo can change this."})
			return
		}
	}
	if body.Title != nil && strings.TrimSpace(*body.Title) != d.Title {
		if _, err := a.d.Community.Rename(r.Context(), rp.UUID, num, *body.Title, p); err != nil {
			a.d.metaProblem(w, r, err)
			return
		}
	}
	if body.Status != nil {
		comment := ""
		if body.Comment != nil {
			comment = *body.Comment
		}
		if _, err := a.d.Community.SetStatus(r.Context(), rp.UUID, num, string(*body.Status), comment, p); err != nil {
			a.d.metaProblem(w, r, err)
			return
		}
	}
	if d, ok = a.discussion(w, r, rp, num); !ok {
		return
	}
	out, err := a.withChanges(r, rp, d)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// CommentOnDiscussion implements gen.ServerInterface.
func (a *CommunityAPI) CommentOnDiscussion(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, num gen.Num) {
	rp, err := a.d.metaLoad(r, pType, ns, name, auth.ActionDiscuss)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	var body gen.CommentCreate
	if !metaDecode(w, r, &body) {
		return
	}
	e, err := a.d.Community.Comment(r.Context(), rp.UUID, num, body.Body, httpx.Principal(r.Context()))
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusCreated, commEvent(*e))
}

// MergePullRequest implements gen.ServerInterface. Merging needs write
// access to the Repo.
func (a *CommunityAPI) MergePullRequest(w http.ResponseWriter, r *http.Request, pType gen.RepoType, ns gen.Ns, name gen.Name, num gen.Num) {
	rp, err := a.d.metaLoad(r, pType, ns, name, auth.ActionWrite)
	if err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	var body gen.MergeRequest
	if r.ContentLength != 0 {
		if !metaDecode(w, r, &body) {
			return
		}
	}
	comment := ""
	if body.Comment != nil {
		comment = *body.Comment
	}
	if _, err := a.d.Community.Merge(r.Context(), rp.UUID, num, comment, httpx.Principal(r.Context())); err != nil {
		a.d.metaProblem(w, r, err)
		return
	}
	d, ok := a.discussion(w, r, rp, num)
	if !ok {
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, commDiscussion(d, true))
}
