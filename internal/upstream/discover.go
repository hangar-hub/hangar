package upstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/index"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/upstream/deps"
)

// ArsenalState says how much of an upstream Repo the Arsenal holds
// (BROWSE-2).
type ArsenalState string

// Arsenal states.
const (
	ArsenalNone    ArsenalState = "none"
	ArsenalPartial ArsenalState = "partial"
	ArsenalFull    ArsenalState = "full"
)

// searchCache keeps upstream search pages for a few minutes, keyed by
// Upstream, query and a hash of the token used, so private results never
// reach another user.
type searchCache struct {
	ttl time.Duration
	max int

	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	page    *SearchPage
	expires time.Time
}

func newSearchCache(ttl time.Duration, maxEntries int) *searchCache {
	return &searchCache{ttl: ttl, max: maxEntries, entries: map[string]cacheEntry{}}
}

func cacheKey(up *Upstream, cred Credentials, q SearchQuery) string {
	b, _ := json.Marshal(q)
	tok := sha256.Sum256([]byte(cred.Token))
	return up.ID.String() + "|" + hex.EncodeToString(tok[:]) + "|" + string(b)
}

func (c *searchCache) get(key string, now time.Time) (*SearchPage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || now.After(e.expires) {
		delete(c.entries, key)
		return nil, false
	}
	return e.page, true
}

func (c *searchCache) put(key string, page *SearchPage, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.max {
		for k, e := range c.entries {
			if now.After(e.expires) {
				delete(c.entries, k)
			}
		}
		for k := range c.entries {
			if len(c.entries) < c.max {
				break
			}
			delete(c.entries, k)
		}
	}
	c.entries[key] = cacheEntry{page: page, expires: now.Add(c.ttl)}
}

// DiscoverItem is one search result with its state in the Arsenal.
type DiscoverItem struct {
	RepoSummary
	Arsenal ArsenalState
	// LocalSHA is set when the Arsenal holds a different Revision
	// ("Update available").
	LocalSHA model.CommitSHA
}

// DiscoverPage is a page of Discover results.
type DiscoverPage struct {
	Upstream   Upstream
	Items      []DiscoverItem
	NextCursor string
}

// Discover searches an Upstream and marks each result with its state in
// the Arsenal as p sees it (BROWSE-2, DATA-7).
func (s *Service) Discover(ctx context.Context, p model.Principal, upstreamName string, q SearchQuery) (*DiscoverPage, error) {
	up, err := s.opts.Registry.Enabled(ctx, upstreamName)
	if err != nil {
		return nil, err
	}
	cred, err := s.credentials(ctx, up, p.UserID)
	if err != nil {
		return nil, err
	}
	key := cacheKey(up, cred, q)
	page, ok := s.cache.get(key, s.opts.Now())
	if !ok {
		if page, err = s.opts.Client.Search(ctx, *up, cred, q); err != nil {
			return nil, err
		}
		s.cache.put(key, page, s.opts.Now())
	}
	out := &DiscoverPage{Upstream: *up, NextCursor: page.NextCursor}
	for _, r := range page.Repos {
		state, local, err := s.localState(ctx, p, up.LocalID(r.ID), r.SHA)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, DiscoverItem{RepoSummary: r, Arsenal: state, LocalSHA: local})
	}
	return out, nil
}

// localState reports how much of a Repo the Arsenal holds, as p sees it:
// none when it is missing or p cannot read it, full when every LFS object
// at the default branch head is present, partial otherwise. localSHA is
// the head when it differs from upstreamSHA.
func (s *Service) localState(ctx context.Context, p model.Principal, local model.RepoID, upstreamSHA model.CommitSHA) (ArsenalState, model.CommitSHA, error) {
	r, err := s.opts.Repos.Get(ctx, local)
	if errors.Is(err, repo.ErrNotFound) || errors.Is(err, repo.ErrInvalid) {
		return ArsenalNone, "", nil
	}
	if err != nil {
		return "", "", err
	}
	// Callers who cannot read the Repo must not learn it exists.
	if !s.readable(ctx, p, r) {
		return ArsenalNone, "", nil
	}
	head, err := s.opts.Repos.Resolve(ctx, r.ID, "")
	if errors.Is(err, repo.ErrRevisionNotFound) {
		return ArsenalNone, "", nil
	}
	if err != nil {
		return "", "", err
	}
	entries, err := s.opts.Repos.Tree(ctx, r.ID, head, "", repo.TreeOpts{Recursive: true})
	if err != nil {
		return "", "", err
	}
	state := ArsenalFull
	for _, e := range entries {
		if e.LFS != nil && !e.Present {
			state = ArsenalPartial
			break
		}
	}
	var localSHA model.CommitSHA
	if !upstreamSHA.IsZero() && head != upstreamSHA {
		localSHA = head
	}
	return state, localSHA, nil
}

