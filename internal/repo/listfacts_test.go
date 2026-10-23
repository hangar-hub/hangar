package repo_test

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/community/communitydb"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/index"
	"github.com/hangar-hub/hangar/internal/index/indexdb"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
)

// factsWorld is a database of Repos with index results, dataset Split
// sizes, likes and downloads, written directly, and two Repo services over
// it: one that lists from repo_facts in SQL (Options.SQLFacets) and one
// that reads every candidate's facts, which List did before. Neither reads
// git to list, so the Repos need no git data.
type factsWorld struct {
	e        *env
	sql, mem *repo.Store
	rng      *rand.Rand
	// keys are what each sort orders a Repo by, to put Repos with equal
	// keys in creation order, as the SQL listing does; reading every
	// candidate leaves them in the order the database returned them.
	params, likes, downloads map[uuid.UUID]int64
	repos                    []db.Repo
	likers                   []model.Principal
	viewers                  map[string]model.Principal
}

func newFactsWorld(t *testing.T, d *db.DB) *factsWorld {
	w := &factsWorld{rng: rand.New(rand.NewPCG(1, 2)), params: map[uuid.UUID]int64{}, likes: map[uuid.UUID]int64{},
		downloads: map[uuid.UUID]int64{}, viewers: map[string]model.Principal{}}
	idx := indexdb.New(indexdb.Options{DB: d, DatasetSplits: func(ctx context.Context, repoUUID uuid.UUID, commit model.CommitSHA) ([]indexdb.DatasetSplit, error) {
		var rows []db.DatasetSplit
		if err := d.NewSelect().Model(&rows).Where("repo_id = ?", repoUUID).Where("commit_sha = ?", string(commit)).
			Order("config", "split").Scan(ctx); err != nil {
			return nil, err
		}
		var out []indexdb.DatasetSplit
		for _, r := range rows {
			out = append(out, indexdb.DatasetSplit{Config: r.Config, Split: r.Split, Converted: r.Status == "ready", Rows: *r.NumRows})
		}
		return out, nil
	}})
	comm := communitydb.New(communitydb.Options{DB: d})
	w.e = newEnv(t, d, func(o *repo.Options) {
		o.Index, o.Community, o.SQLFacets = idx, comm, true
		o.AnonymousRead = func(context.Context) (bool, error) { return true, nil }
	})
	w.sql = w.e.svc
	memOpts := w.e.opts
	memOpts.SQLFacets = false
	var err error
	w.mem, err = repo.NewStore(memOpts)
	require.NoError(t, err)
	return w
}

