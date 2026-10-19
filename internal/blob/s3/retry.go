package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/minio/minio-go/v7"

	"github.com/hangar-hub/hangar/internal/blob"
)

// Defaults for uploads over slow or unreliable links, such as a home
// connection to a cloud bucket.
const (
	// DefaultRetries is how many times a failed upload request is sent
	// again.
	DefaultRetries = 5
	// DefaultStallTimeout is how long an upload request may make no
	// progress: send no body bytes, or wait for a response after the body.
	DefaultStallTimeout = 2 * time.Minute
	// defaultRetryBackoff is the first wait before a retry. Each wait
	// doubles, up to maxRetryBackoff, with full jitter.
	defaultRetryBackoff = time.Second
	maxRetryBackoff     = 30 * time.Second
)

// errStalled is the cause of an upload attempt cancelled for making no
// progress.
var errStalled = errors.New("no progress")

// upload sends one upload request, a part or a single PUT, with retries.
// send makes one attempt with body; body replays the same bytes on every
// attempt and fails the upload, without a retry, if its source does.
// Requests are retried when the connection fails, stalls for longer than
// the stall timeout, or S3 answers with a server error, throttling or a
// request timeout. Every upload request Hangar makes is idempotent: a part
// or object PUT sent again replaces the first.
func (s *Store) upload(ctx context.Context, body *replayBody, send func(ctx context.Context, body io.Reader) error) error {
	for attempt := 0; ; attempt++ {
		r, rerr := body.rewind()
		if rerr != nil {
			return rerr
		}
		err := s.attempt(ctx, r, send)
		if err == nil {
			return nil
		}
		if berr := body.err(); berr != nil {
			return berr
		}
		if !retryable(ctx, err) {
			return err
		}
		if attempt >= s.retries {
			if attempt > 0 {
				return fmt.Errorf("s3 blob store: gave up after %d attempts: %w", attempt+1, err)
			}
			return err
		}
		wait := backoff(s.retryBackoff, attempt)
		// The reason is a class, not the error text: that names the
		// request URL, which must stay out of logs.
		slog.WarnContext(ctx, "s3 blob store: retrying an upload request",
			"attempt", attempt+1, "of", s.retries+1, "wait", wait.String(), "reason", retryReason(err))
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return errors.Join(err, ctx.Err())
		case <-t.C:
		}
	}
}

// attempt makes one request, cancelling it if it goes for the stall
// timeout without sending a body byte or finishing.
func (s *Store) attempt(ctx context.Context, r *progressReader, send func(ctx context.Context, body io.Reader) error) error {
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	r.touch()
	w := newStallWatch(s.stallTimeout, r, func() { cancel(errStalled) })
	defer w.stop()
	err := send(actx, r)
	if err != nil && ctx.Err() == nil && errors.Is(context.Cause(actx), errStalled) {
		return fmt.Errorf("s3 blob store: upload made no progress for %s: %w", s.stallTimeout, errStalled)
	}
	return err
}

// backoff returns the wait before retry n (0-based): a random duration
// between half and all of base doubled n times, capped at maxRetryBackoff.
func backoff(base time.Duration, n int) time.Duration {
	ceiling := min(base<<min(n, 16), maxRetryBackoff)
	if ceiling <= 0 {
		return 0
	}
	return ceiling/2 + rand.N(ceiling/2+1) //nolint:gosec // Jitter, not a secret.
}

// retryableCodes are S3 error codes worth sending a request again for.
var retryableCodes = map[string]bool{
	"RequestTimeout": true, "InternalError": true, "ServiceUnavailable": true, "SlowDown": true,
	"SlowDownWrite": true, "Throttling": true, "ThrottlingException": true, "RequestLimitExceeded": true,
	"RequestThrottled": true, "OperationAborted": true,
	// Temporary credentials that expired are fetched again on the next
	// request.
	"ExpiredToken": true, "ExpiredTokenException": true,
}

