package xet_test

import (
	"context"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/xet"
	"github.com/hangar-hub/hangar/internal/xet/xettest"
)

// countingBody counts the bytes the handler reads from a request body.
type countingBody struct {
	io.ReadCloser
	n atomic.Int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// bodyMeter is middleware that reports, as each request finishes, how
// many body bytes its handler read.
func bodyMeter(read chan<- int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cb := &countingBody{ReadCloser: r.Body}
			r.Body = cb
			next.ServeHTTP(w, r)
			read <- cb.n.Load()
		})
	}
}

// greedy reads every xorb body to the end, so only the handler can bound it.
type greedy struct{ xet.Ingestor }

func (g greedy) PutXorb(_ context.Context, _ *xet.Claims, _ string, r io.Reader, _ int64) (bool, error) {
	if _, err := io.Copy(io.Discard, r); err != nil {
		return false, err
	}
	return true, nil
}

// postStream POSTs n bytes of zeros with no declared length (chunked), so
// only the limit applied while reading can stop the handler.
func (c *casEnv) postStream(t *testing.T, path, token string, n int64) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.url+path, io.LimitReader(zeroReader{}, n))
	require.NoError(t, err)
	req.ContentLength = -1
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// The server may close the connection before the client has sent
		// everything; the bytes read are checked on the server side.
		return nil
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func waitRead(t *testing.T, read <-chan int64) int64 {
	t.Helper()
	select {
	case n := <-read:
		return n
	case <-time.After(30 * time.Second):
		t.Fatal("the request did not finish")
		return 0
	}
}

// TestCASBoundsBodiesWhileReading checks that the xorb and shard handlers
// stop reading a body at the limit, whatever the Ingestor does and with no
// Content-Length to refuse up front.
func TestCASBoundsBodiesWhileReading(t *testing.T) {
	read := make(chan int64, 4)
	c := newCASWith(t, xet.HandlerOptions{CASURL: "https://hangar.example.com/xet-cas"},
		func(ing xet.Ingestor) xet.Ingestor { return greedy{ing} }, bodyMeter(read))
	const extra = 8 << 20
	cases := []struct {
		path  string
		limit int64
	}{
		{"/xet-cas/v1/xorbs/default/" + strings.Repeat("ab", 32), xet.MaxXorbSize},
		{"/xet-cas/v1/shards", xet.MaxShardSize},
		{"/xet-cas/v2/shards", xet.MaxShardSize},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			resp := c.postStream(t, tc.path, c.token, tc.limit+extra)
			if resp != nil {
				require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
			}
			require.LessOrEqual(t, waitRead(t, read), tc.limit+1, "the handler read past the limit")
		})
	}
}

// holdShards blocks the first AcceptShard for one Namespace until
// release is called; later calls return at once.
type holdShards struct {
	xet.Ingestor
	namespace string
	held      atomic.Bool
	entered   chan struct{}
	released  chan struct{}
	once      sync.Once
}

func (h *holdShards) release() { h.once.Do(func() { close(h.released) }) }

