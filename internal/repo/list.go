package repo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/jamesagarside/hangar/internal/gitstore"
	"github.com/jamesagarside/hangar/internal/index"
	"github.com/jamesagarside/hangar/internal/model"
)

// Facet names in ListResult.Facets.
const (
	FacetFormat     = "format"
	FacetTask       = "task"
	FacetLibrary    = "library"
	FacetQuant      = "quant"
	FacetLicence    = "licence"
	FacetModality   = "modality"
	FacetLanguage   = "language"
	FacetSize       = "size_category"
	FacetMirror     = "mirror"
	FacetType       = "type"
	FacetNamespaces = "namespace"
)

// needsFacts reports whether List must read the index or community data.
func (o ListOptions) needsFacts() bool {
	return len(o.Formats)+len(o.Tasks)+len(o.Libraries)+len(o.Quants)+len(o.Licences)+
		len(o.Modalities)+len(o.Languages)+len(o.SizeCategories) > 0 ||
		o.ParamsMin > 0 || o.ParamsMax > 0 || o.ContextMin > 0 || o.RowsMin > 0 || o.RowsMax > 0 || o.IncludeFacets ||
		o.Sort == "parameters" || o.Sort == "downloads" || o.Sort == "likes"
}

// List implements Service. Plain listings are filtered, sorted and paged in
// the database. Metadata filters, facets and the parameters, downloads and
// likes sorts read each candidate's index (at its default branch head) and
// community counts, so they need Options.Index and Options.Community.
func (s *Store) List(ctx context.Context, opts ListOptions) (*ListResult, error) {
	switch opts.Sort {
	case "", "name", "created", "updated", "size", "parameters", "downloads", "likes":
	default:
		return nil, fmt.Errorf("%w: unknown sort %q", ErrInvalid, opts.Sort)
	}
	if opts.Type != "" && !opts.Type.Valid() {
		return nil, fmt.Errorf("%w: unknown repo type %q", ErrInvalid, opts.Type)
	}
	offset := 0
	if opts.Cursor != "" {
		n, err := strconv.Atoi(opts.Cursor)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%w: invalid cursor", ErrInvalid)
		}
		offset = n
	}
	q, err := s.listQuery(ctx, opts)
	if err != nil {
		return nil, err
	}
	if !opts.needsFacts() {
		q = q.OrderExpr(s.sortExpr(opts.Sort, opts.Desc))
		if opts.Limit > 0 {
			q = q.Limit(int64(opts.Limit) + 1)
		}
		q = q.Offset(int64(offset))
		var rows []repoRow
		if err := q.Scan(ctx, &rows); err != nil {
			return nil, fmt.Errorf("repo: list: %w", err)
		}
		res := &ListResult{Repos: make([]*Repo, 0, len(rows))}
		if opts.Limit > 0 && len(rows) > opts.Limit {
			rows = rows[:opts.Limit]
			res.NextCursor = strconv.Itoa(offset + opts.Limit)
		}
		for i := range rows {
			res.Repos = append(res.Repos, toRepo(&rows[i]))
		}
		return res, nil
	}
	if s.opts.Index == nil || (s.opts.Community == nil && (opts.Sort == "downloads" || opts.Sort == "likes")) {
		return nil, fmt.Errorf("%w: metadata filters, facets and this sort need the index and community services", ErrInvalid)
	}
	var rows []repoRow
	if err := q.Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("repo: list: %w", err)
	}
	type cand struct {
		r     *Repo
		facts repoFacts
	}
	cands := make([]cand, 0, len(rows))
	facets := map[string]map[string]int{}
	for i := range rows {
		r := toRepo(&rows[i])
		f, err := s.facts(ctx, r, opts.Sort)
		if err != nil {
			return nil, err
		}
		if !f.matches(opts) {
			continue
		}
		if opts.IncludeFacets {
			f.count(facets, r)
		}
		cands = append(cands, cand{r: r, facts: f})
	}
	sortKey := func(c cand) (int64, string) {
		switch opts.Sort {
		case "parameters":
			return c.facts.parameters, ""
		case "downloads":
			return c.facts.downloads, ""
		case "likes":
			return c.facts.likes, ""
		case "size":
			return c.r.SizeBytes, ""
		case "created":
			return c.r.CreatedAt.UnixNano(), ""
		case "updated":
			return c.r.LastModified.UnixNano(), ""
		default:
			return 0, strings.ToLower(c.r.ID.String())
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, as := sortKey(cands[i])
		b, bs := sortKey(cands[j])
		if opts.Desc {
			a, b, as, bs = b, a, bs, as
		}
		if a != b {
			return a < b
		}
		return as < bs
	})
	res := &ListResult{Repos: []*Repo{}}
	if opts.IncludeFacets {
		res.Facets = facets
	}
	if offset < len(cands) {
		cands = cands[offset:]
	} else {
		cands = nil
	}
	if opts.Limit > 0 && len(cands) > opts.Limit {
		cands = cands[:opts.Limit]
		res.NextCursor = strconv.Itoa(offset + opts.Limit)
	}
	for _, c := range cands {
		res.Repos = append(res.Repos, c.r)
	}
	return res, nil
}

