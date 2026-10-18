//go:build compat && xetserver

// Package xet holds the Xet upload compatibility suite (compat/xet, pytest)
// and the Go test server it drives. The server is the Hub API from
// internal/hfapi (through hfapitest) with the real Xet Ingestor and CAS
// handler from internal/xet in front of it. It needs W4's internal/hfapi,
// so it builds only with the xetserver tag; add xetreal for W2's Repo
// service and W1's filesystem Blob store (see README.md).
package xet

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/blob"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/hfapi"
	"github.com/hangar-hub/hangar/internal/hfapi/hfapitest"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/xet"
	"github.com/hangar-hub/hangar/internal/xet/xettest"
)

// Environment variables that configure the test server.
const (
	// EnvServeInfo names the file the server writes its endpoint and Tokens
	// to; the server then runs until its standard input closes.
	EnvServeInfo = "HANGAR_XET_SERVE_INFO"
	// EnvMode is "ingest" (default) or "reject" (hfapi.xet).
	EnvMode = "HANGAR_XET_MODE"
	// EnvTokenTTL is xet.token_ttl, such as "40s".
	EnvTokenTTL = "HANGAR_XET_TOKEN_TTL"
	// EnvXorbRate slows reading xorb bodies to this many bytes per second,
	// to make an upload long without large files.
	EnvXorbRate = "HANGAR_XET_XORB_BYTES_PER_SEC"
	// EnvShardDelay pauses each shard before reassembly, like a large file
	// on slow storage.
	EnvShardDelay = "HANGAR_XET_SHARD_DELAY"
	// EnvIdleProxy puts a proxy with this idle timeout in front of the
	// whole server; clients reach Hangar only through it.
	EnvIdleProxy = "HANGAR_XET_PROXY_IDLE"
)

// serveInfo is what the pytest suite reads.
type serveInfo struct {
	Endpoint string            `json:"endpoint"`
	Tokens   map[string]string `json:"tokens"`
	Org      string            `json:"org"`
	Backend  string            `json:"backend"`
}

// backend is what backend_fake_test.go or backend_real_test.go provides.
type backend struct {
	name string
	// opts adjusts the Hub API test environment (its Repo service).
	opts func(*hfapitest.Options)
	// db and blobs are what the Ingestor uses.
	db    *db.DB
	blobs blob.Store
}

// stats counts what the CAS saw, for the suite's assertions.
type stats struct {
	TokenRequests atomic.Int64
	XorbPosts     atomic.Int64
	XorbsInserted atomic.Int64
	ShardPostsV1  atomic.Int64
	ShardPostsV2  atomic.Int64
	ChunkQueries  atomic.Int64
	Telemetry     atomic.Int64
	CAS401        atomic.Int64
	CAS403        atomic.Int64
	ShardsActive  atomic.Int64
	// ShardsCancelled counts shard uploads whose client went away
	// before the result, such as a connection a proxy dropped.
	ShardsCancelled atomic.Int64
}

func (s *stats) snapshot(proxyDropped int64) map[string]int64 {
	return map[string]int64{
		"token_requests": s.TokenRequests.Load(), "xorb_posts": s.XorbPosts.Load(),
		"xorbs_inserted": s.XorbsInserted.Load(), "shard_posts_v1": s.ShardPostsV1.Load(),
		"shard_posts_v2": s.ShardPostsV2.Load(), "chunk_queries": s.ChunkQueries.Load(),
		"telemetry": s.Telemetry.Load(), "cas_401": s.CAS401.Load(), "cas_403": s.CAS403.Load(),
		"shards_active": s.ShardsActive.Load(), "shards_cancelled": s.ShardsCancelled.Load(),
		"proxy_dropped": proxyDropped,
	}
}

// counting wraps the Ingestor to count inserted xorbs and to hold or slow
// shards for the kill and proxy tests.
type counting struct {
	xet.Ingestor
	st    *stats
	rate  int64
	delay time.Duration
	hold  *gate
}

func (c *counting) PutXorb(ctx context.Context, cl *xet.Claims, hash string, r io.Reader, size int64) (bool, error) {
	if c.rate > 0 {
		r = &throttled{r: r, rate: c.rate}
	}
	ins, err := c.Ingestor.PutXorb(ctx, cl, hash, r, size)
	if ins {
		c.st.XorbsInserted.Add(1)
	}
	return ins, err
}

func (c *counting) AcceptShard(ctx context.Context, cl *xet.Claims, r io.Reader, size int64, events func(xet.ShardEvent)) (*xet.ShardResult, error) {
	c.st.ShardsActive.Add(1)
	defer c.st.ShardsActive.Add(-1)
	if err := c.hold.wait(ctx); err != nil {
		return nil, err
	}
	defer func() {
		if ctx.Err() != nil {
			c.st.ShardsCancelled.Add(1)
		}
	}()
	return c.Ingestor.AcceptShard(ctx, cl, r, size, func(e xet.ShardEvent) {
		if events != nil {
			events(e)
		}
		if c.delay > 0 && e.Type == xet.EventCommitting && e.Stage == xet.StageUploading {
			select {
			case <-time.After(c.delay):
			case <-ctx.Done():
			}
		}
	})
}

// gate blocks shards while closed.
type gate struct {
	mu   sync.Mutex
	open chan struct{}
}

func newGate() *gate {
	g := &gate{open: make(chan struct{})}
	close(g.open)
	return g
}

func (g *gate) set(hold bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.open:
		if hold {
			g.open = make(chan struct{})
		}
	default:
		if !hold {
			close(g.open)
		}
	}
}