// DiscoverFile is one upstream file and whether the Arsenal holds it.
type DiscoverFile struct {
	Path    string
	Size    int64
	LFS     bool
	Present bool
}

// QuantState is a GGUF quant group and whether the Arsenal holds all of it.
type QuantState struct {
	index.QuantGroup
	Present bool
}

// DependencyState is a Dependency and its target's state in the Arsenal.
type DependencyState struct {
	deps.Dependency
	Arsenal ArsenalState
}

// DatasetSplit is a Split from the Upstream's Dataset Viewer.
type DatasetSplit struct {
	Split    string
	NumRows  *int64
	NumBytes *int64
}

// DatasetConfig is a Config from the Upstream's Dataset Viewer.
type DatasetConfig struct {
	Config string
	Splits []DatasetSplit
}

// DiscoverDetail is an upstream Repo's detail panel (06 "Discover").
type DiscoverDetail struct {
	Upstream     Upstream
	Info         *RepoInfo
	Card         string
	Files        []DiscoverFile
	Quants       []QuantState
	Configs      []DatasetConfig
	Preview      json.RawMessage
	Dependencies []DependencyState
	TotalSize    int64
	Arsenal      ArsenalState
	LocalSHA     model.CommitSHA
}

// maxCardBytes bounds the Model card shown in Discover.
const maxCardBytes = 1 << 20

// DiscoverDetail returns the files, quants, Configs and Splits, the
// Dependencies Hangar would pull, the total size and the Arsenal state of
// an upstream Repo at a Revision.
func (s *Service) DiscoverDetail(ctx context.Context, p model.Principal, upstreamName string, id model.RepoID, revision string) (*DiscoverDetail, error) {
	up, err := s.opts.Registry.Enabled(ctx, upstreamName)
	if err != nil {
		return nil, err
	}
	cred, err := s.credentials(ctx, up, p.UserID)
	if err != nil {
		return nil, err
	}
	info, err := s.opts.Client.RepoInfo(ctx, *up, cred, id, revision)
	if err != nil {
		return nil, err
	}
	local := up.LocalID(id)
	out := &DiscoverDetail{Upstream: *up, Info: info}
	if out.Arsenal, out.LocalSHA, err = s.localState(ctx, p, local, info.SHA); err != nil {
		return nil, err
	}
	present := map[string]bool{}
	if out.Arsenal != ArsenalNone {
		var oids []string
		for _, sib := range info.Siblings {
			if sib.LFS != nil {
				oids = append(oids, sib.LFS.OID)
			}
		}
		if refs, err := s.opts.Repos.LFSRefs(ctx, local, oids); err == nil {
			for oid, ref := range refs {
				present[oid] = ref.Present
			}
		}
	}
	sizes := map[string]int64{}
	var files []string
	for _, sib := range info.Siblings {
		f := DiscoverFile{Path: sib.Path, Size: sib.Size, LFS: sib.LFS != nil, Present: out.Arsenal != ArsenalNone}
		if sib.LFS != nil {
			f.Present = present[sib.LFS.OID]
		}
		out.Files = append(out.Files, f)
		out.TotalSize += sib.Size
		sizes[sib.Path] = sib.Size
		files = append(files, sib.Path)
	}
	filePresent := map[string]bool{}
	for _, f := range out.Files {
		filePresent[f.Path] = f.Present
	}
	for _, g := range index.GroupQuants(sizes) {
		qs := QuantState{QuantGroup: g, Present: true}
		for _, f := range append(append([]string{}, g.Files...), g.Projectors...) {
			if !filePresent[f] {
				qs.Present = false
			}
		}
		out.Quants = append(out.Quants, qs)
	}
	// The card, the files Dependencies come from and a dataset's Viewer
	// answers are separate Upstream requests, each a round trip that can
	// take seconds over a home link, so they run at once (bellatrix
	// finding 10: a lookup took 40 seconds).
	var wg sync.WaitGroup
	wg.Go(func() {
		if body, err := s.readUpstreamFile(ctx, up, cred, id, info.SHA, "README.md", sizes, maxCardBytes); err == nil {
			out.Card = string(body)
		}
	})
	if id.Type == model.RepoTypeDataset {
		wg.Go(func() { out.Configs, out.Preview = s.upstreamDatasetInfo(ctx, up, cred, id) })
	}
	ds, err := deps.Parse(ctx, id, files, func(ctx context.Context, path string) ([]byte, error) {
		return s.readUpstreamFile(ctx, up, cred, id, info.SHA, path, sizes, deps.MaxFileSize)
	})
	wg.Wait()
	if err != nil {
		return nil, err
	}
	for _, d := range ds {
		st, _, err := s.localState(ctx, p, up.LocalID(d.Target), "")
		if err != nil {
			return nil, err
		}
		out.Dependencies = append(out.Dependencies, DependencyState{Dependency: d, Arsenal: st})
	}
	return out, nil
}

