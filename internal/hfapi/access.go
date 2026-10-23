package hfapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

// target describes a Repo for a permission check.
func target(rp *repo.Repo) auth.Target {
	return auth.Target{Repo: &auth.RepoTarget{
		ID: rp.ID, UUID: rp.UUID, NamespaceID: rp.NamespaceID, Visibility: rp.Visibility,
		Gated: rp.Gating.Enabled(), Mirror: rp.Mirror,
	}}
}

// repoRef is a Repo named in a request path: the requested ID, and the
// escaped path segments before and after `ns/name`, for redirects.
type repoRef struct {
	id     model.RepoID
	before []string
	after  []string
}

// load looks up a Repo, checks the caller may read it, redirects requests
// for renamed or transferred Repos to the canonical ID with a 307, then
// checks the action. ok false means a response was written.
//
// Read access is checked before redirecting, so the redirect never shows a
// private Repo's new name to a caller who cannot read it. One Authz call
// covers both checks: a caller who cannot read a Repo gets ErrNotFound for
// every action (auth.Authz), so ErrForbidden or ErrGated means the Repo is
// readable and only the action is refused. Every Hub request comes through
// here, and each check costs database queries.
func (h *Handler) load(w http.ResponseWriter, r *http.Request, ref repoRef, action auth.Action) (*repo.Repo, bool) {
	ctx := r.Context()
	p := httpx.Principal(ctx)
	httpx.SetRepo(ctx, ref.id)
	rp, err := h.d.Repos.Get(ctx, ref.id)
	if errors.Is(err, repo.ErrNotFound) && h.d.PullThrough != nil && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		rp, err = h.d.PullThrough.Repo(ctx, p, ref.id)
	}
	if err != nil {
		switch {
		case errors.Is(err, repo.ErrNotFound), errors.Is(err, repo.ErrInvalid):
			httpx.HubRepoNotFound(w, r, ref.id)
		case errors.Is(err, ErrPullPending):
			pullPending(w, r, ref.id)
		default:
			// A Pull-through refused by policy, such as the Namespace's
			// storage quota, is a *repo.PolicyError: 403 with its message.
			h.serviceError(w, r, ref.id, err)
		}
		return nil, false
	}
	err = h.d.Authz.Can(ctx, p, action, target(rp))
	if err != nil && !errors.Is(err, auth.ErrForbidden) && !errors.Is(err, auth.ErrGated) {
		h.denied(w, r, ref.id, err)
		return nil, false
	}
	if !rp.ID.Equal(ref.id) || rp.ID.Type != ref.id.Type {
		h.redirectMoved(w, r, ref, rp.ID)
		return nil, false
	}
	httpx.SetRepo(ctx, rp.ID)
	if err != nil {
		h.denied(w, r, rp.ID, err)
		return nil, false
	}
	return rp, true
}

// can checks a further action on a Repo already loaded.
func (h *Handler) can(w http.ResponseWriter, r *http.Request, rp *repo.Repo, action auth.Action) bool {
	if err := h.d.Authz.Can(r.Context(), httpx.Principal(r.Context()), action, target(rp)); err != nil {
		h.denied(w, r, rp.ID, err)
		return false
	}
	return true
}

// denied maps an Authz error to the Hub's response.
func (h *Handler) denied(w http.ResponseWriter, r *http.Request, id model.RepoID, err error) {
	switch {
	case errors.Is(err, auth.ErrNotFound):
		httpx.HubRepoNotFound(w, r, id)
	case errors.Is(err, auth.ErrGated):
		httpx.HubGated(w, r, id)
	case errors.Is(err, auth.ErrForbidden):
		h.writeForbidden(w, r, "You do not have permission to do this on "+id.Path()+".")
	default:
		h.internalError(w, r, err)
	}
}

// redirectMoved answers 307 with a relative Location that swaps the old
// `ns/name` for the canonical one and keeps the rest of the path and query
// (contract §10). 307 keeps the method and body.
func (h *Handler) redirectMoved(w http.ResponseWriter, r *http.Request, ref repoRef, to model.RepoID) {
	parts := append(append(append([]string{}, ref.before...), url.PathEscape(to.Namespace), url.PathEscape(to.Name)), ref.after...)
	loc := "/" + strings.Join(parts, "/")
	if r.URL.RawQuery != "" {
		loc += "?" + r.URL.RawQuery
	}
	w.Header().Set("Location", loc)
	httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusTemporaryRedirect, Message: "Repository moved to " + to.Path() + "."})
}

// redirectSingleSegment answers 307 for a single-segment Repo ID, such as
// /gpt2/resolve/main/config.json, inserting hfapi.default_namespace.
func (h *Handler) redirectSingleSegment(w http.ResponseWriter, r *http.Request, before, after []string) {
	parts := append(append(append([]string{}, before...), url.PathEscape(h.d.Settings.DefaultNamespace)), after...)
	loc := "/" + strings.Join(parts, "/")
	if r.URL.RawQuery != "" {
		loc += "?" + r.URL.RawQuery
	}
	w.Header().Set("Location", loc)
	httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusTemporaryRedirect, Message: "Repo IDs need a Namespace; redirecting to " + h.d.Settings.DefaultNamespace + "."})
}

