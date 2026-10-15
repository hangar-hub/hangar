package pipeline

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
)

// readWatch is the Repo service the dataset Jobs read files through. It
// remembers the first error reading a file that says nothing about the
// file's content, such as a Blob store outage, so that a Job can tell a
// dataset that cannot be converted from one it could not read this time.
// The conversion and statistics code wrap errors in many ways, some of
// them without %w, so the error is caught where it happens rather than
// classified afterwards.
type readWatch struct {
	repo.Service

	mu  sync.Mutex
	err error
}

func newReadWatch(s repo.Service) *readWatch { return &readWatch{Service: s} }

// OpenFile implements repo.Service.
func (w *readWatch) OpenFile(ctx context.Context, id model.RepoID, commit model.CommitSHA, path string, rng *blob.Range) (io.ReadCloser, model.Entry, error) {
	rc, e, err := w.Service.OpenFile(ctx, id, commit, path, rng)
	if err != nil {
		w.record(err)
		return nil, e, err
	}
	return &watchedReader{ReadCloser: rc, w: w}, e, nil
}

// record keeps err if it is the first transient read error.
func (w *readWatch) record(err error) {
	if !transientRead(err) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil {
		w.err = err
	}
}

// reset forgets the recorded error, before a new unit of work.
func (w *readWatch) reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.err = nil
}

// failure returns the transient read error recorded since the last reset.
func (w *readWatch) failure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// watchedReader records the errors of a file's reads.
type watchedReader struct {
	io.ReadCloser
	w *readWatch
}

func (r *watchedReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil {
		r.w.record(err)
	}
	return n, err
}

// transientRead reports whether an error reading a Repo file may go away
// when the read is tried again. The end of a file, and a file or object
// that does not exist, is not present or does not match its hash, are
// facts about the dataset, so the Job records them as the dataset's
// failure; any other error, such as a refused connection, a timeout or a
// database error, is not. The Job's own cancellation is checked before
// this, so a deadline here is a request's, which the next try may meet.
func transientRead(err error) bool {
	switch {
	case err == nil,
		errors.Is(err, io.EOF),
		errors.Is(err, repo.ErrNotFound),
		errors.Is(err, repo.ErrRevisionNotFound),
		errors.Is(err, repo.ErrEntryNotFound),
		errors.Is(err, repo.ErrNotPresent),
		errors.Is(err, repo.ErrLFSObjectMissing),
		errors.Is(err, blob.ErrNotFound),
		errors.Is(err, blob.ErrInvalidRange),
		errors.Is(err, blob.ErrSizeMismatch),
		errors.Is(err, blob.ErrChecksumMismatch):
		return false
	}
	return true
}
