// Package viewer is the Dataset Viewer API (DATA-4), compatible with the
// Hub's datasets-server: /is-valid, /splits, /first-rows, /rows, /search,
// /filter, /parquet, /info, /size, /statistics and the rest. It reads the
// Parquet export the pipeline writes, applies the Repo's read
// permissions, and serves media cells through short-lived signed URLs.
package viewer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/datasets/layout"
	"github.com/jamesagarside/hangar/internal/datasets/pipeline"
	"github.com/jamesagarside/hangar/internal/datasets/pq"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/jobs"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// Errors beyond those in package datasets.
var (
	// ErrFailed means the dataset, Config or Split could not be converted.
	ErrFailed = errors.New("the dataset could not be converted")
	// ErrInvalidParameter means a request parameter is out of range.
	ErrInvalidParameter = errors.New("invalid parameter")
)

// hubError is an error the Viewer answers with the Hub's HTTP status,
// error code and message, and nothing else: no internal error text.
type hubError struct {
	status  int
	code    string
	message string
	// failed is set for a conversion or statistics failure (ErrFailed).
	failed bool
}

func (e *hubError) Error() string { return e.code + ": " + e.message }

// Is makes errors.Is(err, ErrFailed) true for failures.
func (e *hubError) Is(target error) bool { return e.failed && target == ErrFailed }

// failure is the error for a stored failure code.
func failure(stored string) error {
	f := pipeline.FailureOf(stored)
	return &hubError{status: http.StatusInternalServerError, code: f.Code, message: f.Message, failed: true}
}

// tooBigAdvice ends the Hub's TooBigRows message (libcommon parquet_utils).
const tooBigAdvice = "\n\nMake sure that\n\n" +
	"1. the Parquet files contain a page index to enable random access without loading entire row groups" +
	"2. otherwise use smaller row-group sizes when serializing the Parquet files\n" //nolint:misspell // the Hub's message, word for word

// tooBig is the Hub's TooBigContentError for rows that decode to more
// than a request's budget. /first-rows answers it with 501, as the Hub's
// worker caches it; the other endpoints with 500.
func tooBig(err error, status int) error {
	var tb *pq.TooBigError
	if !errors.As(err, &tb) {
		return err
	}
	return tooBigError(tb, status)
}

func tooBigError(tb *pq.TooBigError, status int) *hubError {
	return &hubError{status: status, code: "TooBigContentError", message: tb.Error() + tooBigAdvice}
}

// Options configure the Viewer; they mirror the viewer.* configuration.
type Options struct {
	// PublicURL is server.public_url.
	PublicURL string
	// MaxScanRows and MaxScanTime bound /search and /filter scans.
	MaxScanRows int64
	MaxScanTime time.Duration
	// MaxCellBytes is the largest cell /rows returns untruncated.
	MaxCellBytes int
	// AssetTTL is the lifetime of signed media URLs. Zero means one hour.
	AssetTTL time.Duration
	// MaxRequestBytes is the decoded Parquet data one request may hold.
	// Zero means pq.DefaultBudgetBytes, the Hub's 300 MB.
	MaxRequestBytes int64
}

// Deps are the services the Viewer uses.
type Deps struct {
	Repos repo.Service
	Authz auth.Authz
	Store *pipeline.Store
	Jobs  jobs.Queue
	// Blobs caches /search and /filter results.
	Blobs  blob.Store
	Signer *httpx.Signer
	Log    *slog.Logger
	Now    func() time.Time
}

// Service implements datasets.Viewer.
type Service struct {
	d      Deps
	o      Options
	layout *layout.Service

	mu      sync.Mutex
	layouts map[string]*datasets.Resolved
	files   *fileCache
}

var _ datasets.Viewer = (*Service)(nil)

