// Package webhooktest provides a webhook.Emitter that records events.
package webhooktest

import (
	"context"
	"errors"
	"sync"

	"github.com/hangar-hub/hangar/internal/webhook"
)

// Fake records emitted events. Set Err to make Emit fail.
type Fake struct {
	Err error

	mu     sync.Mutex
	events []webhook.Event
}

var _ webhook.Emitter = (*Fake)(nil)

// Emit implements webhook.Emitter.
func (f *Fake) Emit(_ context.Context, e webhook.Event) error {
	if f.Err != nil {
		return f.Err
	}
	if e.Event == "" {
		return errors.New("webhook event needs a name")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return nil
}

// Events returns every emitted event in order.
func (f *Fake) Events() []webhook.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]webhook.Event(nil), f.events...)
}

// Names returns the name of every emitted event in order.
func (f *Fake) Names() []webhook.EventName {
	var out []webhook.EventName
	for _, e := range f.Events() {
		out = append(out, e.Event)
	}
	return out
}
