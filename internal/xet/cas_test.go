package xet_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/db/dbtest"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/xet"
	"github.com/hangar-hub/hangar/internal/xet/xettest"
)

type casEnv struct {
	*env
	url   string
	token string
	srv   *httptest.Server
}

// newCAS serves the CAS handler behind the request ID middleware, as
// httpx.Dispatch would, with the Ingestor wrapped by wrap if given.
func newCAS(t *testing.T, casURL string, heartbeat time.Duration, wrap func(xet.Ingestor) xet.Ingestor) *casEnv {
	t.Helper()
	return newCASWith(t, xet.HandlerOptions{CASURL: casURL, Heartbeat: heartbeat}, wrap, nil)
}

// newCASWith is newCAS with every handler option, and middleware run
// inside the request ID middleware when mw is given.
func newCASWith(t *testing.T, o xet.HandlerOptions, wrap func(xet.Ingestor) xet.Ingestor, mw func(http.Handler) http.Handler) *casEnv {
	t.Helper()
	e := newEnv(t, dbtest.SQLite(t))
	var ing xet.Ingestor = e.svc
	if wrap != nil {
		ing = wrap(ing)
	}
	h, err := xet.NewHandler(ing, o)
	require.NoError(t, err)
	var inner http.Handler = h
	if mw != nil {
		inner = mw(h)
	}
	srv := httptest.NewServer(httpx.RequestIDs(inner))
	t.Cleanup(srv.Close)
	tok, err := e.svc.IssueWriteToken(context.Background(), xet.TokenRequest{Principal: alice(), Repo: aliceRepo, Revision: "main"})
	require.NoError(t, err)
	return &casEnv{env: e, url: srv.URL, token: tok.AccessToken, srv: srv}
}

func (c *casEnv) do(t *testing.T, method, path, token string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, c.url+path, bytes.NewReader(body))
	require.NoError(t, err)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func readJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var v map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&v))
	return v
}

func ndjson(t *testing.T, resp *http.Response) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		var v map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &v), sc.Text())
		out = append(out, v)
	}
	require.NoError(t, sc.Err())
	return out
}