// seed adds n Repos across four Namespaces, with every kind of value the
// facts are built from, including values that differ only in case,
// licences that are not strings, Repos with no branch and Repos that are
// not indexed.
func (w *factsWorld) seed(n int) {
	e, rng := w.e, w.rng
	alice, bob, carol := e.addUser("alice"), e.addUser("bob"), e.addUser("carol")
	lab := e.addOrg("lab", map[uuid.UUID]model.Role{alice.UserID: model.RoleRead})
	other := e.addOrg("Other", nil)
	for i := range 6 {
		w.likers = append(w.likers, e.addUser(fmt.Sprintf("liker%d", i)))
	}
	restricted := alice
	restricted.Method, restricted.Token = model.AuthToken, &model.TokenInfo{Name: "t", Scope: model.ScopeRead,
		Restrictions: model.TokenRestrictions{Namespaces: []string{"LAB"}}}
	w.viewers = map[string]model.Principal{"alice": alice, "carol": carol, "anonymous": {}, "restricted": restricted,
		"admin": {Kind: model.PrincipalUser, UserID: uuid.New(), IsAdmin: true}}
	namespaces := []uuid.UUID{alice.NamespaceID, bob.NamespaceID, lab, other}
	pick := func(vals ...string) string { return vals[rng.IntN(len(vals))] }
	some := func(vals ...string) []string {
		var out []string
		for _, v := range vals {
			if rng.IntN(3) == 0 {
				out = append(out, v)
			}
		}
		return out
	}
	for i := range n {
		r := db.Repo{ID: db.NewID(), NamespaceID: namespaces[rng.IntN(len(namespaces))], Name: fmt.Sprintf("%s-%02d", pick("Alpha", "beta", "gamma"), i),
			Type: pick("model", "model", "dataset"), Visibility: pick("public", "public", "internal", "private"), DefaultBranch: pick("main", "main", "main", "dev"),
			HeadRef: "refs/heads/main", Tags: []string{}, Settings: map[string]any{}, Gating: map[string]any{}, SizeBytes: rng.Int64N(5) * 1000,
			Timestamps: db.Timestamps{CreatedAt: start.Add(time.Duration(rng.IntN(5)) * time.Hour)}}
		switch rng.IntN(5) {
		case 0:
			r.Settings["licence"] = pick("mit", "MIT", "apache-2.0")
		case 1:
			r.Settings["licence"] = 5
		}
		switch rng.IntN(6) {
		case 0:
			r.Settings["mirror"] = true
		case 1:
			r.Settings["mirror"] = "true"
		}
		if rng.IntN(3) > 0 {
			lm := start.Add(time.Duration(rng.IntN(5)) * time.Hour)
			r.LastModifiedAt = &lm
		}
		if rng.IntN(2) == 0 {
			r.Tags = []string{"featured"}
		}
		_, err := e.d.NewInsert().Model(&r).Exec(e.ctx)
		require.NoError(e.t, err)
		w.repos = append(w.repos, r)
		if r.Visibility == "private" && rng.IntN(2) == 0 {
			_, err := e.d.NewInsert().Model(&db.Collaborator{ID: db.NewID(), RepoID: r.ID, UserID: carol.UserID, Role: "read"}).Exec(e.ctx)
			require.NoError(e.t, err)
		}
		if rng.IntN(8) > 0 {
			sha := fmt.Sprintf("%040x", rng.Uint64())
			_, err := e.d.NewInsert().Model(&db.Ref{RepoID: r.ID, Name: model.BranchRef(r.DefaultBranch), Target: sha}).Exec(e.ctx)
			require.NoError(e.t, err)
			if rng.IntN(6) > 0 {
				w.index(r, sha, func(s *index.Summary) {
					s.Formats = some("safetensors", "gguf", "GGUF", "onnx")
					s.LibraryName = pick("", "transformers", "Transformers", "diffusers")
					s.PipelineTag = pick("", "text-generation", "fill-mask")
					s.Licence.ID = pick("", "mit", "apache-2.0", "MIT")
					for _, q := range some("Q4_K_M", "q4_k_m", "Q8_0", "") {
						s.Quantisations = append(s.Quantisations, index.Quantisation{Name: q})
					}
					if rng.IntN(4) > 0 {
						s.Parameters = &index.Parameters{Total: rng.Int64N(4) * 1e9}
					}
					s.ContextLength = rng.Int64N(3) * 4096
					if r.Type == "dataset" {
						s.Dataset = &index.DatasetSummary{Modalities: some("text", "Text", "image"), Languages: some("en", "fr", "EN"),
							Formats: some("parquet", "csv", "safetensors"), Tasks: some("text-classification", "fill-mask"),
							SizeCategory: pick("", "n<1K", "1K<n<10K", "N<1K"), Rows: rng.Int64N(3) * 5000}
					}
				})
			}
			if r.Type == "dataset" && rng.IntN(3) == 0 {
				rows := rng.Int64N(4) * 20000
				_, err := e.d.NewInsert().Model(&db.DatasetSplit{RepoID: r.ID, Commit: sha, Config: "default", Split: "train",
					NumRows: &rows, Features: db.JSON("{}"), Status: "ready"}).Exec(e.ctx)
				require.NoError(e.t, err)
			}
		}
		for _, l := range w.likers[:rng.IntN(len(w.likers)+1)] {
			_, err := e.d.NewInsert().Model(&db.Like{UserID: l.UserID, RepoID: r.ID}).Exec(e.ctx)
			require.NoError(e.t, err)
			w.likes[r.ID]++
		}
		for _, ago := range rng.Perm(45)[:rng.IntN(4)] {
			count := 1 + rng.Int64N(3)
			day := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -ago)
			_, err := e.d.NewInsert().Model(&db.DownloadCount{RepoID: r.ID, Day: day, Count: count}).Exec(e.ctx)
			require.NoError(e.t, err)
			if ago <= 30 {
				w.downloads[r.ID] += count
			}
		}
	}
}

// index stores index results for a Repo's Commit, as indexdb does.
func (w *factsWorld) index(r db.Repo, sha string, edit func(*index.Summary)) {
	s := &index.Summary{}
	edit(s)
	if s.Parameters != nil {
		w.params[r.ID] = s.Parameters.Total
	} else {
		delete(w.params, r.ID)
	}
	raw, err := json.Marshal(s)
	require.NoError(w.e.t, err)
	_, err = w.e.d.NewInsert().Model(&db.Metadata{RepoID: r.ID, Commit: sha, IndexerVersion: index.Version, Summary: raw, IndexedAt: start}).
		On("CONFLICT (repo_id, commit_sha, indexer_version) DO UPDATE").Set("summary = EXCLUDED.summary").Set("updated_at = EXCLUDED.updated_at").
		Exec(w.e.ctx)
	require.NoError(w.e.t, err)
}