// resolveRevision turns a Revision into a Commit SHA, writing
// RevisionNotFound when it names nothing.
func (h *Handler) resolveRevision(w http.ResponseWriter, r *http.Request, rp *repo.Repo, rev string) (model.CommitSHA, bool) {
	if rev == "" {
		rev = rp.DefaultBranch
	}
	sha, err := h.d.Repos.Resolve(r.Context(), rp.ID, rev)
	if errors.Is(err, repo.ErrRevisionNotFound) && h.awaitsHistory(r, rp) {
		sha, err = h.resolveAfterHistory(r, rp, rev)
	}
	if err != nil {
		switch {
		case errors.Is(err, repo.ErrRevisionNotFound), errors.Is(err, repo.ErrInvalid):
			httpx.HubRevisionNotFound(w, r, rev)
		case errors.Is(err, ErrPullPending):
			pullPending(w, r, rp.ID)
		default:
			h.serviceError(w, r, rp.ID, err)
		}
		return "", false
	}
	return sha, true
}

// awaitsHistory reports whether a Revision that does not resolve in rp
// may be history that a Pull has not stored yet: rp is a Mirror, which
// planning creates before it stores the history, and Pull-through is on
// for this read.
func (h *Handler) awaitsHistory(r *http.Request, rp *repo.Repo) bool {
	return rp.Mirror && h.d.PullThrough != nil && (r.Method == http.MethodGet || r.Method == http.MethodHead)
}

// resolveAfterHistory asks Pull-through to wait for the history of a
// Mirror that has none yet, then resolves rev again. It returns
// ErrPullPending while planning goes on, and repo.ErrRevisionNotFound
// when Pull-through does not apply or rev still names nothing.
func (h *Handler) resolveAfterHistory(r *http.Request, rp *repo.Repo, rev string) (model.CommitSHA, error) {
	ctx := r.Context()
	if _, err := h.d.PullThrough.Repo(ctx, httpx.Principal(ctx), rp.ID); errors.Is(err, repo.ErrNotFound) {
		return "", fmt.Errorf("%w: %s", repo.ErrRevisionNotFound, rev)
	} else if err != nil {
		return "", err
	}
	return h.d.Repos.Resolve(ctx, rp.ID, rev)
}

// pullPending answers 503 with Retry-After for a Repo that a Pull-through
// is still planning.
func pullPending(w http.ResponseWriter, r *http.Request, id model.RepoID) {
	w.Header().Set("Retry-After", "10")
	httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusServiceUnavailable, Message: "Hangar is pulling " + id.Path() + " into the Arsenal. Try again shortly."})
}

// serviceError maps a repo.Service error to the Hub's response.
func (h *Handler) serviceError(w http.ResponseWriter, r *http.Request, id model.RepoID, err error) {
	var pe *repo.PolicyError
	switch {
	case errors.Is(err, repo.ErrNotFound):
		httpx.HubRepoNotFound(w, r, id)
	case errors.Is(err, repo.ErrRevisionNotFound):
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusNotFound, Code: httpx.CodeRevisionNotFound, Message: errMessage(err)})
	case errors.Is(err, repo.ErrEntryNotFound):
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusNotFound, Code: httpx.CodeEntryNotFound, Message: errMessage(err)})
	case errors.Is(err, repo.ErrExists), errors.Is(err, repo.ErrRefExists):
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusConflict, Message: errMessage(err)})
	case errors.Is(err, repo.ErrParentMismatch):
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusPreconditionFailed, Message: errMessage(err)})
	case errors.Is(err, repo.ErrRefConflict):
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusConflict, Message: errMessage(err)})
	case errors.Is(err, repo.ErrReadOnly), errors.Is(err, repo.ErrProtectedRef):
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusForbidden, Message: errMessage(err)})
	case errors.As(err, &pe):
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusForbidden, Message: pe.Error()})
	case errors.Is(err, repo.ErrLFSObjectMissing), errors.Is(err, repo.ErrInvalid), errors.Is(err, model.ErrInvalidPath),
		errors.Is(err, model.ErrInvalidRefName), errors.Is(err, repo.ErrChecksumMismatch):
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusBadRequest, Message: errMessage(err)})
	case errors.Is(err, context.Canceled):
		httpx.WriteHubError(w, r, &httpx.HubError{Status: httpx.StatusClientClosedRequest, Message: httpx.ClientClosedMessage})
	default:
		h.internalError(w, r, err)
	}
}

// errMessage turns a wrapped service error into a sentence for
// X-Error-Message: the first letter in upper case, ending in a full stop.
func errMessage(err error) string {
	s := err.Error()
	if s == "" {
		return "Request failed."
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}
