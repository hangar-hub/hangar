package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/hangar-hub/hangar/internal/api/gen"
	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/org"
)

// signInToSeeNamespaces is the answer to an anonymous caller while
// security.anonymous_read is off, whether or not the Namespace exists.
const signInToSeeNamespaces = "Sign in to see users and Organisations on this Instance."

// canSeeNamespaces reports whether p may see Namespace profiles and
// Organisation members: anyone while security.anonymous_read is on, only
// signed-in callers while it is off (05-auth.md "Permission rules"). It
// asks Authz before any lookup, so a refusal says nothing about whether a
// Namespace exists.
func canSeeNamespaces(ctx context.Context, authz auth.Authz, p model.Principal) (bool, error) {
	err := authz.Can(ctx, p, auth.ActionRead, auth.Target{Namespace: &auth.NamespaceTarget{}})
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, auth.ErrForbidden), errors.Is(err, auth.ErrNotFound):
		return false, nil
	default:
		return false, err
	}
}

// refuseNamespaces answers 401 to a caller canSeeNamespaces refused, the
// answer anonymous callers get for any closed resource.
func refuseNamespaces(w http.ResponseWriter, r *http.Request) {
	httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusUnauthorized, Detail: signInToSeeNamespaces})
}

// NamespacesAPI serves Namespace profiles: users' and Organisations' pages.
type NamespacesAPI struct{ d RepoDeps }

// NewNamespacesAPI returns the Namespace handler.
func NewNamespacesAPI(d RepoDeps) (*NamespacesAPI, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	return &NamespacesAPI{d: d}, nil
}

// GetNamespace implements gen.ServerInterface. repo_count counts the Repos
// the caller can see; member_count is given for Organisations. While
// security.anonymous_read is off, anonymous callers get 401.
func (a *NamespacesAPI) GetNamespace(w http.ResponseWriter, r *http.Request, ns gen.Ns) {
	ok, err := canSeeNamespaces(r.Context(), a.d.Authz, httpx.Principal(r.Context()))
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	if !ok {
		refuseNamespaces(w, r)
		return
	}
	n, err := a.d.Orgs.Get(r.Context(), ns)
	if err != nil {
		if errors.Is(err, org.ErrNotFound) {
			httpx.WriteProblem(w, r, &httpx.Problem{Status: http.StatusNotFound, Detail: "Namespace " + ns + " does not exist."})
			return
		}
		a.d.problem(w, r, err)
		return
	}
	out := toNamespace(n)
	count, err := a.d.Repos.CountByNamespace(r.Context(), n.ID, httpx.Principal(r.Context()))
	if err != nil {
		a.d.problem(w, r, err)
		return
	}
	out.RepoCount = &count
	if n.Kind == org.KindOrg {
		members, err := a.d.Orgs.Members(r.Context(), n.Name)
		if err != nil {
			a.d.problem(w, r, err)
			return
		}
		mc := len(members)
		out.MemberCount = &mc
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}
