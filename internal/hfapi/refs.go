package hfapi

import (
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// commitsPageSize is the default page size of the Commit listing.
const commitsPageSize = 50

// noreplyDomain is the email domain of Commits made by Hangar for a User
// without an email address; the local part is the username.
const noreplyDomain = "@users.noreply.hangar"

// SafetensorsGuardMessage is the X-Error-Message of the 403 that stops
// transformers' safetensors auto-conversion (03-hf-api.md).
const SafetensorsGuardMessage = "Hangar does not send models to the Hugging Face safetensors conversion service, and this Revision has no safetensors weights. Set DISABLE_SAFETENSORS_CONVERSION=1 to stop transformers asking."

// refs serves GET /api/{type}s/{repo}/refs[?include_prs=1] (contract §4.1).
func (h *Handler) refs(w http.ResponseWriter, r *http.Request, ref repoRef) {
	rp, ok := h.load(w, r, ref, auth.ActionRead)
	if !ok {
		return
	}
	refs, err := h.d.Repos.Refs(r.Context(), rp.ID)
	if err != nil {
		h.serviceError(w, r, rp.ID, err)
		return
	}
	render := func(list []repo.Ref, short func(string) string) []map[string]any {
		out := make([]map[string]any, 0, len(list))
		for _, rf := range list {
			out = append(out, map[string]any{"name": short(rf.Name), "ref": rf.Name, "targetCommit": string(rf.Target)})
		}
		return out
	}
	trim := func(prefix string) func(string) string {
		return func(s string) string { return strings.TrimPrefix(s, prefix) }
	}
	out := map[string]any{
		"branches": render(refs.Branches, trim(model.RefHeads)),
		"tags":     render(refs.Tags, trim(model.RefTags)),
		"converts": render(refs.Converts, trim(model.RefConvert)),
	}
	if httpx.QueryBool(r.URL.Query(), "include_prs", false) {
		out["pullRequests"] = render(refs.PullRequests, trim(model.RefPR))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// commits serves GET /api/{type}s/{repo}/commits/{rev} (contract §4.2),
// newest first, paginated with an absolute Link header.
func (h *Handler) commits(w http.ResponseWriter, r *http.Request, ref repoRef, rev string) {
	rp, ok := h.load(w, r, ref, auth.ActionRead)
	if !ok {
		return
	}
	if rev == "" {
		rev = rp.DefaultBranch
	}
	sha, ok := h.resolveRevision(w, r, rp, rev)
	if !ok {
		return
	}
	if strings.HasPrefix(r.UserAgent(), "transformers/") && rp.ID.Type == model.RepoTypeModel {
		has, err := h.hasSafetensors(r, rp, sha)
		if err != nil {
			h.serviceError(w, r, rp.ID, err)
			return
		}
		if !has {
			httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusForbidden, Message: SafetensorsGuardMessage})
			return
		}
	}
	q := r.URL.Query()
	page := httpx.QueryInt(q, "p", 0, 0, 1<<30)
	limit := httpx.QueryInt(q, "limit", commitsPageSize, 1, 1000)
	log, err := h.d.Repos.Log(r.Context(), rp.ID, string(sha), repo.LogOpts{Skip: page * limit, Limit: limit + 1})
	if err != nil {
		h.serviceError(w, r, rp.ID, err)
		return
	}
	if len(log) > limit {
		log = log[:limit]
		next := url.Values{}
		for k, v := range q {
			next[k] = v
		}
		next.Set("p", strconv.Itoa(page+1))
		next.Set("limit", strconv.Itoa(limit))
		httpx.SetLinkNext(w, httpx.AbsoluteURL(h.d.Settings.PublicURL, r.URL.EscapedPath(), next))
	}
	formatted := false
	for _, e := range httpx.QueryList(q, "expand") {
		formatted = formatted || e == "formatted"
	}
	out := make([]map[string]any, 0, len(log))
	for _, c := range log {
		item := map[string]any{
			"id":      string(c.SHA),
			"title":   c.Title,
			"message": c.Message,
			"authors": []map[string]any{{"user": authorName(c.Author)}},
			"date":    model.NewHubTime(c.Author.When),
		}
		if formatted {
			item["formatted"] = map[string]any{
				"title":   "<p>" + html.EscapeString(c.Title) + "</p>",
				"message": "<p>" + strings.ReplaceAll(html.EscapeString(c.Message), "\n", "<br>") + "</p>",
			}
		}
		out = append(out, item)
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

// hasSafetensors reports whether the root of a Commit has safetensors
// weights or a safetensors index.
func (h *Handler) hasSafetensors(r *http.Request, rp *repo.Repo, sha model.CommitSHA) (bool, error) {
	entries, err := h.d.Repos.Tree(r.Context(), rp.ID, sha, "", repo.TreeOpts{})
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Type == model.EntryFile && (strings.HasSuffix(e.Path, ".safetensors") || strings.HasSuffix(e.Path, ".safetensors.index.json")) {
			return true, nil
		}
	}
	return false, nil
}

// authorName is the Hub `authors[].user` of a Commit.
func authorName(s repo.Signature) string {
	if local, ok := strings.CutSuffix(s.Email, noreplyDomain); ok && local != "" {
		return local
	}
	if s.Name != "" {
		return s.Name
	}
	return s.Email
}

// createBranch serves POST /api/{type}s/{repo}/branch/{branch} (contract §4.3).
func (h *Handler) createBranch(w http.ResponseWriter, r *http.Request, ref repoRef, branch string) {
	rp, ok := h.load(w, r, ref, auth.ActionWrite)
	if !ok {
		return
	}
	var req struct {
		StartingPoint string `json:"startingPoint"`
	}
	if !h.decodeOptionalJSON(w, r, &req) {
		return
	}
	from := req.StartingPoint
	if from == "" {
		from = rp.DefaultBranch
	}
	if _, ok := h.resolveRevision(w, r, rp, from); !ok {
		return
	}
	err := h.d.Repos.CreateBranch(r.Context(), rp.ID, branch, from, httpx.Principal(r.Context()))
	if err != nil {
		if errors.Is(err, repo.ErrRefExists) {
			httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusConflict, Message: "Reference already exists: refs/heads/" + branch + "."})
			return
		}
		h.serviceError(w, r, rp.ID, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{})
}

// deleteBranch serves DELETE /api/{type}s/{repo}/branch/{branch} (contract §4.4).
func (h *Handler) deleteBranch(w http.ResponseWriter, r *http.Request, ref repoRef, branch string) {
	rp, ok := h.load(w, r, ref, auth.ActionWrite)
	if !ok {
		return
	}
	if err := h.d.Repos.DeleteBranch(r.Context(), rp.ID, branch, httpx.Principal(r.Context())); err != nil {
		if errors.Is(err, repo.ErrRevisionNotFound) || errors.Is(err, repo.ErrEntryNotFound) {
			httpx.HubRevisionNotFound(w, r, branch)
			return
		}
		h.serviceError(w, r, rp.ID, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{})
}

// createTag serves POST /api/{type}s/{repo}/tag/{rev} (contract §4.5): the
// path names the Revision to tag and the body names the tag.
func (h *Handler) createTag(w http.ResponseWriter, r *http.Request, ref repoRef, rev string) {
	rp, ok := h.load(w, r, ref, auth.ActionWrite)
	if !ok {
		return
	}
	var req struct {
		Tag     string `json:"tag"`
		Message string `json:"message"`
	}
	if !h.decodeJSON(w, r, &req) {
		return
	}
	if req.Tag == "" {
		h.writeBadRequest(w, r, "The request body needs a tag name.")
		return
	}
	if _, ok := h.resolveRevision(w, r, rp, rev); !ok {
		return
	}
	err := h.d.Repos.CreateTag(r.Context(), repo.TagParams{Repo: rp.ID, Tag: req.Tag, Revision: rev, Message: req.Message, Actor: httpx.Principal(r.Context())})
	if err != nil {
		if errors.Is(err, repo.ErrRefExists) {
			httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusConflict, Message: "Tag reference already exists: refs/tags/" + req.Tag + "."})
			return
		}
		h.serviceError(w, r, rp.ID, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{})
}

// deleteTag serves DELETE /api/{type}s/{repo}/tag/{tag} (contract §4.6).
func (h *Handler) deleteTag(w http.ResponseWriter, r *http.Request, ref repoRef, tag string) {
	rp, ok := h.load(w, r, ref, auth.ActionWrite)
	if !ok {
		return
	}
	if err := h.d.Repos.DeleteTag(r.Context(), rp.ID, tag, httpx.Principal(r.Context())); err != nil {
		if errors.Is(err, repo.ErrRevisionNotFound) || errors.Is(err, repo.ErrEntryNotFound) {
			httpx.HubRevisionNotFound(w, r, tag)
			return
		}
		h.serviceError(w, r, rp.ID, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]any{})
}

// decodeJSON reads a buffered JSON body, answering 400 when it is not valid.
func (h *Handler) decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, h.d.Settings.BodyMax))
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusRequestEntityTooLarge, Message: "The request body is too large."})
			return false
		}
		h.writeBadRequest(w, r, "The request body is not valid JSON.")
		return false
	}
	return true
}

// decodeOptionalJSON is decodeJSON for endpoints whose body may be empty.
func (h *Handler) decodeOptionalJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return true
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, h.d.Settings.BodyMax))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		h.writeBadRequest(w, r, "The request body is not valid JSON.")
		return false
	}
	return true
}