func (s *Store) sortExpr(sortBy string, desc bool) string {
	dir := " ASC"
	if desc {
		dir = " DESC"
	}
	switch sortBy {
	case "created":
		return "repo.created_at" + dir + ", repo.id"
	case "updated":
		return "COALESCE(repo.last_modified_at, repo.created_at)" + dir + ", repo.id"
	case "size":
		return "repo.size_bytes" + dir + ", repo.id"
	default:
		return "lower(ns.name)" + dir + ", lower(repo.name)" + dir + ", repo.id"
	}
}

// listQuery applies the filters the database can check: deletion, read
// access, Token restrictions, type, Namespaces, search, Mirror and tags.
func (s *Store) listQuery(ctx context.Context, opts ListOptions) (*bun.SelectQuery, error) {
	q := s.selectRepo(s.db).Where("repo.deleted_at IS NULL")
	p := opts.Viewer
	if !p.IsAdmin || p.IsAnonymous() {
		var nsIDs []uuid.UUID
		if p.IsUser() && p.UserID != uuid.Nil {
			if p.NamespaceID != uuid.Nil {
				nsIDs = append(nsIDs, p.NamespaceID)
			}
			ms, err := s.ns.MembershipsOf(ctx, p.UserID)
			if err != nil {
				return nil, fmt.Errorf("repo: list memberships: %w", err)
			}
			for _, m := range ms {
				nsIDs = append(nsIDs, m.Org.ID)
			}
		}
		q = q.WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
			if !p.IsAnonymous() || s.opts.AnonymousRead {
				q = q.WhereOr("repo.visibility = ?", string(model.VisibilityPublic))
			}
			if !p.IsAnonymous() {
				q = q.WhereOr("repo.visibility = ?", string(model.VisibilityInternal))
			}
			if len(nsIDs) > 0 {
				q = q.WhereOr("repo.namespace_id IN (?)", bun.List(nsIDs))
			}
			if p.IsUser() && p.UserID != uuid.Nil {
				q = q.WhereOr("repo.id IN (SELECT repo_id FROM collaborators WHERE user_id = ?)", p.UserID)
			}
			// Keeps the group valid when nothing above applies.
			return q.WhereOr("1 = 0")
		})
	}
	if p.Token != nil && !p.Token.Restrictions.IsZero() {
		r := p.Token.Restrictions
		q = q.WhereGroup(" AND ", func(q *bun.SelectQuery) *bun.SelectQuery {
			if len(r.Namespaces) > 0 {
				q = q.WhereOr("lower(ns.name) IN (?)", bun.List(lowerAll(r.Namespaces)))
			}
			if len(r.Repos) > 0 {
				q = q.WhereOr("repo.type || ':' || lower(ns.name) || '/' || lower(repo.name) IN (?)", bun.List(lowerAll(r.Repos)))
			}
			return q.WhereOr("1 = 0")
		})
	}
	if opts.Type != "" {
		q = q.Where("repo.type = ?", string(opts.Type))
	}
	if len(opts.Namespaces) > 0 {
		q = q.Where("lower(ns.name) IN (?)", bun.List(lowerAll(opts.Namespaces)))
	}
	if opts.Search != "" {
		fn := "instr"
		if !s.db.IsSQLite() {
			fn = "strpos"
		}
		q = q.Where(fn+"(lower(ns.name || '/' || repo.name), ?) > 0", strings.ToLower(opts.Search))
	}
	if opts.Mirror != nil {
		if *opts.Mirror {
			q = q.Where(s.mirrorExpr())
		} else {
			q = q.Where("NOT (" + s.mirrorExpr() + ")")
		}
	}
	for _, t := range opts.Tags {
		q = q.Where(s.tagExpr(), t)
	}
	if opts.SizeMax > 0 {
		q = q.Where("repo.size_bytes <= ?", opts.SizeMax)
	}
	return q, nil
}

func (s *Store) mirrorExpr() string {
	if s.db.IsSQLite() {
		return "COALESCE(json_extract(repo.settings, '$.mirror'), 0) = 1"
	}
	return "COALESCE((repo.settings->>'mirror')::boolean, false)"
}

func (s *Store) tagExpr() string {
	if s.db.IsSQLite() {
		return "EXISTS (SELECT 1 FROM json_each(repo.tags) WHERE json_each.value = ?)"
	}
	return "repo.tags @> jsonb_build_array(CAST(? AS text))"
}

// repoFacts are the values List filters, sorts and counts facets on.
type repoFacts struct {
	formats, tasks, libraries, quants, licences []string
	modalities, languages                       []string
	sizeCategory                                string
	parameters, contextLength, rows             int64
	downloads, likes                            int64
}