// retryable reports whether an upload request that failed with err should
// be sent again: a connection that failed or stalled, or a response that
// says to try later. S3 answers that describe the request itself, such as
// a missing upload, a denied request or a bad part, are final.
func retryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if errors.Is(err, errStalled) {
		return true
	}
	if errors.Is(err, blob.ErrSizeMismatch) || errors.Is(err, context.Canceled) {
		return false
	}
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		if retryableCodes[resp.Code] {
			return true
		}
		switch resp.StatusCode {
		case http.StatusRequestTimeout, http.StatusTooManyRequests,
			http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	// Anything else is the transport: a reset or closed connection, a
	// response header timeout, a failed dial or TLS handshake.
	return true
}

// errNoETag is a part upload that S3 answered with success but no ETag. S3
// always sends one, and the part cannot be completed without it, so the
// answer is treated as a failed attempt and retried.
var errNoETag = errors.New("s3 blob store: the part upload's response has no ETag")

// errAttemptOver is returned to a transport that reads an attempt's body
// after the attempt has ended. net/http can go on reading a request body
// after RoundTrip returns, for example when the server answers early, and
// those reads must not take bytes meant for the next attempt.
var errAttemptOver = errors.New("s3 blob store: the upload attempt is over")

// retryReason names the kind of failure for the retry log.
func retryReason(err error) string {
	if errors.Is(err, errStalled) {
		return "stalled"
	}
	if errors.Is(err, errNoETag) {
		return "no-etag"
	}
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		if resp.Code != "" {
			return "s3." + resp.Code
		}
		return "http." + strconv.Itoa(resp.StatusCode)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "connection"
}

// replayBody is an upload body that can be sent more than once. A seekable
// source is rewound for each attempt. Any other source is copied to a
// spool file as it is first read, so a retry sends the bytes already read
// from the file and then carries on reading the source. The source is read
// once, in order, whatever the number of attempts, and at most one part
// is spooled, on disk rather than in memory, so uploads still stream.
type replayBody struct {
	ctx  context.Context
	src  io.Reader
	size int64
	dir  string

	// mu serialises reads, so a transport still reading a finished
	// attempt's body cannot race the next attempt.
	mu sync.Mutex
	// seeker and start rewind a seekable source.
	seeker io.Seeker
	start  int64
	// read counts bytes taken from the source; spool holds them when the
	// source cannot be rewound.
	read     int64
	spool    *os.File
	spoolErr error
	srcErr   error
	srcEOF   bool
	replay   bool
	current  *progressReader
}

// newReplayBody returns a body of exactly size bytes from r. With replay
// false the body is sent once and nothing is spooled. Spool files go in
// dir, or the system temporary directory when dir is empty.
func newReplayBody(ctx context.Context, r io.Reader, size int64, dir string, replay bool) *replayBody {
	b := &replayBody{ctx: ctx, src: r, size: size, dir: dir, replay: replay}
	if !replay {
		return b
	}
	if sk, ok := r.(io.ReadSeeker); ok {
		if pos, err := sk.Seek(0, io.SeekCurrent); err == nil {
			b.seeker, b.start = sk, pos
		}
	}
	return b
}

// rewind ends the current attempt and returns a reader for the next one,
// from the first byte.
func (b *replayBody) rewind() (*progressReader, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.current != nil {
		b.current.over = true
		if !b.replay {
			return nil, errors.New("s3 blob store: the upload body cannot be sent again")
		}
		if b.seeker != nil {
			if _, err := b.seeker.Seek(b.start, io.SeekStart); err != nil {
				return nil, fmt.Errorf("s3 blob store: rewind the upload body: %w", err)
			}
			b.read, b.srcEOF = 0, false
		}
	}
	b.current = &progressReader{body: b}
	return b.current, nil
}

// err returns the error that makes another attempt pointless: the source
// failed, ended early, or the spool file could not be used.
func (b *replayBody) err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case b.srcErr != nil:
		return b.srcErr
	case b.spoolErr != nil:
		return b.spoolErr
	case b.srcEOF && b.read < b.size:
		return fmt.Errorf("%w: read %d bytes, want %d", blob.ErrSizeMismatch, b.read, b.size)
	}
	return nil
}

