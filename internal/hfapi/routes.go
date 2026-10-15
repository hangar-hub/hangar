package hfapi

import (
	"net/http"

	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
)

type methods = map[string]http.HandlerFunc

// serveRepoAPI routes /api/{models|datasets}/{ns}/{name}[/{action}/...].
func (h *Handler) serveRepoAPI(w http.ResponseWriter, r *http.Request, t model.RepoType, p reqPath) {
	before := []string{"api", t.Plural()}
	// Single-segment Repo IDs: /api/models/gpt2, /api/models/gpt2/revision/main.
	if h.d.Settings.DefaultNamespace != "" {
		if p.len() == 1 || (p.len() >= 3 && repoActions[p.at(1)] && !repoActions[p.at(2)]) {
			httpx.SetRoute(r.Context(), "/api/"+t.Plural()+"/{name}")
			h.redirectSingleSegment(w, r, before, p.esc)
			return
		}
	}
	if p.len() < 2 {
		h.notFound(w, r)
		return
	}
	ref := repoRef{id: model.RepoID{Type: t, Namespace: p.at(0), Name: p.at(1)}, before: before, after: p.esc[2:]}
	rest := p.shift(2)
	prefix := "/api/" + t.Plural() + "/{ns}/{name}"
	if rest.len() == 0 {
		h.route(w, r, prefix, true, methods{http.MethodGet: func(w http.ResponseWriter, r *http.Request) { h.repoInfo(w, r, ref, "") }})
		return
	}
	action := rest.at(0)
	args := rest.shift(1)
	switch action {
	case "revision":
		rev, tail := takeRevision(args)
		h.route(w, r, prefix+"/revision/{rev}", rev != "" && tail.len() == 0, methods{http.MethodGet: func(w http.ResponseWriter, r *http.Request) { h.repoInfo(w, r, ref, rev) }})
	case "tree":
		rev, tail := takeRevision(args)
		h.route(w, r, prefix+"/tree/{rev}/{path}", rev != "", methods{http.MethodGet: func(w http.ResponseWriter, r *http.Request) { h.tree(w, r, ref, rev, tail.joined(0)) }})
	case "paths-info":
		rev, tail := takeRevision(args)
		h.route(w, r, prefix+"/paths-info/{rev}", rev != "" && tail.len() == 0, methods{http.MethodPost: func(w http.ResponseWriter, r *http.Request) { h.pathsInfo(w, r, ref, rev) }})
	case "refs":
		h.route(w, r, prefix+"/refs", args.len() == 0, methods{http.MethodGet: func(w http.ResponseWriter, r *http.Request) { h.refs(w, r, ref) }})
	case "commits":
		rev, tail := takeRevision(args)
		h.route(w, r, prefix+"/commits/{rev}", tail.len() == 0, methods{http.MethodGet: func(w http.ResponseWriter, r *http.Request) { h.commits(w, r, ref, rev) }})
	case "branch":
		branch := args.joined(0)
		h.route(w, r, prefix+"/branch/{branch}", branch != "", methods{
			http.MethodPost:   func(w http.ResponseWriter, r *http.Request) { h.createBranch(w, r, ref, branch) },
			http.MethodDelete: func(w http.ResponseWriter, r *http.Request) { h.deleteBranch(w, r, ref, branch) },
		})
	case "tag":
		name := args.joined(0)
		h.route(w, r, prefix+"/tag/{rev}", name != "", methods{
			http.MethodPost:   func(w http.ResponseWriter, r *http.Request) { h.createTag(w, r, ref, name) },
			http.MethodDelete: func(w http.ResponseWriter, r *http.Request) { h.deleteTag(w, r, ref, name) },
		})
	case "settings":
		h.route(w, r, prefix+"/settings", args.len() == 0, methods{http.MethodPut: func(w http.ResponseWriter, r *http.Request) { h.updateSettings(w, r, ref) }})
	case "preupload":
		rev, tail := takeRevision(args)
		h.route(w, r, prefix+"/preupload/{rev}", rev != "" && tail.len() == 0, methods{http.MethodPost: func(w http.ResponseWriter, r *http.Request) { h.preupload(w, r, ref, rev) }})
	case "commit":
		rev, tail := takeRevision(args)
		h.route(w, r, prefix+"/commit/{rev}", rev != "" && tail.len() == 0, methods{http.MethodPost: func(w http.ResponseWriter, r *http.Request) { h.commit(w, r, ref, rev) }})
	case "discussions":
		h.serveDiscussions(w, r, ref, prefix+"/discussions", args)
	case "likers":
		h.route(w, r, prefix+"/likers", args.len() == 0, methods{http.MethodGet: func(w http.ResponseWriter, r *http.Request) { h.likers(w, r, ref) }})
	case "like":
		h.route(w, r, prefix+"/like", args.len() == 0, methods{
			http.MethodPost:   func(w http.ResponseWriter, r *http.Request) { h.like(w, r, ref, true) },
			http.MethodDelete: func(w http.ResponseWriter, r *http.Request) { h.like(w, r, ref, false) },
		})
	case "xet-write-token":
		rev, tail := takeRevision(args)
		h.route(w, r, prefix+"/xet-write-token/{rev}", rev != "" && tail.len() == 0, methods{http.MethodGet: func(w http.ResponseWriter, r *http.Request) { h.xetWriteToken(w, r, ref, rev) }})
	case "parquet":
		if t != model.RepoTypeDataset {
			h.notFound(w, r)
			return
		}
		h.route(w, r, prefix+"/parquet/{config}/{split}/{n}", args.len() <= 3, methods{http.MethodGet: func(w http.ResponseWriter, r *http.Request) { h.parquet(w, r, ref, args.segs) }})
	case "croissant":
		h.route(w, r, prefix+"/croissant", t == model.RepoTypeDataset && args.len() == 0, methods{http.MethodGet: func(w http.ResponseWriter, r *http.Request) { h.croissant(w, r, ref) }})
	default:
		// xet-read-token is deliberately absent: Hangar never offers Xet
		// downloads, so it answers 404 like any unknown path (contract §8.2).
		h.notFound(w, r)
	}
}
