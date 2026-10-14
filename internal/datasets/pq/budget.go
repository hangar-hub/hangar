package pq

import (
	"errors"
	"fmt"
	"math"
	"sync/atomic"
)

// DefaultBudgetBytes is the decoded Parquet data one Dataset Viewer request
// may hold, the Hub's rows_index.max_arrow_data_in_memory (300 MB).
const DefaultBudgetBytes = 300_000_000

// ErrTooBig means reading the rows needs more decoded data than the
// request's Budget allows. Errors that wrap it are *TooBigError.
var ErrTooBig = errors.New("parquet: scan size limit exceeded")

// TooBigError is the error for a read that would go over a Budget.
type TooBigError struct {
	// Read is the decoded data the read needs, counting what the request
	// already holds.
	Read int64
	// Limit is the Budget's limit.
	Limit int64
}

// Error gives the Hub's message for rows too big to read.
func (e *TooBigError) Error() string {
	return fmt.Sprintf("Scan size limit exceeded: attempted to read %d bytes, limit is %d bytes", e.Read, e.Limit)
}

// Is makes errors.Is(err, ErrTooBig) true.
func (e *TooBigError) Is(target error) bool { return target == ErrTooBig }

// Budget limits the decoded Parquet data a request holds at once: decoded
// pages, the values built from them and their levels, as Hangar estimates
// them from the page headers before parquet-go decodes anything. A nil
// Budget allows any amount. A Budget is safe for concurrent use.
type Budget struct {
	limit int64
	used  atomic.Int64
}

// NewBudget returns a Budget of limit bytes.
func NewBudget(limit int64) *Budget { return &Budget{limit: limit} }

// Used returns the bytes charged and not released.
func (b *Budget) Used() int64 {
	if b == nil {
		return 0
	}
	return b.used.Load()
}

// remaining returns what can still be charged.
func (b *Budget) remaining() int64 {
	if b == nil {
		return math.MaxInt64
	}
	return max(b.limit-b.used.Load(), 0)
}

// charge takes n bytes from the Budget, or fails with a *TooBigError and
// takes nothing.
func (b *Budget) charge(n int64) error {
	if b == nil || n <= 0 {
		return nil
	}
	if used := b.used.Add(n); used > b.limit {
		b.used.Add(-n)
		return &TooBigError{Read: used, Limit: b.limit}
	}
	return nil
}

// release gives back n bytes charged earlier.
func (b *Budget) release(n int64) {
	if b != nil && n > 0 {
		b.used.Add(-n)
	}
}

// tooBig is the error for a read of n more bytes than b holds now.
func (b *Budget) tooBig(n int64) error {
	return &TooBigError{Read: b.Used() + n, Limit: b.limit}
}