func TestCASRoutes(t *testing.T) {
	c := newCAS(t, "https://hangar.example.com/xet-cas", 0, nil)
	data := textBytes(40_000, 6)
	u := xettest.BuildUpload([][]byte{data}, xettest.UploadOptions{ChunkSize: 4096, ChunksPerXorb: 4,
		Schemes: []xet.CompressionScheme{xet.CompressionLZ4}})
	x := u.Xorbs[0]

	// Telemetry at the host root (and under the prefix) is accepted and discarded.
	require.Equal(t, http.StatusOK, c.do(t, http.MethodPost, "/v1/telemetry", "", []byte(`{"a":1}`)).StatusCode)
	require.Equal(t, http.StatusOK, c.do(t, http.MethodPost, "/xet-cas/v1/telemetry", "", nil).StatusCode)
	require.Equal(t, http.StatusMethodNotAllowed, c.do(t, http.MethodGet, "/v1/telemetry", "", nil).StatusCode)

	// Every other route needs a valid token.
	resp := c.do(t, http.MethodPost, "/xet-cas/v1/xorbs/default/"+x.Hash.String(), "", x.Body)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.NotEmpty(t, resp.Header.Get("X-Error-Message"))
	require.Equal(t, http.StatusUnauthorized, c.do(t, http.MethodGet, "/xet-cas/v1/chunks/default/"+x.Hash.String(), "hf_hub_token", nil).StatusCode)

	// Global deduplication: always 404, for any prefix.
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		require.Equal(t, http.StatusNotFound, c.do(t, m, "/xet-cas/v1/chunks/default/"+x.Hash.String(), c.token, nil).StatusCode)
		require.Equal(t, http.StatusNotFound, c.do(t, m, "/xet-cas/v1/chunks/default-merkledb/"+x.Hash.String(), c.token, nil).StatusCode)
	}

	// Xorbs: inserted, then already there.
	resp = c.do(t, http.MethodPost, "/xet-cas/v1/xorbs/default/"+x.Hash.String(), c.token, x.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, map[string]any{"was_inserted": true}, readJSON(t, resp))
	resp = c.do(t, http.MethodPost, "/xet-cas/v1/xorbs/default/"+x.Hash.String(), c.token, x.Body)
	require.Equal(t, map[string]any{"was_inserted": false}, readJSON(t, resp))
	for _, other := range u.Xorbs[1:] {
		require.Equal(t, http.StatusOK, c.do(t, http.MethodPost, "/xet-cas/v1/xorbs/default/"+other.Hash.String(), c.token, other.Body).StatusCode)
	}

	// Bad xorbs.
	unstored := xettest.BuildXorb([][]byte{[]byte("not stored")}, false)
	resp = c.do(t, http.MethodPost, "/xet-cas/v1/xorbs/default/"+unstored.Hash.String(), c.token, x.Body)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, resp.Header.Get("X-Error-Message"), "does not match")
	require.Equal(t, http.StatusBadRequest, c.do(t, http.MethodPost, "/xet-cas/v1/xorbs/default/XYZ", c.token, x.Body).StatusCode)
	require.Equal(t, http.StatusMethodNotAllowed, c.do(t, http.MethodGet, "/xet-cas/v1/xorbs/default/"+x.Hash.String(), c.token, nil).StatusCode)
	req, err := http.NewRequest(http.MethodPost, c.url+"/xet-cas/v1/xorbs/default/"+x.Hash.String(), strings.NewReader("x"))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.ContentLength = xet.MaxXorbSize + 1
	resp, err = http.DefaultClient.Do(req)
	if err == nil {
		require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
		_ = resp.Body.Close()
	}

	// Unknown routes.
	require.Equal(t, http.StatusNotFound, c.do(t, http.MethodGet, "/xet-cas/v1/reconstructions/abc", c.token, nil).StatusCode)
	require.Equal(t, http.StatusNotFound, c.do(t, http.MethodGet, "/elsewhere", c.token, nil).StatusCode)

	// v1 shard.
	resp = c.do(t, http.MethodPost, "/xet-cas/v1/shards", c.token, u.ShardBytes())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, map[string]any{"result": float64(1)}, readJSON(t, resp))
	require.Equal(t, data, c.objectBytes(t, u.Files[0].SHA256))

	// v1 shard naming a xorb Hangar does not have: 400 with the advice.
	missing := xettest.BuildUpload([][]byte{textBytes(9000, 8)}, xettest.UploadOptions{})
	resp = c.do(t, http.MethodPost, "/xet-cas/v1/shards", c.token, missing.ShardBytes())
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, xet.UnknownXorbMessage, resp.Header.Get("X-Error-Message"))
	require.Equal(t, http.StatusBadRequest, c.do(t, http.MethodPost, "/xet-cas/v1/shards", c.token, []byte("garbage")).StatusCode)

	// v2 shard: NDJSON frames ending in a result.
	resp = c.do(t, http.MethodPost, "/xet-cas/v2/shards", c.token, u.ShardBytes())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/x-ndjson", resp.Header.Get("Content-Type"))
	frames := ndjson(t, resp)
	require.Equal(t, map[string]any{"type": "validating", "verified": float64(0), "total": float64(0)}, frames[0])
	require.Equal(t, map[string]any{"type": "result"}, frames[len(frames)-1])
	require.Contains(t, frames, map[string]any{"type": "committing", "stage": "uploading"})

	// v2 errors arrive as a terminal error frame after a 200.
	resp = c.do(t, http.MethodPost, "/xet-cas/v2/shards", c.token, missing.ShardBytes())
	require.Equal(t, http.StatusOK, resp.StatusCode)
	frames = ndjson(t, resp)
	require.Equal(t, map[string]any{"type": "error", "message": xet.UnknownXorbMessage, "retryable": false}, frames[len(frames)-1])

	// Errors before streaming starts keep their status.
	require.Equal(t, http.StatusUnauthorized, c.do(t, http.MethodPost, "/xet-cas/v2/shards", "bad", u.ShardBytes()).StatusCode)
}

// readOnly makes every token read-scoped.
type readOnly struct{ xet.Ingestor }

func (r readOnly) VerifyToken(ctx context.Context, tok string) (*xet.Claims, error) {
	c, err := r.Ingestor.VerifyToken(ctx, tok)
	if err != nil {
		return nil, err
	}
	c.Scope = xet.ScopeRead
	return c, nil
}

func TestCASScopesExpiryAndReject(t *testing.T) {
	c := newCAS(t, "https://hangar.example.com/xet-cas", 0, func(i xet.Ingestor) xet.Ingestor { return readOnly{i} })
	x := xettest.BuildXorb([][]byte{[]byte("abc")}, false)
	require.Equal(t, http.StatusForbidden, c.do(t, http.MethodPost, "/xet-cas/v1/xorbs/default/"+x.Hash.String(), c.token, x.Body).StatusCode)
	require.Equal(t, http.StatusForbidden, c.do(t, http.MethodPost, "/xet-cas/v1/shards", c.token, nil).StatusCode)
	require.Equal(t, http.StatusNotFound, c.do(t, http.MethodGet, "/xet-cas/v1/chunks/default/"+x.Hash.String(), c.token, nil).StatusCode,
		"read scope is enough for the deduplication query")

	c = newCAS(t, "https://hangar.example.com/xet-cas", 0, nil)
	c.advance(time.Hour + xet.DefaultClockSkew + time.Second)
	resp := c.do(t, http.MethodPost, "/xet-cas/v1/xorbs/default/"+x.Hash.String(), c.token, x.Body)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "an expired token gets 401")

	// Reject mode refuses CAS writes with the message to turn Xet off.
	f := xettest.NewFake("https://hangar.example.com/xet-cas", time.Hour)
	tok, err := f.IssueWriteToken(context.Background(), xet.TokenRequest{Principal: alice(), Repo: aliceRepo})
	require.NoError(t, err)
	f.Reject = true
	h, err := xet.NewHandler(rejecting{f}, xet.HandlerOptions{CASURL: "https://hangar.example.com/xet-cas"})
	require.NoError(t, err)
	srv := httptest.NewServer(httpx.RequestIDs(h))
	defer srv.Close()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/xet-cas/v1/xorbs/default/"+x.Hash.String(), bytes.NewReader(x.Body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Equal(t, xet.RejectMessage, resp.Header.Get("X-Error-Message"))
}

