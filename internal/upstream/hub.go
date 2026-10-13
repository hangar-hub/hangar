package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// hub tracks transfers running in this process, so concurrent
// Pull-through requests for one object share one transfer and read its
// bytes as they arrive (06 "Pull-through").
type hub struct {
	dir string

	mu     sync.Mutex
	active map[string]*transfer
}

func newHub(dir string) *hub { return &hub{dir: dir, active: map[string]*transfer{}} }

// transfer is one object being pulled. Spooled transfers also write the
// bytes to a local file that readers tail.
type transfer struct {
	h    *hub
	oid  string
	size int64

	mu      sync.Mutex
	spool   *os.File
	path    string
	written int64
	done    bool
	err     error
	readers int
	changed chan struct{}
}

func (h *hub) lookup(oid string) *transfer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.active[oid]
}

// begin registers a transfer of oid. It returns the existing transfer and
// false when one is already running.
func (h *hub) begin(oid string, size int64, spool bool) (*transfer, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if t, ok := h.active[oid]; ok {
		return t, false, nil
	}
	t := &transfer{h: h, oid: oid, size: size, changed: make(chan struct{})}
	if spool {
		f, err := os.CreateTemp(h.dir, "hangar-pull-*.part")
		if err != nil {
			return nil, false, fmt.Errorf("create a Pull-through spool file: %w", err)
		}
		t.spool, t.path = f, f.Name()
	}
	h.active[oid] = t
	return t, true, nil
}

// spooled reports whether readers can tail the transfer.
func (t *transfer) spooled() bool { return t.path != "" }

func (t *transfer) notifyLocked() {
	close(t.changed)
	t.changed = make(chan struct{})
}

// Write appends to the spool file and wakes readers.
func (t *transfer) Write(p []byte) (int, error) {
	n, err := t.spool.Write(p)
	t.mu.Lock()
	t.written += int64(n)
	t.notifyLocked()
	t.mu.Unlock()
	return n, err
}

// finish ends the transfer with err (nil on success) and removes it from
// the hub; the spool file goes when the last reader closes.
func (t *transfer) finish(err error) {
	t.h.mu.Lock()
	if t.h.active[t.oid] == t {
		delete(t.h.active, t.oid)
	}
	t.h.mu.Unlock()
	t.mu.Lock()
	t.done, t.err = true, err
	if t.spool != nil {
		_ = t.spool.Close()
	}
	t.notifyLocked()
	cleanup := t.readers == 0
	t.mu.Unlock()
	if cleanup {
		t.removeSpool()
	}
}

func (t *transfer) removeSpool() {
	if t.path != "" {
		_ = os.Remove(t.path)
	}
}

// wait blocks until the transfer ends and returns its error.
func (t *transfer) wait(ctx context.Context) error {
	for {
		t.mu.Lock()
		done, err, ch := t.done, t.err, t.changed
		t.mu.Unlock()
		if done {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
		}
	}
}

// reader returns a reader of the spooled bytes from the start, which
// blocks for bytes not yet written and fails if the transfer fails.
func (t *transfer) reader(ctx context.Context) (io.ReadCloser, error) {
	f, err := os.Open(t.path)
	if err != nil {
		return nil, fmt.Errorf("open the Pull-through spool file: %w", err)
	}
	t.mu.Lock()
	t.readers++
	t.mu.Unlock()
	return &tailReader{ctx: ctx, t: t, f: f}, nil
}

type tailReader struct {
	ctx    context.Context
	t      *transfer
	f      *os.File
	off    int64
	closed bool
}

func (r *tailReader) Read(p []byte) (int, error) {
	for {
		r.t.mu.Lock()
		written, done, terr, ch := r.t.written, r.t.done, r.t.err, r.t.changed
		r.t.mu.Unlock()
		if r.off < written {
			want := min(int64(len(p)), written-r.off)
			n, err := r.f.ReadAt(p[:want], r.off)
			r.off += int64(n)
			if n > 0 {
				return n, nil
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return 0, err
			}
		}
		if done {
			if terr != nil {
				return 0, fmt.Errorf("the transfer from the Upstream failed: %w", terr)
			}
			if r.off >= written {
				return 0, io.EOF
			}
			continue
		}
		select {
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		case <-ch:
		}
	}
}

func (r *tailReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	err := r.f.Close()
	r.t.mu.Lock()
	r.t.readers--
	cleanup := r.t.readers == 0 && r.t.done
	r.t.mu.Unlock()
	if cleanup {
		r.t.removeSpool()
	}
	return err
}