// optionSets are the listings compared: every metadata filter, with values
// in other cases, every sort in both directions, and the plain filters
// combined with them.
func optionSets() []repo.ListOptions {
	yes, no := true, false
	var out []repo.ListOptions
	for _, s := range []string{"", "name", "created", "updated", "size", "parameters", "downloads", "likes"} {
		for _, desc := range []bool{false, true} {
			out = append(out, repo.ListOptions{IncludeFacets: true, Sort: s, Desc: desc})
		}
	}
	for _, o := range []repo.ListOptions{
		{Formats: []string{"gguf"}}, {Formats: []string{"SAFETENSORS", "onnx"}}, {Libraries: []string{"transformers"}},
		{Tasks: []string{"text-generation"}}, {Tasks: []string{"Fill-Mask"}}, {Quants: []string{"Q4_K_M"}}, {Quants: []string{""}},
		{Licences: []string{"MIT"}}, {Licences: []string{"apache-2.0", "mit"}}, {Modalities: []string{"TEXT"}},
		{Languages: []string{"en"}}, {SizeCategories: []string{"n<1k"}}, {SizeCategories: []string{""}},
		{ParamsMin: 1e9}, {ParamsMax: 2e9}, {ParamsMin: 1e9, ParamsMax: 2e9}, {ContextMin: 4096},
		{RowsMin: 10000}, {RowsMax: 20000}, {RowsMin: 1, RowsMax: 60000},
		{Type: model.RepoTypeDataset, Languages: []string{"EN", "fr"}}, {Namespaces: []string{"LAB"}, Formats: []string{"gguf"}},
		{Search: "ALPHA", ParamsMin: 1}, {Mirror: &yes, Libraries: []string{"Transformers"}}, {Mirror: &no, ContextMin: 1},
		{Tags: []string{"featured"}, Formats: []string{"safetensors"}}, {SizeMax: 2000, ParamsMin: 1},
		{Formats: []string{"gguf"}, Licences: []string{"mit"}, Sort: "likes", Desc: true},
	} {
		o.IncludeFacets = true
		out = append(out, o)
		o.Sort, o.Desc = "downloads", true
		out = append(out, o)
		o.Sort, o.Desc = "parameters", false
		out = append(out, o)
	}
	return out
}

// sortKey is what opts.Sort orders r by.
func (w *factsWorld) sortKey(opts repo.ListOptions, r *repo.Repo) string {
	n := func(v int64) string { return fmt.Sprintf("%020d", v) }
	switch opts.Sort {
	case "parameters":
		return n(w.params[r.UUID])
	case "downloads":
		return n(w.downloads[r.UUID])
	case "likes":
		return n(w.likes[r.UUID])
	case "size":
		return n(r.SizeBytes)
	case "created":
		return n(r.CreatedAt.UnixNano())
	case "updated":
		return n(r.LastModified.UnixNano())
	default:
		return strings.ToLower(r.ID.String())
	}
}

// inCreationOrder puts Repos with equal sort keys in creation order, which
// only changes the order of a listing that reads every candidate.
func (w *factsWorld) inCreationOrder(opts repo.ListOptions, repos []*repo.Repo) []string {
	out := slices.Clone(repos)
	slices.SortStableFunc(out, func(a, b *repo.Repo) int {
		c := cmp.Compare(w.sortKey(opts, a), w.sortKey(opts, b))
		if opts.Desc {
			c = -c
		}
		if c != 0 {
			return c
		}
		return cmp.Compare(a.UUID.String(), b.UUID.String())
	})
	return ids(&repo.ListResult{Repos: out})
}

