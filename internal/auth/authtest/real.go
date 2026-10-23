package authtest

import (
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hangar-hub/hangar/internal/audit"
	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/org"
)

// Clock is a settable clock for tests.
type Clock struct {
	mu   sync.Mutex
	t    time.Time
	tick time.Duration
}

// NewClock returns a Clock at t.
func NewClock(t time.Time) *Clock { return &Clock{t: t} }

// Now returns the clock's time, then moves it forward by the tick, if any.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.t
	c.t = c.t.Add(c.tick)
	return now
}

// SetTick makes every later call to Now move the clock forward by d, so
// time passes between the steps of one request, as it does on a slow
// machine. Zero stops it.
func (c *Clock) SetTick(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tick = d
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// RealOptions configures NewService.
type RealOptions struct {
	// Config changes the default test configuration.
	Config func(*config.Config)
	// Audit records events; nil uses audit.NewDB on the same database.
	Audit audit.Recorder
	// Clock, if set, drives the Service's clock.
	Clock *Clock
	// Upstreams resolves Upstream names for upstream tokens.
	Upstreams auth.UpstreamLookup
	// Log receives the Service's operational messages; nil discards them.
	Log *slog.Logger
}

// NewService returns the real, database-backed auth.Service on d, for
// tests in other packages that want the real Authenticator and Authz. The
// configuration is the default with public_url http://hangar.test and a
// random Instance key.
func NewService(t testing.TB, d *db.DB, o RealOptions) (*auth.Service, *org.DBService) {
	t.Helper()
	cfg := config.Default()
	cfg.Server.PublicURL = "http://hangar.test"
	key := make([]byte, config.InstanceKeyBytes)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	cfg.Security.InstanceKey = base64.StdEncoding.EncodeToString(key)
	if o.Config != nil {
		o.Config(&cfg)
	}
	rec := o.Audit
	if rec == nil {
		rec = audit.NewDB(d, nil)
	}
	orgs := org.NewDB(d, rec)
	opts := auth.Options{DB: d, Config: &cfg, Orgs: orgs, Audit: rec, Upstreams: o.Upstreams, Log: o.Log}
	if o.Clock != nil {
		opts.Now = o.Clock.Now
	}
	s, err := auth.New(opts)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	return s, orgs
}