func (s *Store) facts(ctx context.Context, r *Repo, sortBy string) (repoFacts, error) {
	var f repoFacts
	if r.Licence != "" {
		f.licences = append(f.licences, r.Licence)
	}
	head, ok, err := gitstore.LookupRef(ctx, s.db, r.UUID, model.BranchRef(r.DefaultBranch))
	if err != nil {
		return f, err
	}
	if ok {
		sum, err := s.opts.Index.Get(ctx, r.UUID, head)
		switch {
		case err == nil:
			f.fromSummary(sum)
		case errors.Is(err, index.ErrNotIndexed):
		default:
			return f, err
		}
	}
	if s.opts.Community != nil && (sortBy == "downloads" || sortBy == "likes") {
		if f.likes, err = s.opts.Community.Likes(ctx, r.UUID); err != nil {
			return f, err
		}
		days, err := s.opts.Community.Downloads(ctx, r.UUID, s.now().AddDate(0, 0, -30).Truncate(24*time.Hour))
		if err != nil {
			return f, err
		}
		for _, d := range days {
			f.downloads += d.Count
		}
	}
	return f, nil
}

func (f *repoFacts) fromSummary(sum *index.Summary) {
	f.formats = sum.Formats
	if sum.PipelineTag != "" {
		f.tasks = append(f.tasks, sum.PipelineTag)
	}
	if sum.LibraryName != "" {
		f.libraries = append(f.libraries, sum.LibraryName)
	}
	for _, q := range sum.Quantisations {
		if q.Name != "" && !slices.Contains(f.quants, q.Name) {
			f.quants = append(f.quants, q.Name)
		}
	}
	if sum.Licence.ID != "" && !slices.Contains(f.licences, sum.Licence.ID) {
		f.licences = append(f.licences, sum.Licence.ID)
	}
	if sum.Parameters != nil {
		f.parameters = sum.Parameters.Total
	}
	f.contextLength = sum.ContextLength
	if d := sum.Dataset; d != nil {
		f.modalities = d.Modalities
		f.languages = d.Languages
		f.sizeCategory = d.SizeCategory
		f.rows = d.Rows
		f.tasks = append(f.tasks, d.Tasks...)
		for _, fm := range d.Formats {
			if !slices.Contains(f.formats, fm) {
				f.formats = append(f.formats, fm)
			}
		}
	}
}

func anyFold(have, want []string) bool {
	if len(want) == 0 {
		return true
	}
	for _, w := range want {
		for _, h := range have {
			if strings.EqualFold(h, w) {
				return true
			}
		}
	}
	return false
}

func (f repoFacts) matches(o ListOptions) bool {
	switch {
	case !anyFold(f.formats, o.Formats), !anyFold(f.tasks, o.Tasks), !anyFold(f.libraries, o.Libraries),
		!anyFold(f.quants, o.Quants), !anyFold(f.licences, o.Licences), !anyFold(f.modalities, o.Modalities),
		!anyFold(f.languages, o.Languages):
		return false
	case len(o.SizeCategories) > 0 && !anyFold([]string{f.sizeCategory}, o.SizeCategories):
		return false
	case o.ParamsMin > 0 && f.parameters < o.ParamsMin, o.ParamsMax > 0 && (f.parameters == 0 || f.parameters > o.ParamsMax):
		return false
	case o.ContextMin > 0 && f.contextLength < o.ContextMin:
		return false
	case o.RowsMin > 0 && f.rows < o.RowsMin, o.RowsMax > 0 && (f.rows == 0 || f.rows > o.RowsMax):
		return false
	}
	return true
}

func (f repoFacts) count(facets map[string]map[string]int, r *Repo) {
	add := func(name string, values ...string) {
		m := facets[name]
		if m == nil {
			m = map[string]int{}
			facets[name] = m
		}
		seen := map[string]bool{}
		for _, v := range values {
			if v != "" && !seen[v] {
				seen[v] = true
				m[v]++
			}
		}
	}
	add(FacetFormat, f.formats...)
	add(FacetTask, f.tasks...)
	add(FacetLibrary, f.libraries...)
	add(FacetQuant, f.quants...)
	add(FacetLicence, f.licences...)
	add(FacetModality, f.modalities...)
	add(FacetLanguage, f.languages...)
	add(FacetSize, f.sizeCategory)
	add(FacetMirror, strconv.FormatBool(r.Mirror))
	add(FacetType, string(r.ID.Type))
	add(FacetNamespaces, r.ID.Namespace)
}

// CountByNamespace returns how many live Repos each Namespace holds, for
// Namespace profiles.
func (s *Store) CountByNamespace(ctx context.Context, namespaceID uuid.UUID, viewer model.Principal) (int, error) {
	q, err := s.listQuery(ctx, ListOptions{Viewer: viewer})
	if err != nil {
		return 0, err
	}
	n, err := q.Where("repo.namespace_id = ?", namespaceID).Count(ctx)
	return int(n), err
}
