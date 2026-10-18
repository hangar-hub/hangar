package gitserver

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/scan"
	"github.com/hangar-hub/hangar/internal/telemetry"
)

// git smart HTTP services.
const (
	serviceUploadPack  = "git-upload-pack"
	serviceReceivePack = "git-receive-pack"
)

// serveGit routes git smart HTTP requests: `info/refs?service=...`,
// `git-upload-pack` and `git-receive-pack`, with or without `.git`.
func (s *Server) serveGit(w http.ResponseWriter, r *http.Request) {
	rp, err := parseRepoPath(r.URL.EscapedPath())
	if err != nil {
		httpx.SetRoute(r.Context(), "/{git}")
		s.gitError(w, r, http.StatusNotFound, "Not found.")
		return
	}
	prefix := routePrefix(rp.ID)
	switch {
	case len(rp.Rest) == 2 && rp.Rest[0] == "info" && rp.Rest[1] == "refs":
		httpx.SetRoute(r.Context(), prefix+"/info/refs")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			s.methodNotAllowed(w, r, "GET, HEAD")
			return
		}
		s.infoRefs(w, r, rp)
	case len(rp.Rest) == 1 && (rp.Rest[0] == serviceUploadPack || rp.Rest[0] == serviceReceivePack):
		httpx.SetRoute(r.Context(), prefix+"/"+rp.Rest[0])
		if r.Method != http.MethodPost {
			s.methodNotAllowed(w, r, "POST")
			return
		}
		if rp.Rest[0] == serviceUploadPack {
			s.uploadPack(w, r, rp)
		} else {
			s.receivePack(w, r, rp)
		}
	default:
		// The dumb HTTP protocol (objects/..., HEAD) is not served.
		httpx.SetRoute(r.Context(), prefix+".git/{path}")
		s.gitError(w, r, http.StatusNotFound, "Not found. Hangar serves git over the smart HTTP protocol only.")
	}
}

func (s *Server) methodNotAllowed(w http.ResponseWriter, r *http.Request, allow string) {
	w.Header().Set("Allow", allow)
	s.gitError(w, r, http.StatusMethodNotAllowed, "Method not allowed.")
}