// readUpstreamFile downloads a small file from the Upstream.
func (s *Service) readUpstreamFile(ctx context.Context, up *Upstream, cred Credentials, id model.RepoID, commit model.CommitSHA, path string, sizes map[string]int64, limit int64) ([]byte, error) {
	size, ok := sizes[path]
	if !ok || size > limit {
		return nil, fs.ErrNotExist
	}
	rc, _, err := s.opts.Client.Download(ctx, *up, cred, id, commit, path, nil)
	if errors.Is(err, ErrNotFound) {
		return nil, fs.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(io.LimitReader(rc, limit))
}

// upstreamDatasetInfo reads Configs, Splits and sizes from the Upstream's
// Dataset Viewer, and the first rows of the first Split (DATA-7). The
// Viewer is optional: errors leave the fields empty.
func (s *Service) upstreamDatasetInfo(ctx context.Context, up *Upstream, cred Credentials, id model.RepoID) ([]DatasetConfig, json.RawMessage) {
	q := url.Values{"dataset": {id.String()}}
	raw, err := s.opts.Client.Viewer(ctx, *up, cred, "/splits", q)
	if err != nil {
		s.log.DebugContext(ctx, "upstream dataset viewer /splits", "error", err.Error())
		return nil, nil
	}
	var splits struct {
		Splits []struct {
			Config string `json:"config"`
			Split  string `json:"split"`
		} `json:"splits"`
	}
	if json.Unmarshal(raw, &splits) != nil || len(splits.Splits) == 0 {
		return nil, nil
	}
	type sizeRow struct {
		Config  string `json:"config"`
		Split   string `json:"split"`
		NumRows int64  `json:"num_rows"`
		Bytes   int64  `json:"num_bytes_parquet_files"`
	}
	sizes := map[string]sizeRow{}
	if raw, err := s.opts.Client.Viewer(ctx, *up, cred, "/size", q); err == nil {
		var sz struct {
			Size struct {
				Splits []sizeRow `json:"splits"`
			} `json:"size"`
		}
		if json.Unmarshal(raw, &sz) == nil {
			for _, r := range sz.Size.Splits {
				sizes[r.Config+"/"+r.Split] = r
			}
		}
	}
	var configs []DatasetConfig
	idx := map[string]int{}
	for _, sp := range splits.Splits {
		i, ok := idx[sp.Config]
		if !ok {
			i = len(configs)
			idx[sp.Config] = i
			configs = append(configs, DatasetConfig{Config: sp.Config})
		}
		ds := DatasetSplit{Split: sp.Split}
		if r, ok := sizes[sp.Config+"/"+sp.Split]; ok {
			rows, b := r.NumRows, r.Bytes
			ds.NumRows, ds.NumBytes = &rows, &b
		}
		configs[i].Splits = append(configs[i].Splits, ds)
	}
	first := splits.Splits[0]
	preview, err := s.opts.Client.Viewer(ctx, *up, cred, "/first-rows", url.Values{"dataset": {id.String()}, "config": {first.Config}, "split": {first.Split}})
	if err != nil {
		preview = nil
	}
	return configs, preview
}

// Graph is a Repo's Dependencies both ways.
type Graph struct {
	Needs    []DependencyState
	NeededBy []NeededBy
}

// NeededBy is a Repo that needs the one asked about.
type NeededBy struct {
	Repo model.RepoID
	deps.Dependency
	Arsenal ArsenalState
}

// Graph returns what r needs at its default branch head and which Repos
// (that p can read) need r at their own heads.
func (s *Service) Graph(ctx context.Context, p model.Principal, r *repo.Repo) (*Graph, error) {
	g := &Graph{}
	head, err := s.opts.Repos.Resolve(ctx, r.ID, "")
	if err == nil {
		needs, err := s.deps.Needs(ctx, r.UUID, head)
		if err != nil {
			return nil, err
		}
		if len(needs) == 0 {
			if needs, err = s.resolveAndStore(ctx, r, head); err != nil {
				return nil, err
			}
		}
		up := s.mirrorUpstream(ctx, r)
		for _, d := range needs {
			target := d.Target
			if up != nil {
				target = up.LocalID(target)
			}
			st, _, err := s.localState(ctx, p, target, "")
			if err != nil {
				return nil, err
			}
			g.Needs = append(g.Needs, DependencyState{Dependency: d, Arsenal: st})
		}
	} else if !errors.Is(err, repo.ErrRevisionNotFound) {
		return nil, err
	}
	if g.NeededBy, err = s.NeededBy(ctx, p, r); err != nil {
		return nil, err
	}
	return g, nil
}

// NeededBy returns the Repos that p can read whose default branch heads
// need r, by r's own ID or, for a Mirror, by its ID on the Upstream.
func (s *Service) NeededBy(ctx context.Context, p model.Principal, r *repo.Repo) ([]NeededBy, error) {
	var out []NeededBy
	targets := []model.RepoID{r.ID}
	if up := s.mirrorUpstream(ctx, r); up != nil {
		if uid, ok := up.UpstreamID(r.ID); ok && !uid.Equal(r.ID) {
			targets = append(targets, uid)
		}
	}
	heads := map[uuid.UUID]model.CommitSHA{}
	seen := map[string]bool{}
	for _, t := range targets {
		rows, err := s.deps.NeededBy(ctx, t)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			src, err := s.opts.Repos.GetByUUID(ctx, row.RepoUUID)
			if err != nil || src.DeletedAt != nil {
				continue
			}
			h, ok := heads[src.UUID]
			if !ok {
				h, _ = s.opts.Repos.Resolve(ctx, src.ID, "")
				heads[src.UUID] = h
			}
			if h != row.Commit {
				continue
			}
			if !s.readable(ctx, p, src) {
				continue
			}
			key := src.UUID.String() + row.Key()
			if seen[key] {
				continue
			}
			seen[key] = true
			st, _, err := s.localState(ctx, p, src.ID, "")
			if err != nil {
				return nil, err
			}
			out = append(out, NeededBy{Repo: src.ID, Dependency: row.Dependency, Arsenal: st})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Repo.String() < out[j].Repo.String() })
	return out, nil
}