// rejecting answers writes as the Service does in reject mode.
type rejecting struct{ *xettest.Fake }

func (r rejecting) PutXorb(context.Context, *xet.Claims, string, io.Reader, int64) (bool, error) {
	return false, xet.ErrRejected
}

func TestCASOwnHostname(t *testing.T) {
	c := newCAS(t, "https://cas.example.com", 0, nil)
	x := xettest.BuildXorb([][]byte{[]byte("abc")}, false)
	resp := c.do(t, http.MethodPost, "/v1/xorbs/default/"+x.Hash.String(), c.token, x.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, http.StatusOK, c.do(t, http.MethodPost, "/v1/telemetry", "", nil).StatusCode)
	require.Equal(t, http.StatusNotFound, c.do(t, http.MethodPost, "/xet-cas/v1/xorbs/default/"+x.Hash.String(), c.token, x.Body).StatusCode)

	_, err := xet.NewHandler(nil, xet.HandlerOptions{CASURL: "https://h"})
	require.Error(t, err)
	_, err = xet.NewHandler(c.svc, xet.HandlerOptions{CASURL: "/relative"})
	require.Error(t, err)
}

// slowShards pauses before reassembly, like a large file on slow storage.
type slowShards struct {
	xet.Ingestor
	pause time.Duration
}

func (s slowShards) AcceptShard(ctx context.Context, c *xet.Claims, r io.Reader, size int64, events func(xet.ShardEvent)) (*xet.ShardResult, error) {
	return s.Ingestor.AcceptShard(ctx, c, r, size, func(e xet.ShardEvent) {
		events(e)
		if e.Type == xet.EventCommitting && e.Stage == xet.StageUploading {
			select {
			case <-time.After(s.pause):
			case <-ctx.Done():
			}
		}
	})
}

// TestCASHeartbeatsBehindIdleProxy is the 30-second idle timeout case,
// scaled down: a proxy that drops connections silent for 300 ms, and a
// shard that takes 1.5 s. Heartbeats every 100 ms keep it alive; without
// them the proxy drops the connection.
func TestCASHeartbeatsBehindIdleProxy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		heartbeat time.Duration
		survives  bool
	}{
		{"heartbeats", 100 * time.Millisecond, true},
		{"no heartbeats in time", time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCAS(t, "https://hangar.example.com/xet-cas", tc.heartbeat, func(i xet.Ingestor) xet.Ingestor {
				return slowShards{Ingestor: i, pause: 1500 * time.Millisecond}
			})
			u := xettest.BuildUpload([][]byte{textBytes(20_000, 9)}, xettest.UploadOptions{ChunkSize: 4096})
			c.putAll(t, u)
			proxy, err := xettest.NewIdleProxy(strings.TrimPrefix(c.url, "http://"), 300*time.Millisecond)
			require.NoError(t, err)
			defer proxy.Close()

			req, err := http.NewRequest(http.MethodPost, "http://"+proxy.Addr+"/xet-cas/v2/shards", bytes.NewReader(u.ShardBytes()))
			require.NoError(t, err)
			req.Header.Set("Authorization", "Bearer "+c.token)
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			body, readErr := io.ReadAll(resp.Body)
			lines := strings.Split(strings.TrimSpace(string(body)), "\n")
			if tc.survives {
				require.NoError(t, readErr)
				require.JSONEq(t, `{"type":"result"}`, lines[len(lines)-1])
				repeats := 0
				for _, l := range lines {
					if l == `{"type":"committing","stage":"uploading"}` {
						repeats++
					}
				}
				require.GreaterOrEqual(t, repeats, 5, "the last frame is repeated while the shard is processed")
				require.Zero(t, proxy.Dropped.Load())
			} else {
				require.NotContains(t, string(body), `"result"`)
				require.Eventually(t, func() bool { return proxy.Dropped.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
			}
		})
	}
}
