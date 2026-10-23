package hfapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/index"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/scan"
)

// infoOpts says which fields a Repo info object carries.
type infoOpts struct {
	// expand lists the requested properties; nil means the default set.
	expand map[string]bool
	// blobs adds size, blobId and lfs to siblings (`blobs=true`).
	blobs bool
	// security adds securityRepoStatus (`securityStatus=true`).
	security bool
	// siblings, cardData and config add those fields to list items
	// (`full`, `cardData` and `config` on /api/models).
	siblings, cardData, config bool
	// list is true for /api/models and /api/datasets items, which carry
	// fewer fields by default than Repo info.
	list bool
	// page holds data read for a whole page of list items; nil reads it
	// for each Repo.
	page *listPage
}

// want reports whether property name belongs in the object.
func (o infoOpts) want(name string, inDefault bool) bool {
	if o.expand != nil {
		return o.expand[name]
	}
	return inDefault
}

// repoInfo serves GET /api/{models|datasets}/{repo}[/revision/{rev}]
// (contract §2).
func (h *Handler) repoInfo(w http.ResponseWriter, r *http.Request, ref repoRef, rev string) {
	rp, ok := h.load(w, r, ref, auth.ActionRead)
	if !ok {
		return
	}
	q := r.URL.Query()
	opts := infoOpts{
		blobs:    httpx.QueryBool(q, "blobs", false),
		security: httpx.QueryBool(q, "securityStatus", false),
		siblings: true,
	}
	if ex := httpx.QueryList(q, "expand"); len(ex) > 0 {
		opts.expand = map[string]bool{}
		for _, e := range ex {
			opts.expand[e] = true
		}
	}
	var sha model.CommitSHA
	if rev != "" {
		if sha, ok = h.resolveRevision(w, r, rp, rev); !ok {
			return
		}
	} else {
		s, err := h.d.Repos.Resolve(r.Context(), rp.ID, rp.DefaultBranch)
		if errors.Is(err, repo.ErrRevisionNotFound) && h.awaitsHistory(r, rp) {
			s, err = h.resolveAfterHistory(r, rp, rp.DefaultBranch)
		}
		switch {
		case err == nil:
			sha = s
		case errors.Is(err, repo.ErrRevisionNotFound):
			// A Mirror being planned without Pull-through, or one that is
			// empty on the Upstream, has no Commits yet.
		case errors.Is(err, ErrPullPending):
			pullPending(w, r, rp.ID)
			return
		default:
			h.serviceError(w, r, rp.ID, err)
			return
		}
	}
	info, err := h.buildInfo(r.Context(), rp, sha, opts)
	if err != nil {
		h.serviceError(w, r, rp.ID, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, info)
}

// buildInfo assembles a Repo info object. sha may be empty for a Repo
// with no Commits.
func (h *Handler) buildInfo(ctx context.Context, rp *repo.Repo, sha model.CommitSHA, o infoOpts) (map[string]any, error) {
	isModel := rp.ID.Type == model.RepoTypeModel
	out := map[string]any{"_id": rp.UUID.String(), "id": rp.ID.String()}
	if isModel && (o.expand == nil) {
		out["modelId"] = rp.ID.String()
	}
	var summary *index.Summary
	needSummary := o.want("tags", true) || o.want("cardData", !o.list || o.cardData) || o.want("config", isModel && (!o.list || o.config)) ||
		o.want("library_name", isModel) || o.want("pipeline_tag", isModel) || o.want("safetensors", isModel && !o.list) || o.want("gguf", isModel && !o.list) ||
		o.want("baseModels", false) || o.want("model-index", false)
	if sha != "" && needSummary {
		if o.page != nil && o.page.summaries != nil {
			summary = o.page.summaries[rp.UUID]
		} else {
			s, err := h.d.Index.Get(ctx, rp.UUID, sha)
			switch {
			case err == nil:
				summary = s
			case errors.Is(err, index.ErrNotIndexed):
			default:
				return nil, err
			}
		}
	}
	lastModified := rp.LastModified
	if lastModified.IsZero() {
		lastModified = rp.CreatedAt
	}
	if o.want("author", true) {
		out["author"] = rp.ID.Namespace
	}
	if o.want("sha", !o.list || o.siblings) && sha != "" {
		out["sha"] = string(sha)
	}
	if o.want("lastModified", true) {
		out["lastModified"] = model.NewHubTime(lastModified)
	}
	if o.want("createdAt", true) {
		out["createdAt"] = model.NewHubTime(rp.CreatedAt)
	}
	if o.want("private", true) {
		out["private"] = rp.Visibility.Private()
	}
	if o.want("gated", true) {
		out["gated"] = gatedValue(rp.Gating)
	}
	if o.want("disabled", true) {
		out["disabled"] = false
	}
	if o.want("downloads", true) || o.want("downloadsAllTime", false) {
		var recent, all int64
		if o.page != nil && o.page.recent != nil {
			recent, all = o.page.recent[rp.UUID], o.page.all[rp.UUID]
		} else {
			var err error
			if recent, all, err = h.downloads(ctx, rp, o.want("downloadsAllTime", false)); err != nil {
				return nil, err
			}
		}
		if o.want("downloads", true) {
			out["downloads"] = recent
		}
		if o.want("downloadsAllTime", false) {
			out["downloadsAllTime"] = all
		}
	}
	if o.want("likes", true) {
		if o.page != nil && o.page.likes != nil {
			out["likes"] = o.page.likes[rp.UUID]
		} else {
			n, err := h.d.Community.Likes(ctx, rp.UUID)
			if err != nil {
				return nil, err
			}
			out["likes"] = n
		}
	}
	if o.want("trendingScore", false) {
		out["trendingScore"] = 0
	}
	if o.want("tags", true) {
		out["tags"] = mergeTags(rp.Tags, summary)
	}
	if o.want("usedStorage", !o.list) {
		out["usedStorage"] = rp.SizeBytes
	}
	if !isModel && o.want("description", !o.list) {
		out["description"] = rp.Description
	}
	if summary != nil {
		addSummaryFields(out, summary, isModel, o)
	}
	if isModel && o.want("spaces", !o.list) {
		out["spaces"] = []string{}
	}
	if o.want("siblings", o.siblings) {
		siblings := []map[string]any{}
		if sha != "" {
			entries, err := h.d.Repos.Tree(ctx, rp.ID, sha, "", repo.TreeOpts{Recursive: true})
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if e.Type != model.EntryFile {
					continue
				}
				s := map[string]any{"rfilename": e.Path}
				if o.blobs {
					s["size"] = e.Size
					s["blobId"] = e.OID
					if e.LFS != nil {
						s["lfs"] = map[string]any{"sha256": e.LFS.OID, "size": e.LFS.Size, "pointerSize": e.LFS.PointerSize}
					}
				}
				siblings = append(siblings, s)
			}
		}
		out["siblings"] = siblings
	}
	if o.security && sha != "" {
		st, err := h.securityRepoStatus(ctx, rp, sha)
		if err != nil {
			return nil, err
		}
		out["securityRepoStatus"] = st
	}
	return out, nil
}