// mirrorUpstream returns the Upstream of a Mirror, or nil.
func (s *Service) mirrorUpstream(ctx context.Context, r *repo.Repo) *Upstream {
	m, err := s.mirrors.active(ctx, r.UUID, r.ID.Type)
	if err != nil {
		return nil
	}
	up, err := s.opts.Registry.Get(ctx, m.UpstreamID)
	if err != nil {
		return nil
	}
	return up
}

// ResolveDependencies resolves and stores the Dependencies of a Commit and
// is called after every index (06 "Dependencies"). For Repos that are not
// Mirrors it Pulls missing code Dependencies from the default Upstream
// when pull.dependencies.auto_pull_code is on, and returns a warning for
// each one it cannot pull.
func (s *Service) ResolveDependencies(ctx context.Context, repoUUID uuid.UUID, commit model.CommitSHA) ([]string, error) {
	r, err := s.opts.Repos.GetByUUID(ctx, repoUUID)
	if err != nil {
		return nil, err
	}
	ds, err := s.resolveAndStore(ctx, r, commit)
	if err != nil {
		return nil, err
	}
	if s.mirrorUpstream(ctx, r) != nil {
		return nil, nil
	}
	var warnings []string
	for _, d := range ds {
		if d.Kind != deps.KindCode || !d.Required {
			continue
		}
		st, _, err := s.localState(ctx, model.System(), d.Target, "")
		if err != nil {
			return nil, err
		}
		if st == ArsenalFull {
			continue
		}
		if !s.opts.Dependencies.AutoPullCode {
			warnings = append(warnings, fmt.Sprintf("%s needs %s, which is not in the arsenal", r.ID, d.Target))
			continue
		}
		up, err := s.opts.Registry.Enabled(ctx, "")
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s needs %s, and no Upstream is enabled to pull it from", r.ID, d.Target))
			continue
		}
		p := pullPayload{UpstreamID: up.ID, Upstream: up.Name, UpstreamRepo: d.Target.String(), Type: d.Target.Type,
			Repo: up.LocalID(d.Target).String(), Revision: d.Revision, Depth: 1, Chain: []string{string(r.ID.Type) + ":" + r.ID.String()}}
		if _, err := s.enqueuePull(ctx, p, uuid.Nil, 0); err != nil {
			return nil, err
		}
	}
	return warnings, nil
}

// readable reports whether p may read r.
func (s *Service) readable(ctx context.Context, p model.Principal, r *repo.Repo) bool {
	return p.Kind == model.PrincipalSystem || s.opts.Authz.Can(ctx, p, auth.ActionRead, repoTarget(r)) == nil
}
