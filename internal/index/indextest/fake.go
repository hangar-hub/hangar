// Package indextest provides an in-memory index.Indexer. Tests set the
// Summary, headers and file sizes each Commit should have.
package indextest

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/index"
	"github.com/jamesagarside/hangar/internal/model"
)

type key struct {
	repo   string
	commit model.CommitSHA
}

// Fake is an in-memory index.Indexer. Index stores the Summary set with
// SetSummary (or an empty one) as the Commit's result; Quants groups the
// GGUF files set with SetFiles using index.GroupQuants.
type Fake struct {
	mu        sync.Mutex
	prepared  map[key]*index.Summary
	stored    map[key]*index.Summary
	headers   map[string]json.RawMessage
	files     map[key]map[string]int64
	indexRuns int
}

var _ index.Indexer = (*Fake)(nil)

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{prepared: map[key]*index.Summary{}, stored: map[key]*index.Summary{}, headers: map[string]json.RawMessage{}, files: map[key]map[string]int64{}}
}

// SetSummary sets what Index produces for a Repo's Commit.
func (f *Fake) SetSummary(repoUUID uuid.UUID, commit model.CommitSHA, s *index.Summary) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepared[key{repoUUID.String(), commit}] = s
}

// SetHeader sets the header JSON FileHeader returns for a path.
func (f *Fake) SetHeader(id model.RepoID, commit model.CommitSHA, path, header string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headers[id.Path()+"@"+string(commit)+":"+path] = json.RawMessage(header)
}

// SetFiles sets the files and sizes Quants groups for a Repo's Commit.
func (f *Fake) SetFiles(id model.RepoID, commit model.CommitSHA, sizes map[string]int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[key{id.Path(), commit}] = sizes
}

// IndexRuns returns how many times Index ran.
func (f *Fake) IndexRuns() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.indexRuns
}

// Index implements index.Indexer.
func (f *Fake) Index(ctx context.Context, repoUUID uuid.UUID, _ model.RepoID, commit model.CommitSHA) (*index.Summary, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.indexRuns++
	k := key{repoUUID.String(), commit}
	s, ok := f.prepared[k]
	if !ok {
		s = &index.Summary{Formats: []string{}}
	}
	f.stored[k] = s
	out := *s
	return &out, nil
}

// Get implements index.Indexer.
func (f *Fake) Get(_ context.Context, repoUUID uuid.UUID, commit model.CommitSHA) (*index.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.stored[key{repoUUID.String(), commit}]
	if !ok {
		return nil, index.ErrNotIndexed
	}
	out := *s
	return &out, nil
}

// FileHeader implements index.Indexer.
func (f *Fake) FileHeader(_ context.Context, id model.RepoID, commit model.CommitSHA, path string) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h, ok := f.headers[id.Path()+"@"+string(commit)+":"+path]
	if !ok {
		return nil, fmt.Errorf("%w: %s", index.ErrNoHeader, path)
	}
	return h, nil
}

// Quants implements index.Indexer.
func (f *Fake) Quants(_ context.Context, id model.RepoID, commit model.CommitSHA) ([]index.QuantGroup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return index.GroupQuants(f.files[key{id.Path(), commit}]), nil
}
