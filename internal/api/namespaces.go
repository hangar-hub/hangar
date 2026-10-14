package api

import (
	"errors"
	"net/http"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/org"
)

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
// the caller can see; member_count is given for Organisations.
func (a *NamespacesAPI) GetNamespace(w http.ResponseWriter, r *http.Request, ns gen.Ns) {
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