// compare checks that both services give the same Repos in the same order
// and the same facet counts and that, a page at a time, the SQL listing
// gives the same pages and cursors as reading every candidate does. With
// few set, it checks two viewers and every third listing, which is enough
// after a change to one Repo.
func (w *factsWorld) compare(t *testing.T, few bool) {
	t.Helper()
	ctx := w.e.ctx
	for name, viewer := range w.viewers {
		if few && name != "alice" && name != "admin" {
			continue
		}
		for i, opts := range optionSets() {
			if few && i%3 != 0 {
				continue
			}
			opts.Viewer = viewer
			label := fmt.Sprintf("%s %+v", name, opts)
			want, err := w.mem.List(ctx, opts)
			require.NoError(t, err, label)
			got, err := w.sql.List(ctx, opts)
			require.NoError(t, err, label)
			wantIDs := w.inCreationOrder(opts, want.Repos)
			require.Equal(t, wantIDs, ids(got), label)
			require.Equal(t, want.Facets, got.Facets, label)
			gotJSON, err := json.Marshal(got.Facets)
			require.NoError(t, err)
			wantJSON, err := json.Marshal(want.Facets)
			require.NoError(t, err)
			require.JSONEq(t, string(wantJSON), string(gotJSON), label)

			// Reading every candidate pages the full listing, with the
			// same facets on every page.
			opts.Limit = 7
			paged := []string{}
			for {
				page, err := w.sql.List(ctx, opts)
				require.NoError(t, err, label)
				require.Equal(t, want.Facets, page.Facets, label)
				paged = append(paged, ids(page)...)
				next := ""
				if len(paged) < len(wantIDs) {
					next = fmt.Sprint(len(paged))
				}
				require.Equal(t, next, page.NextCursor, label)
				if page.NextCursor == "" {
					break
				}
				opts.Cursor = page.NextCursor
			}
			require.Equal(t, wantIDs, paged, label)
		}
	}
}

// Listing from repo_facts in SQL gives what reading every candidate's index
// results and community counts gives: the same Repos, order, cursors and
// facet counts, for every filter, sort and kind of viewer.
func TestListFactsMatchReadingEveryRepo(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		w := newFactsWorld(t, d)
		w.seed(70)
		// Every Repo is new, so a refresh builds every row. Reading index
		// results stores the dataset sizes they lacked, which marks those
		// datasets out of date once more; then there is nothing left to do.
		n, err := w.sql.RefreshListFacts(w.e.ctx)
		require.NoError(t, err)
		require.Equal(t, 70, n)
		n, err = w.sql.RefreshListFacts(w.e.ctx)
		require.NoError(t, err)
		require.Less(t, n, 70)
		n, err = w.sql.RefreshListFacts(w.e.ctx)
		require.NoError(t, err)
		require.Zero(t, n)
		w.compare(t, false)
	})
}

// repo_facts rows are rebuilt when what they were built from changes,
// whichever way it changes, as the triggers of migration 0007 record it:
// the default branch head, the licence setting, the index results (a
// re-index or a new indexer version), a dataset's Split sizes and the
// default branch. A deleted Repo leaves listings and facet counts.
func TestListFactsFollowChanges(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		w := newFactsWorld(t, d)
		w.seed(30)
		w.compare(t, true)
		e := w.e
		withHead := func(match func(db.Repo) bool) db.Repo {
			t.Helper()
			for _, r := range w.repos {
				var ref db.Ref
				if match(r) && e.d.NewSelect().Model(&ref).Where("repo_id = ?", r.ID).Limit(1).Scan(e.ctx) == nil {
					return r
				}
			}
			t.Fatal("no Repo matches")
			return db.Repo{}
		}

		// A new head with new index results.
		r := withHead(func(r db.Repo) bool { return r.Type == "model" })
		sha := strings.Repeat("a", 40)
		w.index(r, sha, func(s *index.Summary) {
			s.Formats, s.LibraryName, s.Parameters = []string{"gguf"}, "llama.cpp", &index.Parameters{Total: 9e9}
		})
		_, err := e.d.NewUpdate().Model((*db.Ref)(nil)).Set("target = ?", sha).Where("repo_id = ?", r.ID).Exec(e.ctx)
		require.NoError(t, err)
		w.compare(t, true)
		res, err := w.sql.List(e.ctx, repo.ListOptions{Viewer: w.viewers["admin"], Libraries: []string{"LLAMA.CPP"}})
		require.NoError(t, err)
		require.Equal(t, []string{r.ID.String()}, uuids(res))

		// A re-index of the same head.
		w.index(r, sha, func(s *index.Summary) { s.Formats, s.LibraryName = []string{"onnx"}, "optimum" })
		w.compare(t, true)

		// A newer indexer version, as after an upgrade.
		raw, err := json.Marshal(&index.Summary{Formats: []string{"mlx"}, LibraryName: "mlx"})
		require.NoError(t, err)
		_, err = e.d.NewInsert().Model(&db.Metadata{RepoID: r.ID, Commit: sha, IndexerVersion: index.Version + 1, Summary: raw, IndexedAt: start}).Exec(e.ctx)
		require.NoError(t, err)
		w.compare(t, true)
		res, err = w.sql.List(e.ctx, repo.ListOptions{Viewer: w.viewers["admin"], Formats: []string{"MLX"}})
		require.NoError(t, err)
		require.Equal(t, []string{r.ID.String()}, uuids(res))

		// The licence setting.
		_, err = e.d.NewUpdate().Model((*db.Repo)(nil)).Set("settings = ?", `{"licence":"gpl-3.0"}`).Where("id = ?", r.ID).Exec(e.ctx)
		require.NoError(t, err)
		w.compare(t, true)

		// The default branch, to one with no ref.
		_, err = e.d.NewUpdate().Model((*db.Repo)(nil)).Set("default_branch = ?", "nowhere").Where("id = ?", r.ID).Exec(e.ctx)
		require.NoError(t, err)
		delete(w.params, r.ID)
		w.compare(t, true)

		// A dataset's Split sizes, which index results take from the export.
		ds := withHead(func(r db.Repo) bool { return r.Type == "dataset" })
		var ref db.Ref
		require.NoError(t, e.d.NewSelect().Model(&ref).Where("repo_id = ?", ds.ID).Limit(1).Scan(e.ctx))
		w.index(ds, ref.Target, func(s *index.Summary) { s.Dataset = &index.DatasetSummary{Modalities: []string{"audio"}} })
		w.compare(t, true)
		rows := int64(777_777)
		_, err = e.d.NewInsert().Model(&db.DatasetSplit{RepoID: ds.ID, Commit: ref.Target, Config: "zz", Split: "validation",
			NumRows: &rows, Features: db.JSON("{}"), Status: "ready"}).Exec(e.ctx)
		require.NoError(t, err)
		_, err = e.d.NewUpdate().Model((*db.DatasetSplit)(nil)).Set("status = 'ready'").Where("repo_id = ?", ds.ID).Exec(e.ctx)
		require.NoError(t, err)
		w.compare(t, true)
		res, err = w.sql.List(e.ctx, repo.ListOptions{Viewer: w.viewers["admin"], RowsMin: 777_777})
		require.NoError(t, err)
		require.Equal(t, []string{ds.ID.String()}, uuids(res))

		// A deleted Repo leaves the listing and its facet counts.
		_, err = e.d.NewUpdate().Model((*db.Repo)(nil)).Set("deleted_at = ?", start).Where("id = ?", ds.ID).Exec(e.ctx)
		require.NoError(t, err)
		w.compare(t, true)
	})
}