// New returns a Viewer.
func New(d Deps, o Options) (*Service, error) {
	if d.Repos == nil || d.Authz == nil || d.Store == nil || d.Jobs == nil || d.Blobs == nil || d.Signer == nil {
		return nil, errors.New("viewer: Repos, Authz, Store, Jobs, Blobs and Signer are required")
	}
	if o.PublicURL == "" {
		return nil, errors.New("viewer: PublicURL is required")
	}
	if o.MaxScanRows <= 0 {
		o.MaxScanRows = 5_000_000
	}
	if o.MaxScanTime <= 0 {
		o.MaxScanTime = 10 * time.Second
	}
	if o.MaxCellBytes <= 0 {
		o.MaxCellBytes = 100 << 10
	}
	if o.AssetTTL <= 0 {
		o.AssetTTL = time.Hour
	}
	if o.MaxRequestBytes <= 0 {
		o.MaxRequestBytes = pq.DefaultBudgetBytes
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Service{d: d, o: o, layout: &layout.Service{Repos: d.Repos}, layouts: map[string]*datasets.Resolved{}, files: newFileCache(64)}, nil
}

func target(r *repo.Repo) auth.Target {
	return auth.Target{Repo: &auth.RepoTarget{ID: r.ID, UUID: r.UUID, NamespaceID: r.NamespaceID, Visibility: r.Visibility,
		Gated: r.Gating.Enabled(), Mirror: r.Mirror}}
}

// load finds a dataset and checks the caller may do action on it. Callers
// who may not get datasets.ErrNotFound, so private names do not leak.
func (s *Service) load(ctx context.Context, p model.Principal, id model.RepoID, action auth.Action) (*repo.Repo, error) {
	if id.Type != model.RepoTypeDataset {
		return nil, fmt.Errorf("%w: %s is not a dataset", datasets.ErrNotFound, id.Path())
	}
	r, err := s.d.Repos.Get(ctx, id)
	if errors.Is(err, repo.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", datasets.ErrNotFound, id.String())
	}
	if err != nil {
		return nil, err
	}
	for _, a := range []auth.Action{auth.ActionRead, action} {
		if err := s.d.Authz.Can(ctx, p, a, target(r)); err != nil {
			if errors.Is(err, auth.ErrNotFound) || errors.Is(err, auth.ErrForbidden) || errors.Is(err, auth.ErrGated) {
				return nil, fmt.Errorf("%w: %s", datasets.ErrNotFound, id.String())
			}
			return nil, err
		}
	}
	return r, nil
}

// state is what the Viewer knows about a dataset's default-branch head.
type state struct {
	repo   *repo.Repo
	commit model.CommitSHA
	rows   []pipeline.SplitRow
	// failed holds the failure code when the dataset has no Configs.
	failed string
	// pending is true while the conversion has not stored its results.
	pending bool
	layout  *datasets.Resolved
}

func (st *state) dataset() string { return st.repo.ID.String() }

// configs lists Config names in order.
func (st *state) configs() []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range st.rows {
		if r.Config != pipeline.DatasetLevel && !seen[r.Config] {
			seen[r.Config] = true
			out = append(out, r.Config)
		}
	}
	return out
}

func (st *state) split(config, split string) (pipeline.SplitRow, bool) {
	for _, r := range st.rows {
		if r.Config == config && r.Split == split {
			return r, true
		}
	}
	return pipeline.SplitRow{}, false
}

func (st *state) hasConfig(config string) bool {
	for _, r := range st.rows {
		if r.Config == config {
			return true
		}
	}
	return false
}