// addSummaryFields maps index results to the Hub's fields. Readers never
// wait for indexing: without results these fields are left out
// (07-indexing-and-scanning.md).
func addSummaryFields(out map[string]any, s *index.Summary, isModel bool, o infoOpts) {
	if s.CardData != nil && o.want("cardData", !o.list || o.cardData) {
		out["cardData"] = s.CardData
	}
	if !isModel {
		return
	}
	if s.LibraryName != "" && o.want("library_name", true) {
		out["library_name"] = s.LibraryName
	}
	if s.PipelineTag != "" && o.want("pipeline_tag", true) {
		out["pipeline_tag"] = s.PipelineTag
	}
	if len(s.Config) > 0 && json.Valid(s.Config) && o.want("config", !o.list || o.config) {
		out["config"] = s.Config
	}
	if s.Parameters != nil && slices.Contains(s.Formats, "safetensors") && o.want("safetensors", !o.list) {
		params := s.Parameters.ByDType
		if params == nil {
			params = map[string]int64{}
		}
		out["safetensors"] = map[string]any{"parameters": params, "total": s.Parameters.Total}
	}
	if s.GGUF != nil && o.want("gguf", !o.list) {
		g := map[string]any{"total": s.GGUF.Total}
		setIf(g, "architecture", s.GGUF.Architecture)
		if s.GGUF.ContextLength > 0 {
			g["context_length"] = s.GGUF.ContextLength
		}
		setIf(g, "chat_template", s.GGUF.ChatTemplate)
		setIf(g, "bos_token", s.GGUF.BOSToken)
		setIf(g, "eos_token", s.GGUF.EOSToken)
		out["gguf"] = g
	}
	if o.want("baseModels", false) && s.CardData != nil {
		if bm, ok := s.CardData["base_model"]; ok {
			out["baseModels"] = bm
		}
	}
	if o.want("model-index", false) && s.CardData != nil {
		if mi, ok := s.CardData["model-index"]; ok {
			out["model-index"] = mi
		}
	}
}

func setIf(m map[string]any, k, v string) {
	if v != "" {
		m[k] = v
	}
}

// gatedValue is the Hub's `gated` field: false, "auto" or "manual".
func gatedValue(g repo.Gating) any {
	if !g.Enabled() {
		return false
	}
	return g.Mode
}

func mergeTags(repoTags []string, s *index.Summary) []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(tags []string) {
		for _, t := range tags {
			if t != "" && !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	add(repoTags)
	if s != nil {
		add(s.Tags)
	}
	return out
}

// downloads returns the download count of the last 30 days, as the Hub's
// `downloads`, and when asked the all-time count.
func (h *Handler) downloads(ctx context.Context, rp *repo.Repo, allTime bool) (recent, all int64, err error) {
	since := h.now().UTC().AddDate(0, 0, -30)
	from := since
	if allTime {
		from = time.Time{}
	}
	days, err := h.d.Community.Downloads(ctx, rp.UUID, from)
	if err != nil {
		return 0, 0, err
	}
	for _, d := range days {
		all += d.Count
		if !d.Day.Before(since.Truncate(24 * time.Hour)) {
			recent += d.Count
		}
	}
	return recent, all, nil
}

// securityRepoStatus is the Hub's `securityRepoStatus`: whether scans ran
// and which files have findings.
func (h *Handler) securityRepoStatus(ctx context.Context, rp *repo.Repo, sha model.CommitSHA) (map[string]any, error) {
	res, err := h.d.Scan.Results(ctx, rp.UUID, sha)
	if errors.Is(err, scan.ErrNotScanned) {
		return map[string]any{"scansDone": false, "filesWithIssues": []any{}}, nil
	}
	if err != nil {
		return nil, err
	}
	issues := []map[string]any{}
	for _, f := range res.Files {
		if level := issueLevel(f.Status); level != "" {
			issues = append(issues, map[string]any{"path": f.Path, "level": level})
		}
	}
	return map[string]any{"scansDone": true, "filesWithIssues": issues}, nil
}

func issueLevel(s scan.Status) string {
	switch s {
	case scan.StatusUnsafe:
		return "danger"
	case scan.StatusSuspicious:
		return "caution"
	case scan.StatusError:
		return "error"
	default:
		return ""
	}
}
