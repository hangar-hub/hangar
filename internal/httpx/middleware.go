package httpx

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/telemetry"
)

// HeaderRequestID is echoed on every response; huggingface_hub includes it
// in error messages.
const HeaderRequestID = "X-Request-Id"

var requestIDRe = regexp.MustCompile(`^[A-Za-z0-9._\-]{1,128}$`)

// NewRequestID returns a random request ID.
func NewRequestID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// RequestIDs gives every request an ID, kept from a well-formed incoming
// X-Request-Id header or generated, and echoes it on the response.
func RequestIDs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if !requestIDRe.MatchString(id) {
			id = NewRequestID()
		}
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(WithInfo(r.Context(), id)))
	})
}

// Recover turns a panic into a 500 in the request's error format and logs
// the stack. http.ErrAbortHandler is re-raised so net/http can abort the
// connection as intended.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rw := wrap(w)
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity, as net/http does.
					panic(v)
				}
				log.ErrorContext(r.Context(), "panic serving request",
					"panic", fmt.Sprint(v), "stack", string(debug.Stack()),
					"method", r.Method, "route", routeOf(r))
				if !rw.wroteHeader {
					WriteError(rw, r, http.StatusInternalServerError, "", "Internal server error. The request ID is "+RequestID(r.Context())+".")
				}
			}()
			next.ServeHTTP(rw, r)
		})
	}
}

// AccessLog writes one info line per request with the method, route
// template, status, duration, bytes, user ID, protocol and Repo ID, and
// renames the current span to `{method} {route}`. It never logs the raw
// path, the query string or headers, which can carry secrets.
func AccessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := wrap(w)
			next.ServeHTTP(rw, r)
			info := Info(r.Context())
			route := routeOf(r)
			attrs := []any{
				"method", r.Method,
				"route", route,
				"status", rw.Status(),
				"duration_ms", time.Since(start).Milliseconds(),
				"bytes_in", r.ContentLength,
				"bytes_out", rw.bytes,
			}
			if info != nil {
				attrs = append(attrs, "protocol", string(info.Protocol()))
				if uid := info.Principal().EndUserID(); uid != "" {
					attrs = append(attrs, "user_id", uid)
				}
				if repo := info.Repo(); !repo.IsZero() {
					attrs = append(attrs, "repo_id", repo.String(), "repo_type", string(repo.Type))
				}
			}
			log.InfoContext(r.Context(), "request", attrs...)

			span := trace.SpanFromContext(r.Context())
			if span.IsRecording() {
				span.SetName(r.Method + " " + route)
				if info != nil {
					span.SetAttributes(telemetry.AttrProtocol.String(string(info.Protocol())))
					if uid := info.Principal().EndUserID(); uid != "" {
						span.SetAttributes(telemetry.AttrEndUserID.String(uid))
					}
					if repo := info.Repo(); !repo.IsZero() {
						span.SetAttributes(telemetry.AttrRepoID.String(repo.String()), telemetry.AttrRepoType.String(string(repo.Type)))
					}
				}
				span.SetAttributes(attribute.String("http.route", route))
			}
		})
	}
}

// routeOf returns the route template the handler recorded, or a
// placeholder that never contains the raw path.
func routeOf(r *http.Request) string {
	if info := Info(r.Context()); info != nil && info.Route() != "" {
		return info.Route()
	}
	return "unmatched"
}

// Authenticator turns request credentials into a Principal. internal/auth
// implements it. It returns an anonymous Principal and no error when the
// request has no credentials.
type Authenticator interface {
	Authenticate(r *http.Request) (model.Principal, error)
}

// ErrInvalidCredentials is what an Authenticator returns (wrapped) for a
// bad, expired or revoked credential.
var ErrInvalidCredentials = errors.New("invalid credentials")

// Authenticate calls authn for every request and stores the Principal. When
// authentication fails, onError writes the protocol's 401 response; nil
// uses WriteError, which picks the format from the request's protocol.
func Authenticate(authn Authenticator, onError func(w http.ResponseWriter, r *http.Request, err error)) func(http.Handler) http.Handler {
	if onError == nil {
		onError = func(w http.ResponseWriter, r *http.Request, err error) {
			WriteUnauthorized(w, r)
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := authn.Authenticate(r)
			if err != nil {
				onError(w, r, err)
				return
			}
			SetPrincipal(r.Context(), p)
			next.ServeHTTP(w, r)
		})
	}
}

