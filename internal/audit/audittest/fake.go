// Package audittest provides an in-memory audit.Recorder and audit.Log.
package audittest

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/audit"
)

// Fake records events in memory, newest last.
type Fake struct {
	mu      sync.Mutex
	entries []audit.Entry
	now     func() time.Time
}

var (
	_ audit.Recorder = (*Fake)(nil)
	_ audit.Log      = (*Fake)(nil)
)

// NewFake returns an empty Fake.
func NewFake() *Fake { return &Fake{now: time.Now} }

// Record implements audit.Recorder.
func (f *Fake) Record(ctx context.Context, e audit.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.Action == "" {
		return errors.New("audit event needs an action")
	}
	if e.Time.IsZero() {
		e.Time = f.now().UTC()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, audit.Entry{Event: e, ID: uuid.NewString(), ActorUsername: e.Actor.Username})
	return nil
}

// Events returns every recorded event in order.
func (f *Fake) Events() []audit.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]audit.Event, len(f.entries))
	for i, e := range f.entries {
		out[i] = e.Event
	}
	return out
}

// Actions returns the action of every recorded event in order.
func (f *Fake) Actions() []audit.Action {
	var out []audit.Action
	for _, e := range f.Events() {
		out = append(out, e.Action)
	}
	return out
}

// List implements audit.Log, newest first. The cursor is an offset.
func (f *Fake) List(_ context.Context, flt audit.Filter) ([]audit.Entry, string, error) {
	f.mu.Lock()
	var out []audit.Entry
	for i := len(f.entries) - 1; i >= 0; i-- {
		e := f.entries[i]
		if flt.ActorUsername != "" && e.ActorUsername != flt.ActorUsername {
			continue
		}
		if len(flt.Actions) > 0 && !slices.Contains(flt.Actions, e.Action) {
			continue
		}
		if !flt.Since.IsZero() && e.Time.Before(flt.Since) {
			continue
		}
		if !flt.Until.IsZero() && !e.Time.Before(flt.Until) {
			continue
		}
		out = append(out, e)
	}
	f.mu.Unlock()
	offset := 0
	if flt.Cursor != "" {
		n, err := strconv.Atoi(flt.Cursor)
		if err != nil || n < 0 {
			return nil, "", errors.New("bad cursor")
		}
		offset = min(n, len(out))
	}
	out = out[offset:]
	next := ""
	if flt.Limit > 0 && len(out) > flt.Limit {
		out = out[:flt.Limit]
		next = strconv.Itoa(offset + flt.Limit)
	}
	return out, next, nil
}