// head loads the state of a dataset's default-branch head, starting its
// conversion when nothing is known about it yet.
func (s *Service) head(ctx context.Context, p model.Principal, id model.RepoID, action auth.Action) (*state, error) {
	r, err := s.load(ctx, p, id, action)
	if err != nil {
		return nil, err
	}
	commit, err := s.d.Repos.Resolve(ctx, r.ID, r.DefaultBranch)
	if errors.Is(err, repo.ErrRevisionNotFound) {
		return nil, fmt.Errorf("%w: the dataset has no Commits", datasets.ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	st := &state{repo: r, commit: commit}
	rows, err := s.d.Store.Splits(ctx, r.UUID, commit)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		if _, err := pipeline.EnqueueConvert(ctx, s.d.Jobs, r.UUID, commit); err != nil {
			s.d.Log.WarnContext(ctx, "could not queue the dataset conversion", "repo", r.ID.Path(), "error", err.Error())
		}
		lay, code, err := s.layoutOrFailure(ctx, r, commit)
		if err != nil {
			return nil, err
		}
		if code != "" {
			// The dataset has no Configs; the state reports why.
			st.failed = code
			return st, nil
		}
		st.layout = lay
		for _, c := range lay.Configs {
			for _, sp := range c.Splits() {
				rows = append(rows, pipeline.SplitRow{Config: c.Name, Split: sp, Status: pipeline.StatusPending})
			}
		}
	}
	for _, row := range rows {
		if row.Config == pipeline.DatasetLevel {
			st.failed = row.Error
			continue
		}
		if row.Status == pipeline.StatusPending {
			st.pending = true
		}
		st.rows = append(st.rows, row)
	}
	return st, nil
}

// resolveLayout returns a Commit's layout, cached in memory.
func (s *Service) resolveLayout(ctx context.Context, r *repo.Repo, commit model.CommitSHA) (*datasets.Resolved, error) {
	key := r.UUID.String() + "@" + string(commit)
	s.mu.Lock()
	if l, ok := s.layouts[key]; ok {
		s.mu.Unlock()
		return l, nil
	}
	s.mu.Unlock()
	l, err := s.layout.Resolve(ctx, r.ID, commit)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if len(s.layouts) > 256 {
		s.layouts = map[string]*datasets.Resolved{}
	}
	s.layouts[key] = l
	s.mu.Unlock()
	return l, nil
}

// info loads a Config's info document.
func (s *Service) info(ctx context.Context, st *state, config string) (*pipeline.ConfigInfo, error) {
	return s.d.Store.Info(ctx, st.repo.UUID, st.commit, config)
}

// readySplit returns a converted Split and its Config's info, or the
// error the Hub gives for a Split that is missing, pending or failed.
func (s *Service) readySplit(ctx context.Context, st *state, config, split string) (pipeline.SplitRow, *pipeline.ConfigInfo, error) {
	if st.failed != "" {
		return pipeline.SplitRow{}, nil, failure(st.failed)
	}
	row, ok := st.split(config, split)
	if !ok {
		return row, nil, fmt.Errorf("%w: %s/%s", datasets.ErrNotFound, config, split)
	}
	switch row.Status {
	case pipeline.StatusPending:
		return row, nil, datasets.ErrNotReady
	case pipeline.StatusFailed:
		return row, nil, failure(row.Error)
	}
	info, err := s.info(ctx, st, config)
	if errors.Is(err, pipeline.ErrNoInfo) {
		return row, nil, datasets.ErrNotReady
	}
	return row, info, err
}

// layoutOrFailure resolves a Commit's Configs, or gives the failure code
// that says why it has none. An error reading the files is returned as it
// is, because the dataset has not failed.
func (s *Service) layoutOrFailure(ctx context.Context, r *repo.Repo, commit model.CommitSHA) (*datasets.Resolved, string, error) {
	lay, err := s.resolveLayout(ctx, r, commit)
	if err != nil {
		code, ok := pipeline.LayoutFailure(err)
		if !ok {
			return nil, "", err
		}
		s.d.Log.DebugContext(ctx, "the dataset has no Configs", "repo", r.ID.Path(), "error", err.Error())
		return nil, code, nil
	}
	return lay, "", nil
}

// splitInfo returns a Split's entry in a Config's info document.
func splitInfo(info *pipeline.ConfigInfo, split string) (*pipeline.SplitInfo, bool) {
	for i := range info.Splits {
		if info.Splits[i].Name == split {
			return &info.Splits[i], true
		}
	}
	return nil, false
}

// Revision returns the Commit the Viewer answers for, for X-Revision.
func (st *state) Revision() string { return string(st.commit) }