func (h *holdShards) AcceptShard(ctx context.Context, c *xet.Claims, r io.Reader, size int64, events func(xet.ShardEvent)) (*xet.ShardResult, error) {
	if _, err := io.Copy(io.Discard, r); err != nil {
		return nil, err
	}
	if c.Repo.Namespace == h.namespace && h.held.CompareAndSwap(false, true) {
		h.entered <- struct{}{}
		select {
		case <-h.released:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &xet.ShardResult{Result: 1}, nil
}

// newHeld returns a holdShards for the Namespace. Tests release it in a
// cleanup too, so a failing test does not leave a request open.
func newHeld(namespace string) *holdShards {
	return &holdShards{namespace: namespace, entered: make(chan struct{}, 4), released: make(chan struct{})}
}

// TestCASShardIngestsPerNamespace checks that a Namespace has at most
// ShardIngests shards in progress: another upload waits for a slot without
// its body being read, then gets 429, while other Namespaces carry on.
func TestCASShardIngestsPerNamespace(t *testing.T) {
	bob := model.Principal{Kind: model.PrincipalUser, UserID: alice().UserID, Username: "bob"}
	bobRepo := model.RepoID{Type: model.RepoTypeModel, Namespace: "bob", Name: "tiny"}
	body := strings.Repeat("s", 1<<20)

	t.Run("busy Namespace gets 429", func(t *testing.T) {
		held := newHeld("alice")
		read := make(chan int64, 8)
		c := newCASWith(t, xet.HandlerOptions{CASURL: "https://hangar.example.com/xet-cas", ShardIngests: 1, ShardQueueWait: 50 * time.Millisecond},
			func(ing xet.Ingestor) xet.Ingestor { held.Ingestor = ing; return held }, bodyMeter(read))
		t.Cleanup(held.release)
		bobTok, err := c.svc.IssueWriteToken(context.Background(), xet.TokenRequest{Principal: bob, Repo: bobRepo, Revision: "main"})
		require.NoError(t, err)

		first := make(chan int, 1)
		go func() {
			resp, err := http.DefaultClient.Do(c.request(t, "/xet-cas/v1/shards", c.token, body))
			if err != nil {
				first <- 0
				return
			}
			_ = resp.Body.Close()
			first <- resp.StatusCode
		}()
		<-held.entered

		for _, path := range []string{"/xet-cas/v1/shards", "/xet-cas/v2/shards"} {
			resp := c.do(t, http.MethodPost, path, c.token, []byte(body))
			require.Equal(t, http.StatusTooManyRequests, resp.StatusCode, path)
			require.Equal(t, "10", resp.Header.Get("Retry-After"))
			require.Contains(t, resp.Header.Get("X-Error-Message"), "Try again")
			require.Zero(t, waitRead(t, read), "a waiting upload's body was read")
		}

		// Another Namespace is not held up.
		resp := c.do(t, http.MethodPost, "/xet-cas/v1/shards", bobTok.AccessToken, []byte(body))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.EqualValues(t, len(body), waitRead(t, read))

		held.release()
		require.Equal(t, http.StatusOK, <-first)
		waitRead(t, read)
		// The slot is free again.
		resp = c.do(t, http.MethodPost, "/xet-cas/v1/shards", c.token, []byte(body))
		require.Equal(t, http.StatusOK, resp.StatusCode)
	})

	t.Run("waiting upload gets the freed slot", func(t *testing.T) {
		held := newHeld("alice")
		c := newCASWith(t, xet.HandlerOptions{CASURL: "https://hangar.example.com/xet-cas", ShardIngests: 1, ShardQueueWait: 10 * time.Second},
			func(ing xet.Ingestor) xet.Ingestor { held.Ingestor = ing; return held }, nil)
		t.Cleanup(held.release)
		statuses := make(chan int, 2)
		send := func() {
			resp, err := http.DefaultClient.Do(c.request(t, "/xet-cas/v1/shards", c.token, body))
			if err != nil {
				statuses <- 0
				return
			}
			_ = resp.Body.Close()
			statuses <- resp.StatusCode
		}
		go send()
		<-held.entered
		go send()
		select {
		case s := <-statuses:
			t.Fatalf("the second upload finished with %d while the first held the slot", s)
		case <-time.After(200 * time.Millisecond):
		}
		held.release()
		require.Equal(t, http.StatusOK, <-statuses)
		require.Equal(t, http.StatusOK, <-statuses)
	})
}

func (c *casEnv) request(t *testing.T, path, token, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, c.url+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

// TestCASTooLargeMessages checks 413 responses say which limit was hit.
func TestCASTooLargeMessages(t *testing.T) {
	c := newCAS(t, "https://hangar.example.com/xet-cas", 0, nil)
	sum := xettest.SHA256Hash([]byte("large"))
	f := xet.ShardFile{Hash: sum, Flags: xet.FileFlagVerification | xet.FileFlagMetadataExt, SHA256: &sum}
	for range 13 { // 13 × 4 GiB declared, over the default 50 GiB
		f.Terms = append(f.Terms, xet.Term{Xorb: sum, UnpackedBytes: math.MaxUint32, Start: 0, End: 1})
		f.Verification = append(f.Verification, xet.Hash{})
	}
	resp := c.do(t, http.MethodPost, "/xet-cas/v1/shards", c.token, xet.MarshalShard(&xet.Shard{Files: []xet.ShardFile{f}}))
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
	msg := resp.Header.Get("X-Error-Message")
	require.True(t, strings.HasPrefix(msg, "The upload is too large: file 0 "), msg)
	require.Contains(t, msg, "xet.max_file_size")
}