// sourceRead reports how many bytes the last attempt took from the source.
func (b *replayBody) sourceRead() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.read
}

// close ends the last attempt and removes the spool file. Call it before
// reading the source again.
func (b *replayBody) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.current != nil {
		b.current.over = true
	}
	if b.spool != nil {
		name := b.spool.Name()
		_ = b.spool.Close()
		_ = os.Remove(name)
		b.spool = nil
	}
}

// readAt fills p from offset off of the body. The caller holds mu.
func (b *replayBody) readAt(p []byte, off int64) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	if off >= b.size {
		return 0, io.EOF
	}
	p = p[:min(int64(len(p)), b.size-off)]
	if b.seeker == nil && off < b.read {
		// Replay from the spool what an earlier attempt already read.
		p = p[:min(int64(len(p)), b.read-off)]
		n, err := b.spool.ReadAt(p, off)
		if n < len(p) {
			b.spoolErr = fmt.Errorf("s3 blob store: read the upload spool file: %w", err)
			return n, b.spoolErr
		}
		return n, nil
	}
	if b.srcEOF {
		return 0, io.EOF
	}
	n, err := b.src.Read(p)
	if n > 0 {
		if b.replay && b.seeker == nil {
			if werr := b.spoolWrite(p[:n]); werr != nil {
				return 0, werr
			}
		}
		b.read += int64(n)
	}
	switch {
	case errors.Is(err, io.EOF):
		b.srcEOF = true
		if n > 0 {
			return n, nil
		}
		return 0, io.EOF
	case err != nil:
		if b.ctx.Err() == nil {
			b.srcErr = err
		}
		return n, err
	}
	return n, nil
}

func (b *replayBody) spoolWrite(p []byte) error {
	if b.spool == nil {
		f, err := os.CreateTemp(b.dir, "hangar-s3-part-*")
		if err != nil {
			b.spoolErr = fmt.Errorf("s3 blob store: create the upload spool file: %w", err)
			return b.spoolErr
		}
		b.spool = f
	}
	if _, err := b.spool.Write(p); err != nil {
		b.spoolErr = fmt.Errorf("s3 blob store: write the upload spool file: %w", err)
		return b.spoolErr
	}
	return nil
}

// progressReader is one attempt's view of a replayBody. It records when it
// last made progress, for the stall watch. It deliberately has no Seek
// method: minio-go retries seekable bodies itself, with short fixed
// backoffs and no stall detection, and upload does that instead.
type progressReader struct {
	body *replayBody
	off  int64
	// over is set, under body.mu, when the attempt has ended.
	over bool
	last atomic.Int64
}

func (r *progressReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	r.body.mu.Lock()
	defer r.body.mu.Unlock()
	if r.over {
		return 0, errAttemptOver
	}
	n, err := r.body.readAt(p, r.off)
	r.off += int64(n)
	r.touch()
	return n, err
}

func (r *progressReader) touch() { r.last.Store(time.Now().UnixNano()) }

// idle returns how long the attempt has gone without reading body bytes.
func (r *progressReader) idle() time.Duration {
	return time.Since(time.Unix(0, r.last.Load()))
}

// stallWatch calls fire once if its reader makes no progress for timeout.
// Progress is reading body bytes; once the body is sent, the wait for the
// response counts against the same timeout, so an attempt is cancelled
// when it has neither sent a byte nor finished for that long.
type stallWatch struct {
	mu      sync.Mutex
	timer   *time.Timer
	stopped bool
}

func newStallWatch(timeout time.Duration, r *progressReader, fire func()) *stallWatch {
	w := &stallWatch{}
	check := func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.stopped {
			return
		}
		if idle := r.idle(); idle < timeout {
			w.timer.Reset(timeout - idle)
			return
		}
		w.stopped = true
		fire()
	}
	w.mu.Lock()
	w.timer = time.AfterFunc(timeout, check)
	w.mu.Unlock()
	return w
}

func (w *stallWatch) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	w.timer.Stop()
}
