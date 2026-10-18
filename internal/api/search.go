package api

import (
	"net/http"
	"strings"

	"github.com/jamesagarside/hangar/internal/api/gen"
	"github.com/jamesagarside/hangar/internal/collections"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/repo"
)

// SearchAPI serves global search across Repos, Namespaces and Collections
// (BROWSE-6).
type SearchAPI struct{ d RepoDeps }

// NewSearchAPI returns the search handler.
func NewSearchAPI(d RepoDeps) (*SearchAPI, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	return &SearchAPI{d: d}, nil
}

// Search implements gen.ServerInterface. Each result type gets up to limit
// results; Repos and Collections the caller cannot see are left out.
func (a *SearchAPI) Search(w http.ResponseWriter, r *http.Request, params gen.SearchParams) {
	limit, err := limitOf(params.Limit)
	if err != nil {
		badRequest(w, r, "limit", err.Error())
		return
	}
	q := strings.TrimSpace(params.Q)
	if q == "" {
		badRequest(w, r, "q", "Enter something to search for.")
		return
	}
	want := map[string]bool{"repos": true, "namespaces": true, "collections": true}
	if params.Types != nil && len(*params.Types) > 0 {
		want = map[string]bool{}
		for _, t := range *params.Types {
			switch t {
			case "repos", "namespaces", "collections":
				want[string(t)] = true
			default:
				badRequest(w, r, "types", "types may contain repos, namespaces and collections.")
				return
			}
		}
	}
	p := httpx.Principal(r.Context())
	out := gen.SearchResults{Repos: []gen.Repo{}, Namespaces: []gen.Namespace{}, Collections: []gen.Collection{}}
	if want["repos"] {
		res, err := a.d.Repos.List(r.Context(), repo.ListOptions{Viewer: p, Search: q, Limit: limit, Sort: "updated", Desc: true})
		if err != nil {
			a.d.problem(w, r, err)
			return
		}
		for _, rp := range res.Repos {
			out.Repos = append(out.Repos, a.d.toRepo(r.Context(), p, rp))
		}
	}
	if want["namespaces"] {
		// Anonymous callers find no users or Organisations while
		// security.anonymous_read is off, as they find no Repos.
		ok, err := canSeeNamespaces(r.Context(), a.d.Authz, p)
		if err != nil {
			a.d.problem(w, r, err)
			return
		}
		want["namespaces"] = ok
	}
	if want["namespaces"] {
		nss, err := a.d.NamespaceSearch.SearchNamespaces(r.Context(), q, limit)
		if err != nil {
			a.d.problem(w, r, err)
			return
		}
		for i := range nss {
			out.Namespaces = append(out.Namespaces, toNamespace(&nss[i]))
		}
	}
	if want["collections"] {
		cs, err := a.d.Collections.List(r.Context(), p, collections.ListOptions{Query: q, Limit: limit})
		if err != nil {
			a.d.problem(w, r, err)
			return
		}
		for _, c := range cs {
			out.Collections = append(out.Collections, toCollection(c))
		}
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}

func toNamespace(n *org.Namespace) gen.Namespace {
	out := gen.Namespace{Name: n.Name, Kind: gen.NamespaceKind(n.Kind), DisplayName: n.DisplayName, Description: str(n.Description)}
	if out.DisplayName == "" {
		out.DisplayName = n.Name
	}
	if n.ReservedMirror {
		rm := true
		out.ReservedMirror = &rm
	}
	if !n.CreatedAt.IsZero() {
		ca := n.CreatedAt
		out.CreatedAt = &ca
	}
	return out
}

func toCollection(c collections.Collection) gen.Collection {
	out := gen.Collection{Namespace: c.Namespace, Slug: c.Slug, Title: c.Title, Description: str(c.Description),
		Visibility: gen.Visibility(c.Visibility), Theme: str(c.Theme), UpdatedAt: c.UpdatedAt, Items: make([]gen.CollectionItem, 0, len(c.Items))}
	for _, it := range c.Items {
		out.Items = append(out.Items, gen.CollectionItem{Id: it.ID, Type: gen.CollectionItemType(it.Type), ItemId: it.ItemID,
			Position: it.Position, Note: str(it.Note)})
	}
	return out
}
