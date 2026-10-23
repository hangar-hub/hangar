package hfapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

// listPageSize is the page size of /api/models and /api/datasets when the
// client sets no limit; pages follow the Link header.
const listPageSize = 100

// sortFields maps the Hub's sort keys to repo.ListOptions.Sort.
var sortFields = map[string]string{
	"lastModified": "updated", "last_modified": "updated", "updatedAt": "updated",
	"createdAt": "created", "created_at": "created",
	"downloads": "downloads", "likes": "likes", "trendingScore": "downloads", "trending_score": "downloads",
	"id": "name", "name": "name",
}

// listRepos serves GET /api/models and GET /api/datasets (contract §5).
func (h *Handler) listRepos(w http.ResponseWriter, r *http.Request, t model.RepoType) {
	q := r.URL.Query()
	opts := repo.ListOptions{Viewer: httpx.Principal(r.Context()), Type: t, Desc: true, Cursor: q.Get("cursor")}
	opts.Search = strings.Join(q["search"], " ")
	if a := q.Get("author"); a != "" {
		opts.Namespaces = []string{a}
	}
	for _, f := range httpx.QueryList(q, "filter") {
		key, value, found := strings.Cut(f, ":")
		switch {
		case found && key == "library":
			opts.Libraries = append(opts.Libraries, value)
		case found && (key == "license" || key == "licence"):
			opts.Licences = append(opts.Licences, value)
		case found && (key == "task_categories" || key == "pipeline_tag"):
			opts.Tasks = append(opts.Tasks, value)
		default:
			opts.Tags = append(opts.Tags, f)
		}
	}
	if pt := q.Get("pipeline_tag"); pt != "" {
		opts.Tasks = append(opts.Tasks, pt)
	}
	if s := q.Get("sort"); s != "" {
		field, ok := sortFields[s]
		if !ok {
			h.writeBadRequest(w, r, "Unknown sort key "+strconv.Quote(s)+". Use lastModified, createdAt, downloads, likes or trendingScore.")
			return
		}
		opts.Sort = field
	} else {
		opts.Sort = "updated"
	}
	// Older clients send direction=-1 for descending; 1 asks for ascending.
	if q.Get("direction") == "1" {
		opts.Desc = false
	}
	limitSet := q.Has("limit")
	opts.Limit = httpx.QueryInt(q, "limit", listPageSize, 1, 1000)
	full := httpx.QueryBool(q, "full", false)
	iopts := infoOpts{list: true, siblings: full, cardData: httpx.QueryBool(q, "cardData", false), config: httpx.QueryBool(q, "config", false)}
	if ex := httpx.QueryList(q, "expand"); len(ex) > 0 {
		iopts.expand = map[string]bool{}
		for _, e := range ex {
			iopts.expand[e] = true
		}
	}
	res, err := h.d.Repos.List(r.Context(), opts)
	if err != nil {
		if errors.Is(err, repo.ErrInvalid) {
			h.writeBadRequest(w, r, errMessage(err))
			return
		}
		h.internalError(w, r, err)
		return
	}
	needSHA := full || iopts.want("sha", false) || iopts.want("siblings", false) || iopts.want("cardData", iopts.cardData) ||
		iopts.want("config", iopts.config) || iopts.want("tags", true) || iopts.want("library_name", t == model.RepoTypeModel) ||
		iopts.want("pipeline_tag", t == model.RepoTypeModel) || iopts.want("safetensors", false) || iopts.want("gguf", false)
	pg, err := h.loadListPage(r.Context(), res.Repos, needSHA, iopts)
	if err != nil {
		h.internalError(w, r, err)
		return
	}
	iopts.page = pg
	out := make([]map[string]any, 0, len(res.Repos))
	for _, rp := range res.Repos {
		var sha model.CommitSHA
		switch {
		case needSHA && pg.heads != nil:
			sha = pg.heads[rp.UUID]
		case needSHA:
			s, err := h.d.Repos.Resolve(r.Context(), rp.ID, rp.DefaultBranch)
			if err != nil && !errors.Is(err, repo.ErrRevisionNotFound) {
				h.internalError(w, r, err)
				return
			}
			sha = s
		}
		item, err := h.buildInfo(r.Context(), rp, sha, iopts)
		if err != nil {
			h.internalError(w, r, err)
			return
		}
		out = append(out, item)
	}
	// With a limit the Hub answers one page; without one, clients follow Link.
	if res.NextCursor != "" && !limitSet {
		next := url.Values{}
		for k, v := range q {
			next[k] = v
		}
		next.Set("cursor", res.NextCursor)
		httpx.SetLinkNext(w, httpx.AbsoluteURL(h.d.Settings.PublicURL, r.URL.EscapedPath(), next))
	}
	httpx.WriteJSON(w, r, http.StatusOK, out)
}
