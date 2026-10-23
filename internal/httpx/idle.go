package httpx

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"time"
)

// idleChunk is the most a single write may send under one write deadline.
// A response therefore keeps going while the client takes at least
// idleChunk per idle timeout (about 3.5 KB/s for 1 MiB in 5 minutes).
const idleChunk = 1 << 20

// IdleTimeout cuts a request whose client stops sending its body, or stops
// reading the response, for longer than d (02-architecture.md, "Errors and
// limits": streaming endpoints have no total timeout but an idle timeout).
//
// The connection's read deadline is pushed to now+d before each read of
// the request body, and its write deadline before each write of the
// response, so a transfer of any size or length keeps going as long as
// data flows. Time the handler spends between reads or writes does not
// count. The read deadline is never set outside a body read: net/http's
// background read, which notices a client that goes away, would take its
// expiry for a closed connection and cancel the request.
func IdleTimeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc := http.NewResponseController(w)
			iw := &idleWriter{ResponseWriter: w, rc: rc, d: d}
			if r.Body != nil && r.Body != http.NoBody {
				r.Body = &idleBody{ReadCloser: r.Body, rc: rc, d: d}
			}
			defer func() {
				// Bound the flush net/http makes after the handler; it
				// clears the write deadline once the response is done.
				if !iw.hijacked {
					_ = rc.SetWriteDeadline(time.Now().Add(d))
				}
			}()
			next.ServeHTTP(iw, r)
		})
	}
}

// BodyTimeout gives the whole request body of a buffered endpoint at most
// d, on top of IdleTimeout's limit between reads, so a client that sends a
// small body a byte at a time cannot hold a goroutine for longer.
// Streaming endpoints (uploads, downloads, git, Xet) must not use it.
func BodyTimeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil && r.Body != http.NoBody {
				until := time.Now().Add(d)
				if b, ok := r.Body.(*idleBody); ok {
					b.until = until
				} else {
					r.Body = &idleBody{ReadCloser: r.Body, rc: http.NewResponseController(w), until: until}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// idleBody pushes the read deadline back before each read of the body,
// to d from now, but never past until when BodyTimeout set it.
type idleBody struct {
	io.ReadCloser
	rc    *http.ResponseController
	d     time.Duration
	until time.Time
}

func (b *idleBody) Read(p []byte) (int, error) {
	var deadline time.Time
	if b.d > 0 {
		deadline = time.Now().Add(b.d)
	}
	if !b.until.IsZero() && (deadline.IsZero() || b.until.Before(deadline)) {
		deadline = b.until
	}
	_ = b.rc.SetReadDeadline(deadline)
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		// The body is done; net/http's background read takes over the
		// connection and must not inherit a deadline.
		_ = b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

// idleWriter pushes the write deadline back before each write or flush,
// and passes Flush, Hijack and ReadFrom through.
type idleWriter struct {
	http.ResponseWriter
	rc       *http.ResponseController
	d        time.Duration
	last     time.Time
	hijacked bool
}

// extend pushes the write deadline back, at most once a second.
func (w *idleWriter) extend() {
	now := time.Now()
	if now.Sub(w.last) < time.Second {
		return
	}
	w.last = now
	_ = w.rc.SetWriteDeadline(now.Add(w.d))
}

// Write implements http.ResponseWriter, in chunks of idleChunk.
func (w *idleWriter) Write(p []byte) (int, error) {
	var total int
	for {
		w.extend()
		chunk := p
		if len(chunk) > idleChunk {
			chunk = chunk[:idleChunk]
		}
		n, err := w.ResponseWriter.Write(chunk)
		total += n
		p = p[n:]
		if err != nil || len(p) == 0 {
			return total, err
		}
	}
}

// ReadFrom implements io.ReaderFrom in chunks of idleChunk. Each chunk is
// an *io.LimitedReader, so a file still goes out with sendfile.
func (w *idleWriter) ReadFrom(src io.Reader) (int64, error) {
	rf, ok := w.ResponseWriter.(io.ReaderFrom)
	if !ok {
		return io.Copy(writerOnly{w}, src)
	}
	var total int64
	for {
		w.extend()
		n, err := rf.ReadFrom(&io.LimitedReader{R: src, N: idleChunk})
		total += n
		if err != nil || n < idleChunk {
			return total, err
		}
	}
}

// writerOnly hides ReadFrom, so io.Copy does not call it again.
type writerOnly struct{ io.Writer }

// Flush implements http.Flusher.
func (w *idleWriter) Flush() {
	w.extend()
	_ = w.rc.Flush()
}

// Hijack implements http.Hijacker. The caller owns the connection and its
// deadlines from then on.
func (w *idleWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, brw, err := w.rc.Hijack()
	if err == nil {
		w.hijacked = true
		_ = conn.SetWriteDeadline(time.Time{})
	}
	return conn, brw, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *idleWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