// gitError writes a plain-text error, which git shows to the user as
// `remote: <message>`. A 401 carries a Basic challenge so git asks the
// credential helper for a token.
func (s *Server) gitError(w http.ResponseWriter, r *http.Request, status int, message string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="`+Realm+`"`)
	}
	noCache(w.Header())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, message+"\n")
	}
}

// gitFailure answers a lookup or internal error.
func (s *Server) gitFailure(w http.ResponseWriter, r *http.Request, err error) {
	var ae *accessError
	if errors.As(err, &ae) {
		s.gitError(w, r, ae.status, ae.message)
		return
	}
	if httpx.ClientGone(r, err) {
		s.log.DebugContext(r.Context(), "client closed the request", "error", err.Error())
		s.gitError(w, r, httpx.StatusClientClosedRequest, httpx.ClientClosedMessage)
		return
	}
	s.log.ErrorContext(r.Context(), "git request failed", "error", err)
	s.gitError(w, r, http.StatusInternalServerError, "Internal server error. The request ID is "+httpx.RequestID(r.Context())+".")
}

func noCache(h http.Header) {
	h.Set("Expires", "Fri, 01 Jan 1980 00:00:00 GMT")
	h.Set("Pragma", "no-cache")
	h.Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
}

// gitProtocol returns the Git-Protocol header if it is well formed, as git
// http-backend does: only `key[=value]` items separated by colons.
func gitProtocol(r *http.Request) string {
	v := r.Header.Get("Git-Protocol")
	if v == "" || len(v) > 256 {
		return ""
	}
	for _, c := range v {
		if c < 0x20 || c > 0x7e {
			return ""
		}
	}
	return v
}

func isV2(proto string) bool {
	for _, item := range strings.Split(proto, ":") {
		if item == "version=2" {
			return true
		}
	}
	return false
}

// infoRefs answers the ref advertisement of either service.
func (s *Server) infoRefs(w http.ResponseWriter, r *http.Request, rp repoPath) {
	service := r.URL.Query().Get("service")
	switch service {
	case serviceUploadPack:
		rec, err := s.lookup(r.Context(), rp.ID, auth.ActionReadFiles)
		if err != nil {
			s.gitFailure(w, r, err)
			return
		}
		s.advertiseUploadPack(w, r, rec)
	case serviceReceivePack:
		rec, err := s.lookup(r.Context(), rp.ID, auth.ActionWrite)
		if err != nil {
			s.gitFailure(w, r, err)
			return
		}
		s.advertiseReceivePack(w, r, rec)
	default:
		s.gitError(w, r, http.StatusForbidden, "Hangar supports the smart HTTP protocol only. Use git 2.39 or later.")
	}
}

// requestBody returns the request body, decompressing gzip bodies.
func requestBody(r *http.Request) (io.ReadCloser, error) {
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))) {
	case "", "identity":
		return r.Body, nil
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		return zr, nil
	default:
		return nil, errors.New("unsupported Content-Encoding")
	}
}

// checkRequestType checks the Content-Type of a service POST, as git
// http-backend does.
func checkRequestType(r *http.Request, service string) bool {
	ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	return strings.EqualFold(strings.TrimSpace(ct), "application/x-"+service+"-request")
}

// flushWriter flushes the response after every write, so git sees progress
// and pack data as soon as they are produced.
type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func newFlushWriter(w http.ResponseWriter) *flushWriter {
	return &flushWriter{w: w, rc: http.NewResponseController(w)}
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		_ = f.rc.Flush()
	}
	return n, err
}

// pktLine encodes one pkt-line.
func pktLine(s string) []byte { return fmt.Appendf(nil, "%04x%s", len(s)+4, s) }

// advertiseUploadPack runs `git upload-pack --advertise-refs` on a view.
func (s *Server) advertiseUploadPack(w http.ResponseWriter, r *http.Request, rec *repo.Repo) {
	ctx, span := s.deps.Tracer.Start(r.Context(), "git.advertise_refs")
	span.SetAttributes(attribute.String("git.service", serviceUploadPack))
	defer span.End()
	view, err := s.deps.Repos.OpenView(ctx, rec.ID)
	if err != nil {
		span.SetStatus(codes.Error, "open view")
		s.gitFailure(w, r, fmt.Errorf("open view: %w", err))
		return
	}
	defer closeView(s, ctx, view)
	proto := gitProtocol(r)
	var out bytes.Buffer
	if err := s.runGit(ctx, view.Dir, proto, nil, &out, "--advertise-refs"); err != nil {
		span.SetStatus(codes.Error, "upload-pack")
		s.gitFailure(w, r, err)
		return
	}
	h := w.Header()
	noCache(h)
	h.Set("Content-Type", "application/x-git-upload-pack-advertisement")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	// git http-backend omits the smart reply for protocol v2.
	if !isV2(proto) {
		_, _ = w.Write(pktLine("# service=" + serviceUploadPack + "\n"))
		_, _ = w.Write([]byte("0000"))
	}
	_, _ = w.Write(out.Bytes())
}

// uploadPack streams a fetch or clone through `git upload-pack --stateless-rpc`.
func (s *Server) uploadPack(w http.ResponseWriter, r *http.Request, rp repoPath) {
	rec, err := s.lookup(r.Context(), rp.ID, auth.ActionReadFiles)
	if err != nil {
		s.gitFailure(w, r, err)
		return
	}
	if !checkRequestType(r, serviceUploadPack) {
		s.gitError(w, r, http.StatusUnsupportedMediaType, "Expected Content-Type application/x-git-upload-pack-request.")
		return
	}
	body, err := requestBody(r)
	if err != nil {
		s.gitError(w, r, http.StatusBadRequest, "The request body could not be decoded.")
		return
	}
	defer body.Close()
	in, fetch := fetchRequest(body)
	if fetch {
		if err := s.deps.Downloads.CheckGit(r.Context(), rec.UUID, rec.ID); scan.IsBlocked(err) {
			s.gitError(w, r, http.StatusForbidden, err.Error()+
				" git clone and fetch send every file in the Repo's history, so they are refused. Download files with huggingface_hub or the web UI instead.")
			return
		} else if err != nil {
			s.gitFailure(w, r, fmt.Errorf("check unsafe downloads: %w", err))
			return
		}
	}

	ctx, span := s.deps.Tracer.Start(r.Context(), "git.upload_pack")
	defer span.End()
	start := time.Now()
	view, err := s.deps.Repos.OpenView(ctx, rec.ID)
	if err != nil {
		span.SetStatus(codes.Error, "open view")
		s.gitFailure(w, r, fmt.Errorf("open view: %w", err))
		return
	}
	defer closeView(s, ctx, view)

	h := w.Header()
	noCache(h)
	h.Set("Content-Type", "application/x-git-upload-pack-result")
	out := &lazyHeaderWriter{w: w}
	err = s.runGit(ctx, view.Dir, gitProtocol(r), in, out, "--stateless-rpc")
	s.recordPackDuration(ctx, "upload-pack", start)
	if err != nil {
		span.SetStatus(codes.Error, "upload-pack")
		if !out.started && ctx.Err() == nil {
			s.gitFailure(w, r, err)
			return
		}
		// The response has started, or the client went away: the pack
		// stream is cut short and git reports the error itself.
		s.log.WarnContext(ctx, "git upload-pack ended early", "error", err)
	}
}

// fetchRequest reports whether an upload-pack request may send a pack: any
// protocol v0 or v1 request, and a protocol v2 request whose command is
// fetch. A v2 ls-refs request only lists refs. It returns a reader of the
// whole body.
func fetchRequest(body io.Reader) (io.Reader, bool) {
	br := bufio.NewReader(body)
	head, _ := br.Peek(4 + len("command=ls-refs"))
	if len(head) >= 4 {
		if cmd, ok := bytes.CutPrefix(head[4:], []byte("command=")); ok && bytes.HasPrefix(cmd, []byte("ls-refs")) {
			return br, false
		}
	}
	return br, true
}

// lazyHeaderWriter writes the 200 status on the first byte, so an error
// before any output can still become an error response.
type lazyHeaderWriter struct {
	w       http.ResponseWriter
	fw      *flushWriter
	started bool
}

func (l *lazyHeaderWriter) Write(p []byte) (int, error) {
	if !l.started {
		l.started = true
		l.w.WriteHeader(http.StatusOK)
		l.fw = newFlushWriter(l.w)
	}
	return l.fw.Write(p)
}

func closeView(s *Server, ctx context.Context, v *repo.GitView) {
	if err := v.Close(); err != nil {
		s.log.WarnContext(ctx, "close git view", "error", err)
	}
}

func (s *Server) recordPackDuration(ctx context.Context, service string, start time.Time) {
	if s.deps.Instruments == nil {
		return
	}
	s.deps.Instruments.GitPackDuration.Record(ctx, time.Since(start).Seconds(),
		metric.WithAttributes(attribute.String(telemetry.MetricService, service)))
}

// gitEnv is the minimal environment of git subprocesses: no system or
// global configuration, an empty HOME, and the client's protocol version.
func (s *Server) gitEnv(proto string) []string {
	env := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"HOME=" + s.home,
		"PATH=" + os.Getenv("PATH"),
		"LANG=C",
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
	}
	if proto != "" {
		env = append(env, "GIT_PROTOCOL="+proto)
	}
	return env
}

// maxStderr bounds how much of a subprocess's stderr is kept for errors.
const maxStderr = 8 << 10

// limitedBuffer keeps the first maxStderr bytes written to it.
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := maxStderr - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// runGit runs `git upload-pack <args> <dir>` with the configured limits. The
// process is killed when ctx ends, which includes the client disconnecting.
func (s *Server) runGit(ctx context.Context, dir, proto string, stdin io.Reader, stdout io.Writer, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	defer cancel()
	full := []string{
		"-c", "uploadpack.allowFilter=true",
		"-c", "uploadpack.allowAnySHA1InWant=false",
		"-c", "uploadpack.allowReachableSHA1InWant=true",
		// refs/hangar/* holds internal refs such as previous Mirror tips.
		"-c", "uploadpack.hideRefs=refs/hangar/",
		"upload-pack",
	}
	full = append(full, args...)
	full = append(full, dir)
	cmd := exec.CommandContext(ctx, s.cfg.GitBinary, full...) //nolint:gosec // fixed arguments; dir comes from repo.Service.
	cmd.Env = s.gitEnv(proto)
	cmd.Dir = dir
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start git upload-pack: %w", err)
	}
	if err := limitCPU(cmd.Process.Pid, s.cfg.Timeout); err != nil {
		s.log.WarnContext(ctx, "could not set the CPU limit of git upload-pack", "error", err)
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("git upload-pack: %w", ctx.Err())
		}
		return fmt.Errorf("git upload-pack: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
