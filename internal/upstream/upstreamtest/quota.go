package upstreamtest

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/upstream"
)

// QuotaCheck is one CheckQuota call a Quota saw.
type QuotaCheck struct {
	Namespace uuid.UUID
	Adding    int64
	// Refused is true when the call returned a quota refusal.
	Refused bool
}

// Quota is a fake upstream.QuotaChecker: per-Namespace limits over a usage
// the test sets, objects the test says a Namespace holds, and a record of
// every check. Namespaces without a limit have no quota. W1's
// gc.Accounting replaces it in a real Instance.
type Quota struct {
	mu       sync.Mutex
	limits   map[uuid.UUID]int64
	used     map[uuid.UUID]int64
	held     map[uuid.UUID]map[string]bool
	checks   []QuotaCheck
	failNext error
}

// NewQuota returns a Quota with no limits.
func NewQuota() *Quota {
	return &Quota{limits: map[uuid.UUID]int64{}, used: map[uuid.UUID]int64{}, held: map[uuid.UUID]map[string]bool{}}
}

// SetLimit sets a Namespace's quota in bytes. Zero removes it.
func (q *Quota) SetLimit(ns uuid.UUID, bytes int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if bytes == 0 {
		delete(q.limits, ns)
		return
	}
	q.limits[ns] = bytes
}

// SetUsed sets the bytes a Namespace already uses.
func (q *Quota) SetUsed(ns uuid.UUID, bytes int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.used[ns] = bytes
}

// Hold records that a Repo of the Namespace already holds an object, so
// NewLFSBytes does not count it.
func (q *Quota) Hold(ns uuid.UUID, oid string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.held[ns] == nil {
		q.held[ns] = map[string]bool{}
	}
	q.held[ns][oid] = true
}

// FailNext makes the next CheckQuota or NewLFSBytes call return err, as a
// database failure would.
func (q *Quota) FailNext(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.failNext = err
}

// Checks returns every CheckQuota call so far.
func (q *Quota) Checks() []QuotaCheck {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]QuotaCheck(nil), q.checks...)
}

// Refusals returns the CheckQuota calls that refused.
func (q *Quota) Refusals() []QuotaCheck {
	var out []QuotaCheck
	for _, c := range q.Checks() {
		if c.Refused {
			out = append(out, c)
		}
	}
	return out
}

func (q *Quota) takeFailure() error {
	err := q.failNext
	q.failNext = nil
	return err
}

// QuotaRefusal is the error a Quota refuses with, worded like W1's
// gc.QuotaError.
type QuotaRefusal struct {
	Used, Adding, Limit int64
}

func (e *QuotaRefusal) Error() string {
	return fmt.Sprintf("This change needs %d B more, but the Namespace has used %d B of its %d B storage quota. Delete files or ask an admin to raise the quota.",
		e.Adding, e.Used, e.Limit)
}

// CheckQuota implements upstream.QuotaChecker.
func (q *Quota) CheckQuota(_ context.Context, ns uuid.UUID, adding int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.takeFailure(); err != nil {
		return err
	}
	c := QuotaCheck{Namespace: ns, Adding: adding}
	limit, ok := q.limits[ns]
	used := q.used[ns]
	if ok && adding > 0 && used+adding > limit {
		c.Refused = true
	}
	q.checks = append(q.checks, c)
	if c.Refused {
		return upstream.QuotaExceeded(&QuotaRefusal{Used: used, Adding: adding, Limit: limit})
	}
	return nil
}

// NewLFSBytes implements upstream.QuotaChecker.
func (q *Quota) NewLFSBytes(_ context.Context, ns uuid.UUID, objects map[string]int64) (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.takeFailure(); err != nil {
		return 0, err
	}
	var n int64
	for oid, size := range objects {
		if !q.held[ns][oid] {
			n += size
		}
	}
	return n, nil
}

var _ upstream.QuotaChecker = (*Quota)(nil)
