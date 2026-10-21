package httpx_test

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/httpx"
)

func idleServer(t *testing.T, d time.Duration, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(httpx.IdleTimeout(d)(h))
	t.Cleanup(srv.Close)
	return srv
}

func dial(t *testing.T, srv *httptest.Server) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func waitErr(t *testing.T, errc <-chan error) error {
	t.Helper()
	select {
	case err := <-errc:
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("the handler was not cut")
		return nil
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.As(err, &ne) && ne.Timeout()
}

func TestIdleTimeoutCutsStalledUpload(t *testing.T) {
	errc := make(chan error, 1)
	srv := idleServer(t, 200*time.Millisecond, func(_ http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		errc <- err
	})
	conn := dial(t, srv)
	_, err := fmt.Fprint(conn, "PUT /x HTTP/1.1\r\nHost: x\r\nContent-Length: 1000\r\n\r\n0123456789")
	require.NoError(t, err)
	err = waitErr(t, errc)
	require.Error(t, err)
	require.True(t, isTimeout(err), err)
}

func TestIdleTimeoutKeepsSlowUploadsAndLongHandlers(t *testing.T) {
	const idle = 200 * time.Millisecond
	type result struct {
		body   string
		ctxErr error
	}
	results := make(chan result, 3)
	srv := idleServer(t, idle, func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			results <- result{ctxErr: err}
			return
		}
		// Work for longer than the idle timeout without reading or
		// writing: the request must not be cancelled.
		time.Sleep(3 * idle)
		results <- result{body: string(b), ctxErr: r.Context().Err()}
		_, _ = io.WriteString(w, "ok")
	})

	// A body sent a byte at a time, for longer than the timeout in all.
	conn := dial(t, srv)
	_, err := fmt.Fprint(conn, "PUT /x HTTP/1.1\r\nHost: x\r\nContent-Length: 12\r\n\r\n")
	require.NoError(t, err)
	for _, c := range "hello, world" {
		time.Sleep(idle / 4)
		_, err := conn.Write([]byte(string(c)))
		require.NoError(t, err)
	}
	res := <-results
	require.NoError(t, res.ctxErr)
	require.Equal(t, "hello, world", res.body)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "ok", string(b))

	// A request without a body, then another on the same keep-alive
	// connection after more than the timeout.
	client := srv.Client()
	for range 2 {
		resp, err := client.Get(srv.URL)
		require.NoError(t, err)
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, "ok", string(b))
		res := <-results
		require.NoError(t, res.ctxErr)
		time.Sleep(2 * idle)
	}
}

func TestIdleTimeoutCutsStalledDownload(t *testing.T) {
	// A sparse file larger than the socket buffers, sent with sendfile
	// through ReadFrom, and the same bytes sent with Write.
	path := filepath.Join(t.TempDir(), "big")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(64<<20))
	require.NoError(t, f.Close())

	for _, mode := range []string{"readfrom", "write"} {
		t.Run(mode, func(t *testing.T) {
			errc := make(chan error, 1)
			srv := idleServer(t, 200*time.Millisecond, func(w http.ResponseWriter, _ *http.Request) {
				f, err := os.Open(path)
				if err != nil {
					errc <- err
					return
				}
				defer f.Close()
				w.Header().Set("Content-Length", fmt.Sprint(64<<20))
				if mode == "readfrom" {
					_, err = io.Copy(w, f)
				} else {
					_, err = io.Copy(struct{ io.Writer }{w}, f)
				}
				errc <- err
			})
			conn := dial(t, srv)
			_, err := fmt.Fprint(conn, "GET /x HTTP/1.1\r\nHost: x\r\n\r\n")
			require.NoError(t, err)
			// The client reads nothing.
			err = waitErr(t, errc)
			require.Error(t, err)
		})
	}
}

func TestIdleTimeoutDeliversWholeDownloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big")
	content := strings.Repeat("0123456789abcdef", (5<<20)/16+3)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	srv := idleServer(t, time.Second, func(w http.ResponseWriter, _ *http.Request) {
		f, err := os.Open(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		_, _ = io.Copy(w, f)
	})
	resp, err := srv.Client().Get(srv.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	// Read steadily but slowly: 64 KiB every 10 ms.
	var got strings.Builder
	buf := make([]byte, 64<<10)
	for {
		n, err := io.ReadFull(resp.Body, buf)
		got.Write(buf[:n])
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		require.NoError(t, err)
		time.Sleep(10 * time.Millisecond)
	}
	require.Equal(t, len(content), got.Len())
	require.Equal(t, content, got.String())
}
