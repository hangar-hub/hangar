package repo

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/index"
	"github.com/hangar-hub/hangar/internal/model"
)

// factsVersion is the version of how repo_facts rows are built from index
// results. Raising it rebuilds every row on the next List.
const factsVersion = 1

// factsBatch is how many repo_facts rows List rebuilds at a time.
const factsBatch = 500

// The facets stored in repo_facet_values. The mirror, type and namespace
// facets come from the repos table.
var storedFacets = []string{FacetFormat, FacetTask, FacetLibrary, FacetQuant, FacetLicence, FacetModality, FacetLanguage, FacetSize}

// listFacts is List with metadata filters, facets or the parameters,
// downloads or likes sort, answered in SQL from repo_facts and
// repo_facet_values (Options.SQLFacets). It gives the same Repos, order,
// cursors and facet counts as reading every candidate's facts, for a
// number of queries that does not grow with the number of Repos.
func (s *Store) listFacts(ctx context.Context, opts ListOptions, offset int) (*ListResult, error) {
	if err := s.refreshFacts(ctx); err != nil {
		return nil, err
	}
	f, err := s.newListFilter(ctx, opts)
	if err != nil {
		return nil, err
	}
	res := &ListResult{Repos: []*Repo{}}
	if opts.IncludeFacets {
		if res.Facets, err = s.factFacets(ctx, f); err != nil {
			return nil, err
		}
	}
	q := s.matchFacts(f, s.selectRepo(s.db))
	key, args := s.factSortKey(opts.Sort)
	dir := " ASC"
	if opts.Desc {
		dir = " DESC"
	}
	// Repos with the same sort key keep the order they were created in.
	q = q.OrderExpr(key+dir+", repo.id ASC", args...)
	if opts.Limit > 0 {
		q = q.Limit(int64(opts.Limit) + 1)
	}
	q = q.Offset(int64(offset))
	var rows []repoRow
	if err := q.Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("repo: list: %w", err)
	}
	if opts.Limit > 0 && len(rows) > opts.Limit {
		rows = rows[:opts.Limit]
		res.NextCursor = strconv.Itoa(offset + opts.Limit)
	}
	for i := range rows {
		res.Repos = append(res.Repos, toRepo(&rows[i]))
	}
	return res, nil
}

// fromRepos starts a query over repos AS repo joined to namespaces AS ns.
func (s *Store) fromRepos() *bun.SelectQuery {
	return s.db.NewSelect().TableExpr("repos AS repo").Join("JOIN namespaces AS ns ON ns.id = repo.namespace_id")
}

// matchFacts applies the list filters and the metadata filters to q, which
// selects from repos AS repo joined to namespaces AS ns, and joins
// repo_facts AS f.
func (s *Store) matchFacts(f *listFilter, q *bun.SelectQuery) *bun.SelectQuery {
	o := f.opts
	q = f.apply(q.Join("LEFT JOIN repo_facts AS f ON f.repo_id = repo.id"))
	for _, c := range []struct {
		facet string
		want  []string
	}{
		{FacetFormat, o.Formats}, {FacetTask, o.Tasks}, {FacetLibrary, o.Libraries}, {FacetQuant, o.Quants},
		{FacetLicence, o.Licences}, {FacetModality, o.Modalities}, {FacetLanguage, o.Languages}, {FacetSize, o.SizeCategories},
	} {
		if len(c.want) == 0 {
			continue
		}
		folded := make([]string, 0, len(c.want))
		for _, w := range c.want {
			folded = append(folded, foldCase(w))
		}
		q = q.Where("EXISTS (SELECT 1 FROM repo_facet_values AS v WHERE v.repo_id = repo.id AND v.facet = ? AND v.folded IN (?))",
			c.facet, bun.List(folded))
	}
	if o.ParamsMin > 0 {
		q = q.Where("COALESCE(f.parameters, 0) >= ?", o.ParamsMin)
	}
	if o.ParamsMax > 0 {
		q = q.Where("COALESCE(f.parameters, 0) <> 0 AND f.parameters <= ?", o.ParamsMax)
	}
	if o.ContextMin > 0 {
		q = q.Where("COALESCE(f.context_length, 0) >= ?", o.ContextMin)
	}
	if o.RowsMin > 0 {
		q = q.Where("COALESCE(f.num_rows, 0) >= ?", o.RowsMin)
	}
	if o.RowsMax > 0 {
		q = q.Where("COALESCE(f.num_rows, 0) <> 0 AND f.num_rows <= ?", o.RowsMax)
	}
	return q
}