func (g *gate) wait(ctx context.Context) error {
	g.mu.Lock()
	ch := g.open
	g.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// throttled reads at most rate bytes per second.
type throttled struct {
	r     io.Reader
	rate  int64
	start time.Time
	n     int64
}

func (t *throttled) Read(p []byte) (int, error) {
	if t.start.IsZero() {
		t.start = time.Now()
	}
	if len(p) > 16<<10 {
		p = p[:16<<10]
	}
	if due := t.start.Add(time.Duration(float64(t.n) / float64(t.rate) * float64(time.Second))); time.Now().Before(due) {
		time.Sleep(time.Until(due))
	}
	n, err := t.r.Read(p)
	t.n += int64(n)
	return n, err
}

// statusRecorder captures the status of CAS responses.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func durationEnv(t *testing.T, name string) time.Duration {
	v := os.Getenv(name)
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	require.NoError(t, err, name)
	return d
}

// startServer starts the front server: the CAS handler for /xet-cas and
// /v1/telemetry, test hooks under /_test, and everything else proxied to
// the Hub API test server. With EnvIdleProxy, an idle-timeout proxy sits
// in front of it all.
func startServer(t *testing.T, be backend) (serveInfo, func() int64) {
	t.Helper()
	front, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	public := "http://" + front.Addr().String()

	var proxy *xettest.IdleProxy
	if idle := durationEnv(t, EnvIdleProxy); idle > 0 {
		proxy, err = xettest.NewIdleProxy(front.Addr().String(), idle)
		require.NoError(t, err)
		t.Cleanup(func() { _ = proxy.Close() })
		public = "http://" + proxy.Addr
	}
	reject := os.Getenv(EnvMode) == "reject"
	st := &stats{}
	hold := newGate()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	var ing *counting
	opts := hfapitest.Options{
		Settings: func(s *hfapi.Settings) {
			s.PublicURL = public
			s.XetReject = reject
		},
		Deps: func(d *hfapi.Deps) {
			svc, err := xet.New(xet.Options{
				DB: be.db, Blobs: be.blobs, LFS: d.LFS, InstanceKey: []byte(strings.Repeat("k", 32)),
				CASURL: public + "/xet-cas", TokenTTL: durationEnv(t, EnvTokenTTL), Reject: reject,
				TempDir: t.TempDir(), Log: log,
			})
			require.NoError(t, err)
			ing = &counting{Ingestor: svc, st: st, delay: durationEnv(t, EnvShardDelay), hold: hold}
			if v := os.Getenv(EnvXorbRate); v != "" {
				rate, err := strconv.ParseInt(v, 10, 64)
				require.NoError(t, err, EnvXorbRate)
				ing.rate = rate
			}
			d.Xet = ing
		},
	}
	be.opts(&opts)
	hub := hfapitest.New(t, opts)

	cas, err := xet.NewHandler(ing, xet.HandlerOptions{CASURL: public + "/xet-cas", Log: log})
	require.NoError(t, err)
	target, err := url.Parse(hub.URL)
	require.NoError(t, err)
	rp := httputil.NewSingleHostReverseProxy(target)
	rp.FlushInterval = -1
	casCounted := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		switch {
		case strings.HasPrefix(p, "/xet-cas/v1/xorbs/"):
			st.XorbPosts.Add(1)
		case p == "/xet-cas/v1/shards":
			st.ShardPostsV1.Add(1)
		case p == "/xet-cas/v2/shards":
			st.ShardPostsV2.Add(1)
		case strings.HasPrefix(p, "/xet-cas/v1/chunks/"):
			st.ChunkQueries.Add(1)
		case p == "/v1/telemetry":
			st.Telemetry.Add(1)
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		cas.ServeHTTP(rec, r)
		switch rec.status {
		case http.StatusUnauthorized:
			st.CAS401.Add(1)
		case http.StatusForbidden:
			st.CAS403.Add(1)
		}
	})
	dropped := func() int64 {
		if proxy == nil {
			return 0
		}
		return proxy.Dropped.Load()
	}
	mux := http.NewServeMux()
	mux.Handle("/xet-cas/", httpx.RequestIDs(casCounted))
	mux.Handle("/v1/telemetry", httpx.RequestIDs(casCounted))
	mux.HandleFunc("/_test/stats", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st.snapshot(dropped()))
	})
	mux.HandleFunc("/_test/hold-shards", func(w http.ResponseWriter, r *http.Request) {
		hold.set(r.URL.Query().Get("on") == "1")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.EscapedPath(), "/xet-write-token/") {
			st.TokenRequests.Add(1)
		}
		rp.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = srv.Serve(front) }()
	t.Cleanup(func() { _ = srv.Close() })

	info := serveInfo{Endpoint: public, Tokens: map[string]string{}, Org: hfapitest.Org, Backend: be.name}
	for name, u := range hub.Users {
		info.Tokens[name] = u.Token
	}
	return info, dropped
}

// TestServeXet starts the server. Without HANGAR_XET_SERVE_INFO it checks
// the CAS answers through the Hub's token and stops; with it, pytest
// drives it until standard input closes.
func TestServeXet(t *testing.T) {
	info, _ := startServer(t, currentBackend(t))
	path := os.Getenv(EnvServeInfo)
	if path == "" {
		req, err := http.NewRequest(http.MethodPost, info.Endpoint+"/v1/telemetry", strings.NewReader("{}"))
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		return
	}
	b, err := json.Marshal(info)
	require.NoError(t, err)
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	require.NoError(t, os.WriteFile(tmp, b, 0o600))
	require.NoError(t, os.Rename(tmp, path))
	_, _ = io.Copy(io.Discard, bufio.NewReader(os.Stdin))
}