// TrustedProxies rewrites r.RemoteAddr, r.URL.Scheme and r.Host from
// X-Forwarded-For, X-Forwarded-Proto and X-Forwarded-Host, but only when
// the direct peer is in one of the trusted ranges. The client address is the
// rightmost X-Forwarded-For entry that is not itself a trusted proxy.
func TrustedProxies(trusted []netip.Prefix) func(http.Handler) http.Handler {
	isTrusted := func(a netip.Addr) bool {
		a = a.Unmap()
		for _, p := range trusted {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			peer, ok := remoteAddr(r.RemoteAddr)
			if !ok || !isTrusted(peer) {
				next.ServeHTTP(w, r)
				return
			}
			r2 := r.Clone(r.Context())
			if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
				hops := strings.Split(strings.Join(xff, ","), ",")
				for i := len(hops) - 1; i >= 0; i-- {
					a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
					if err != nil {
						break
					}
					r2.RemoteAddr = netip.AddrPortFrom(a.Unmap(), 0).String()
					if !isTrusted(a) {
						break
					}
				}
			}
			if proto := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))); proto == "http" || proto == "https" {
				r2.URL.Scheme = proto
			}
			if host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); host != "" && validHost(host) {
				r2.Host = host
			}
			next.ServeHTTP(w, r2)
		})
	}
}

var hostRe = regexp.MustCompile(`^[A-Za-z0-9.\-]+(:\d{1,5})?$|^\[[0-9A-Fa-f:.]+\](:\d{1,5})?$`)

func validHost(h string) bool { return hostRe.MatchString(h) }

func remoteAddr(s string) (netip.Addr, bool) {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr(), true
	}
	host, _, err := net.SplitHostPort(s)
	if err != nil {
		host = s
	}
	a, err := netip.ParseAddr(host)
	return a, err == nil
}

// ClientIP returns the client address of a request after TrustedProxies.
func ClientIP(r *http.Request) string {
	if a, ok := remoteAddr(r.RemoteAddr); ok {
		return a.Unmap().String()
	}
	return r.RemoteAddr
}

// IsHTTPS reports whether the client used HTTPS, directly or through a
// trusted proxy. Server requests have an empty URL scheme unless
// TrustedProxies set it from a trusted X-Forwarded-Proto.
func IsHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.URL.Scheme == "https"
}

// ErrBodyTooLarge is returned by reads past the MaxBody limit.
var ErrBodyTooLarge = errors.New("request body too large")

// MaxBody limits the request body of buffered endpoints. Streaming
// endpoints (uploads, git, Xet) must not use it.
func MaxBody(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				WriteError(w, r, http.StatusRequestEntityTooLarge, "", fmt.Sprintf("Request body is larger than the %d-byte limit.", limit))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// ResponseWriter records the status and byte count, and passes through
// Flush, Hijack and ReadFrom so streaming and git keep working.
type ResponseWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func wrap(w http.ResponseWriter) *ResponseWriter {
	if rw, ok := w.(*ResponseWriter); ok {
		return rw
	}
	return &ResponseWriter{ResponseWriter: w}
}

// Status returns the response status, 200 if the handler wrote a body
// without calling WriteHeader.
func (w *ResponseWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// WriteHeader implements http.ResponseWriter.
func (w *ResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = code >= 200 || code == http.StatusSwitchingProtocols
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write implements http.ResponseWriter.
func (w *ResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// ReadFrom implements io.ReaderFrom so io.Copy can use sendfile.
func (w *ResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := io.Copy(w.ResponseWriter, r)
	w.bytes += n
	return n, err
}

// Flush implements http.Flusher.
func (w *ResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		if !w.wroteHeader {
			w.WriteHeader(http.StatusOK)
		}
		f.Flush()
	}
}

// Hijack implements http.Hijacker.
func (w *ResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijacking not supported")
	}
	return h.Hijack()
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *ResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