func uuids(l *repo.ListResult) []string {
	out := []string{}
	for _, r := range l.Repos {
		out = append(out, r.UUID.String())
	}
	return out
}

// A private Repo never counts towards the facets of a viewer who cannot
// read it, even when it is the only Repo with a value.
func TestListFactsHidePrivateRepos(t *testing.T) {
	t.Parallel()
	dbtest.ForEachDriver(t, func(t *testing.T, d *db.DB) {
		w := newFactsWorld(t, d)
		e := w.e
		alice, bob := e.addUser("alice"), e.addUser("bob")
		for _, v := range []struct{ name, visibility, library string }{{"open", "public", "transformers"}, {"secret", "private", "secret-lib"}} {
			r := db.Repo{ID: db.NewID(), NamespaceID: alice.NamespaceID, Name: v.name, Type: "model", Visibility: v.visibility,
				DefaultBranch: "main", HeadRef: "refs/heads/main", Tags: []string{}, Settings: map[string]any{}, Gating: map[string]any{}}
			_, err := e.d.NewInsert().Model(&r).Exec(e.ctx)
			require.NoError(t, err)
			sha := strings.Repeat("b", 40)
			_, err = e.d.NewInsert().Model(&db.Ref{RepoID: r.ID, Name: "refs/heads/main", Target: sha}).Exec(e.ctx)
			require.NoError(t, err)
			w.index(r, sha, func(s *index.Summary) { s.LibraryName = v.library })
		}
		for _, p := range []model.Principal{alice, bob, {}} {
			res, err := w.sql.List(e.ctx, repo.ListOptions{Viewer: p, IncludeFacets: true})
			require.NoError(t, err)
			_, leaked := res.Facets[repo.FacetLibrary]["secret-lib"]
			require.Equal(t, p.UserID == alice.UserID, leaked, "%s", p.Username)
			res, err = w.sql.List(e.ctx, repo.ListOptions{Viewer: p, IncludeFacets: true, Libraries: []string{"secret-lib"}})
			require.NoError(t, err)
			require.Equal(t, p.UserID == alice.UserID, len(res.Repos) == 1, "%s", p.Username)
			if p.UserID != alice.UserID {
				require.Empty(t, res.Facets, "%s", p.Username)
			}
		}
	})
}