// factSortKey is the ORDER BY expression of a sort, and its arguments.
func (s *Store) factSortKey(sortBy string) (string, []any) {
	switch sortBy {
	case "parameters":
		return "COALESCE(f.parameters, 0)", nil
	case "downloads":
		// The last 30 days, as Community.Downloads counts them.
		return "(SELECT COALESCE(SUM(dc.count), 0) FROM download_counts AS dc WHERE dc.repo_id = repo.id AND dc.day >= ?)",
			[]any{s.downloadsSince()}
	case "likes":
		return "(SELECT COUNT(*) FROM likes AS l WHERE l.repo_id = repo.id)", nil
	case "size":
		return "repo.size_bytes", nil
	case "created":
		return "repo.created_at", nil
	case "updated":
		return "COALESCE(repo.last_modified_at, repo.created_at)", nil
	default:
		// The lower-cased `namespace/name`, in byte order.
		if s.db.IsSQLite() {
			return "lower(ns.name || '/' || repo.name)", nil
		}
		return `lower(ns.name || '/' || repo.name) COLLATE "C"`, nil
	}
}

// downloadsSince is the first day the downloads sort counts.
func (s *Store) downloadsSince() time.Time {
	t := s.now().AddDate(0, 0, -30).Truncate(24 * time.Hour).UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// factFacets counts facet values over the Repos the filters match. Like
// repoFacts.count, it lists every facet, with no values if need be, once
// any Repo matches, and counts each value once per Repo.
func (s *Store) factFacets(ctx context.Context, f *listFilter) (map[string]map[string]int, error) {
	facets := map[string]map[string]int{}
	var groups []struct {
		Type      string `bun:"type"`
		Mirror    string `bun:"mirror"`
		Namespace string `bun:"namespace"`
		N         int    `bun:"n"`
	}
	q := s.matchFacts(f, s.fromRepos()).
		ColumnExpr("repo.type AS type").ColumnExpr(s.mirrorFacetExpr() + " AS mirror").ColumnExpr("ns.name AS namespace").
		ColumnExpr("COUNT(*) AS n").GroupExpr("repo.type, " + s.mirrorFacetExpr() + ", ns.name")
	if err := q.Scan(ctx, &groups); err != nil {
		return nil, fmt.Errorf("repo: list facets: %w", err)
	}
	if len(groups) == 0 {
		return facets, nil
	}
	for _, name := range append(slices.Clone(storedFacets), FacetMirror, FacetType, FacetNamespaces) {
		facets[name] = map[string]int{}
	}
	for _, g := range groups {
		facets[FacetType][g.Type] += g.N
		facets[FacetMirror][g.Mirror] += g.N
		facets[FacetNamespaces][g.Namespace] += g.N
	}
	var values []struct {
		Facet string `bun:"facet"`
		Value string `bun:"value"`
		N     int    `bun:"n"`
	}
	ids := s.matchFacts(f, s.fromRepos()).ColumnExpr("repo.id")
	if err := s.db.NewSelect().TableExpr("repo_facet_values AS v").ColumnExpr("v.facet, v.value, COUNT(*) AS n").
		Where("v.value <> ''").Where("v.repo_id IN (?)", ids).GroupExpr("v.facet, v.value").Scan(ctx, &values); err != nil {
		return nil, fmt.Errorf("repo: list facets: %w", err)
	}
	for _, v := range values {
		if m := facets[v.Facet]; m != nil {
			m[v.Value] += v.N
		}
	}
	return facets, nil
}

// mirrorFacetExpr is "true" for a Mirror and "false" otherwise, as
// Repo.Mirror reads the setting: only a JSON true counts.
func (s *Store) mirrorFacetExpr() string {
	if s.db.IsSQLite() {
		return "CASE WHEN json_type(repo.settings, '$.mirror') = 'true' THEN 'true' ELSE 'false' END"
	}
	return "CASE WHEN repo.settings->'mirror' = 'true'::jsonb THEN 'true' ELSE 'false' END"
}

// licenceExpr is the licence setting when it is a string, as Repo.Licence
// reads it, and ” otherwise.
func (s *Store) licenceExpr() string {
	if s.db.IsSQLite() {
		return "CASE WHEN json_type(repo.settings, '$.licence') = 'text' THEN json_extract(repo.settings, '$.licence') ELSE '' END"
	}
	return "CASE WHEN jsonb_typeof(repo.settings->'licence') = 'string' THEN repo.settings->>'licence' ELSE '' END"
}

// RefreshListFacts rebuilds the repo_facts rows that are out of date, and
// returns how many Repos it rebuilt. List does it first, so it never
// answers from an out-of-date row; a worker calls it on start, so the
// first listing after an upgrade does not build every row.
//
// Triggers on repos, refs, metadata and dataset_splits record each Repo
// whose row is out of date in repo_facts_changes (migration 0007). This
// reads those records, rebuilds the Repos' rows from what is current, and
// deletes exactly the records it read: a change that lands meanwhile
// leaves its own record for the next refresh. Rows built by another
// factsVersion are rebuilt too.
func (s *Store) RefreshListFacts(ctx context.Context) (int, error) {
	changes, outdated, err := s.factsWork(ctx)
	if err != nil || len(changes)+len(outdated) == 0 {
		return 0, err
	}
	// Listings that find work wait for one another and then read it
	// again, so they rebuild each row once and none answers before the
	// changes it found are in its rows.
	s.factsMu.Lock()
	defer s.factsMu.Unlock()
	if changes, outdated, err = s.factsWork(ctx); err != nil || len(changes)+len(outdated) == 0 {
		return 0, err
	}
	records := map[uuid.UUID][]int64{}
	for _, c := range changes {
		records[c.RepoID] = append(records[c.RepoID], c.ID)
	}
	for _, id := range outdated {
		if _, ok := records[id]; !ok {
			records[id] = nil
		}
	}
	ids := make([]uuid.UUID, 0, len(records))
	for id := range records {
		ids = append(ids, id)
	}
	// Concurrent refreshes write rows in the same order.
	slices.SortFunc(ids, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	for start := 0; start < len(ids); start += factsBatch {
		if err := s.rebuildFacts(ctx, ids[start:min(start+factsBatch, len(ids))], records); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// factsWork reads the change records and the Repos whose rows another
// factsVersion built.
func (s *Store) factsWork(ctx context.Context) ([]db.RepoFactsChange, []uuid.UUID, error) {
	var changes []db.RepoFactsChange
	if err := s.db.NewSelect().Model(&changes).Column("id", "repo_id").Scan(ctx); err != nil {
		return nil, nil, fmt.Errorf("repo: read list facts changes: %w", err)
	}
	var outdated []uuid.UUID
	if err := s.db.NewSelect().Model((*db.RepoFacts)(nil)).Column("repo_id").
		Where("version < ? OR version > ?", factsVersion, factsVersion).Scan(ctx, &outdated); err != nil {
		return nil, nil, fmt.Errorf("repo: read outdated list facts: %w", err)
	}
	return changes, outdated, nil
}

// refreshFacts is RefreshListFacts for List.
func (s *Store) refreshFacts(ctx context.Context) error {
	_, err := s.RefreshListFacts(ctx)
	return err
}

// factsSource is what a Repo's repo_facts row is built from, besides its
// index results.
type factsSource struct {
	RepoID  uuid.UUID `bun:"repo_id"`
	Head    string    `bun:"head"`
	Licence string    `bun:"licence"`
}

// rebuildFacts writes the repo_facts and repo_facet_values rows of a batch
// of Repos and deletes the batch's change records. Repos that no longer
// exist only lose their records.
func (s *Store) rebuildFacts(ctx context.Context, batch []uuid.UUID, records map[uuid.UUID][]int64) error {
	var sources []factsSource
	if err := s.db.NewSelect().TableExpr("repos AS repo").
		Join("LEFT JOIN refs AS hr ON hr.repo_id = repo.id AND hr.name = "+
			"CASE WHEN substr(repo.default_branch, 1, 5) = 'refs/' THEN repo.default_branch ELSE 'refs/heads/' || repo.default_branch END").
		ColumnExpr("repo.id AS repo_id").ColumnExpr("COALESCE(hr.target, '') AS head").ColumnExpr(s.licenceExpr()+" AS licence").
		Where("repo.id IN (?)", bun.List(batch)).Scan(ctx, &sources); err != nil {
		return fmt.Errorf("repo: read list facts sources: %w", err)
	}
	heads := map[uuid.UUID]model.CommitSHA{}
	for _, src := range sources {
		if src.Head != "" {
			heads[src.RepoID] = model.CommitSHA(src.Head)
		}
	}
	sums, err := index.GetMany(ctx, s.opts.Index, heads)
	if err != nil {
		return err
	}
	slices.SortFunc(sources, func(a, b factsSource) int { return strings.Compare(a.RepoID.String(), b.RepoID.String()) })
	facts := make([]db.RepoFacts, 0, len(sources))
	var values []db.RepoFacetValue
	built := make([]uuid.UUID, 0, len(sources))
	for _, src := range sources {
		var rf repoFacts
		if src.Licence != "" {
			rf.licences = append(rf.licences, src.Licence)
		}
		if sum := sums[src.RepoID]; sum != nil {
			rf.fromSummary(sum)
		}
		built = append(built, src.RepoID)
		facts = append(facts, db.RepoFacts{RepoID: src.RepoID, Version: factsVersion, Parameters: rf.parameters,
			ContextLength: rf.contextLength, NumRows: rf.rows})
		values = append(values, rf.facetValues(src.RepoID)...)
	}
	var done []int64
	for _, id := range batch {
		done = append(done, records[id]...)
	}
	return s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if len(facts) > 0 {
			if _, err := tx.NewInsert().Model(&facts).On("CONFLICT (repo_id) DO UPDATE").
				Set("version = EXCLUDED.version").Set("parameters = EXCLUDED.parameters").
				Set("context_length = EXCLUDED.context_length").Set("num_rows = EXCLUDED.num_rows").
				Set("updated_at = EXCLUDED.updated_at").Exec(ctx); err != nil {
				return fmt.Errorf("repo: store list facts: %w", err)
			}
			if _, err := tx.NewDelete().Model((*db.RepoFacetValue)(nil)).Where("repo_id IN (?)", bun.List(built)).Exec(ctx); err != nil {
				return fmt.Errorf("repo: store list facets: %w", err)
			}
		}
		for start := 0; start < len(values); start += 1000 {
			part := values[start:min(start+1000, len(values))]
			if _, err := tx.NewInsert().Model(&part).Exec(ctx); err != nil {
				return fmt.Errorf("repo: store list facets: %w", err)
			}
		}
		for start := 0; start < len(done); start += 1000 {
			if _, err := tx.NewDelete().Model((*db.RepoFactsChange)(nil)).
				Where("id IN (?)", bun.List(done[start:min(start+1000, len(done))])).Exec(ctx); err != nil {
				return fmt.Errorf("repo: clear list facts changes: %w", err)
			}
		}
		return nil
	})
}

// facetValues are the repo_facet_values rows of one Repo. They keep empty
// values, which filters can match as repoFacts.matches does, while facet
// counts leave them out; the size category always has a row.
func (f repoFacts) facetValues(repoID uuid.UUID) []db.RepoFacetValue {
	var out []db.RepoFacetValue
	add := func(facet string, values ...string) {
		seen := map[string]bool{}
		for _, v := range values {
			if !seen[v] {
				seen[v] = true
				out = append(out, db.RepoFacetValue{RepoID: repoID, Facet: facet, Value: v, Folded: foldCase(v)})
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
	return out
}

// foldCase maps every rune to the smallest rune of its simple case folding
// orbit, so two strings have the same foldCase exactly when
// strings.EqualFold reports them equal.
func foldCase(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		least := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			least = min(least, f)
		}
		b.WriteRune(least)
	}
	return b.String()
}
