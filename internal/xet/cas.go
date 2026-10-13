package xet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/telemetry"
)

// DefaultHeartbeat is how often /v2/shards repeats its last frame while it
// works, as the Hub's CAS does (about every 20 seconds), so proxies with
// idle timeouts keep the connection open.
const DefaultHeartbeat = 20 * time.Second

// maxTelemetryBody bounds the discarded telemetry body.
const maxTelemetryBody = 1 << 20

// HandlerOptions configure the CAS handler.
type HandlerOptions struct {
	// CASURL is the casUrl clients get. Its path is the prefix of the CAS
	// routes: /xet-cas by default, or none when CAS has its own hostname.
	CASURL string
	// Heartbeat is the /v2/shards heartbeat interval. Zero means DefaultHeartbeat.
	Heartbeat time.Duration
	// Log receives errors; nil discards them.
	Log *slog.Logger
}

// Handler serves the Xet CAS upload endpoints (03-hf-api.md "Xet"):
//
//	GET  {prefix}/v1/chunks/{prefix}/{hash}  always 404 (no global deduplication)
//	POST {prefix}/v1/xorbs/{prefix}/{hash}   {"was_inserted": bool}
//	POST {prefix}/v1/shards                  {"result": 1}
//	POST {prefix}/v2/shards                  NDJSON progress with heartbeats
//	POST /v1/telemetry                       200, body discarded
//
// It authenticates every call with the CAS token itself, so it must be
// mounted where Hub authentication does not reject CAS tokens.
type Handler struct {
	ing       Ingestor
	prefix    string
	heartbeat time.Duration
	log       *slog.Logger
}

// NewHandler returns the CAS handler.
func NewHandler(ing Ingestor, o HandlerOptions) (*Handler, error) {
	if ing == nil {
		return nil, errors.New("xet: NewHandler needs an Ingestor")
	}
	u, err := url.Parse(o.CASURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("xet: CAS URL %q is not an absolute URL", o.CASURL)
	}
	h := &Handler{ing: ing, prefix: strings.TrimRight(u.EscapedPath(), "/"), heartbeat: o.Heartbeat, log: o.Log}
	if h.heartbeat <= 0 {
		h.heartbeat = DefaultHeartbeat
	}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	return h, nil
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	httpx.SetProtocol(r.Context(), telemetry.ProtocolXet)
	p := r.URL.EscapedPath()
	// hf_xet drops casUrl's path for telemetry (research §1.3).
	if p == "/v1/telemetry" || p == h.prefix+"/v1/telemetry" {
		h.telemetry(w, r)
		return
	}
	rest, ok := strings.CutPrefix(p, h.prefix+"/")
	if !ok {
		httpx.WriteError(w, r, http.StatusNotFound, "", "Not found.")
		return
	}
	segs := strings.Split(rest, "/")
	switch {
	case len(segs) == 4 && segs[0] == "v1" && segs[1] == "chunks":
		h.route(w, r, "/v1/chunks/{prefix}/{hash}", http.MethodGet, false, h.chunk)
	case len(segs) == 4 && segs[0] == "v1" && segs[1] == "xorbs":
		h.route(w, r, "/v1/xorbs/{prefix}/{hash}", http.MethodPost, true, func(w http.ResponseWriter, r *http.Request, c *Claims) {
			h.xorb(w, r, c, segs[3])
		})
	case len(segs) == 2 && segs[0] == "v1" && segs[1] == "shards":
		h.route(w, r, "/v1/shards", http.MethodPost, true, h.shardV1)
	case len(segs) == 2 && segs[0] == "v2" && segs[1] == "shards":
		h.route(w, r, "/v2/shards", http.MethodPost, true, h.shardV2)
	default:
		httpx.WriteError(w, r, http.StatusNotFound, "", "Not found.")
	}
}

// route checks the method and the CAS token, then calls fn.
func (h *Handler) route(w http.ResponseWriter, r *http.Request, route, method string, write bool,
	fn func(http.ResponseWriter, *http.Request, *Claims),
) {
	httpx.SetRoute(r.Context(), h.prefix+route)
	if r.Method != method && (method != http.MethodGet || r.Method != http.MethodHead) {
		w.Header().Set("Allow", method)
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, "", "Method not allowed.")
		return
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || strings.TrimSpace(tok) == "" {
		httpx.WriteError(w, r, http.StatusUnauthorized, "", "A Xet access token is required.")
		return
	}
	c, err := h.ing.VerifyToken(r.Context(), strings.TrimSpace(tok))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if write && c.Scope != ScopeWrite {
		h.fail(w, r, ErrScope)
		return
	}
	httpx.SetRepo(r.Context(), c.Repo)
	httpx.SetPrincipal(r.Context(), model.Principal{
		Kind: model.PrincipalUser, UserID: c.UserID, Username: c.Username, Method: model.AuthXet,
	})
	fn(w, r, c)
}

// status maps an error to its HTTP status and the message users see.
func (h *Handler) status(err error) (int, string) {
	switch {
	case errors.Is(err, ErrInvalidToken):
		return http.StatusUnauthorized, "The Xet access token is invalid or has expired."
	case errors.Is(err, ErrScope):
		return http.StatusForbidden, "The Xet access token does not allow this."
	case errors.Is(err, ErrRejected):
		return http.StatusBadRequest, RejectMessage
	case errors.Is(err, ErrUnknownXorb):
		return http.StatusBadRequest, UnknownXorbMessage
	case errors.Is(err, ErrTooLarge):
		return http.StatusRequestEntityTooLarge, "The upload is too large."
	case errors.Is(err, ErrBadXorb), errors.Is(err, ErrBadShard):
		return http.StatusBadRequest, strings.TrimPrefix(err.Error(), "xet: ")
	default:
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return http.StatusRequestEntityTooLarge, "The upload is too large."
		}
		return http.StatusInternalServerError, "Internal error."
	}
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		return // the client went away
	}
	status, msg := h.status(err)
	if status >= 500 {
		h.log.ErrorContext(r.Context(), "Xet CAS request failed", "path", r.URL.Path, "error", err)
	}
	httpx.WriteError(w, r, status, "", msg)
}

// chunk answers the global deduplication query. Hangar has no global chunk
// deduplication, so every chunk is unknown and the client uploads it.
func (h *Handler) chunk(w http.ResponseWriter, r *http.Request, _ *Claims) {
	httpx.WriteError(w, r, http.StatusNotFound, "", "Chunk not found.")
}

func (h *Handler) xorb(w http.ResponseWriter, r *http.Request, c *Claims, hash string) {
	if !ValidHash(hash) {
		h.fail(w, r, fmt.Errorf("%w: %q is not a xorb hash", ErrBadXorb, hash))
		return
	}
	if r.ContentLength > MaxXorbSize {
		h.fail(w, r, ErrTooLarge)
		return
	}
	body := http.MaxBytesReader(w, r.Body, MaxXorbSize)
	inserted, err := h.ing.PutXorb(r.Context(), c, hash, body, r.ContentLength)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]bool{"was_inserted": inserted})
}

// readShard reads the whole shard body before any response is written:
// HTTP/1.1 bodies may become unreadable once a streamed response starts.
func (h *Handler) readShard(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.ContentLength > MaxShardSize {
		h.fail(w, r, ErrTooLarge)
		return nil, false
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxShardSize))
	if err != nil {
		h.fail(w, r, err)
		return nil, false
	}
	return b, true
}

func (h *Handler) shardV1(w http.ResponseWriter, r *http.Request, c *Claims) {
	b, ok := h.readShard(w, r)
	if !ok {
		return
	}
	if _, err := h.ing.AcceptShard(r.Context(), c, bytes.NewReader(b), int64(len(b)), nil); err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, r, http.StatusOK, map[string]int{"result": 1})
}

// shardV2 streams progress as NDJSON (docs/research/xet-upload-research.md
// §2.3). The status is 200 once streaming starts; the terminal `result` or
// `error` frame decides the outcome. While the shard is processed, the
// last frame is repeated every heartbeat interval, so proxies with idle
// timeouts (such as Cloudflare's 100 seconds) keep the connection open.
func (h *Handler) shardV2(w http.ResponseWriter, r *http.Request, c *Claims) {
	b, ok := h.readShard(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	events := make(chan ShardEvent, 64)
	type outcome struct{ err error }
	done := make(chan outcome, 1)
	go func() {
		_, err := h.ing.AcceptShard(ctx, c, bytes.NewReader(b), int64(len(b)), func(e ShardEvent) {
			// The handler writes the terminal frame itself.
			if e.Type == EventResult || e.Type == EventError {
				return
			}
			select {
			case events <- e:
			case <-ctx.Done():
			}
		})
		done <- outcome{err}
	}()

	hdr := w.Header()
	hdr.Set("Content-Type", "application/x-ndjson")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	last := ShardEvent{Type: EventValidating}
	write := func(e ShardEvent) bool {
		line, err := json.Marshal(e)
		if err != nil {
			return false
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !write(last) {
		<-done
		return
	}
	tick := time.NewTicker(h.heartbeat)
	defer tick.Stop()
	for {
		select {
		case e := <-events:
			last = e
			if !write(e) {
				<-done
				return
			}
		case <-tick.C:
			if !write(last) {
				<-done
				return
			}
		case o := <-done:
			// Send progress frames still queued, then the terminal frame.
			for drained := false; !drained; {
				select {
				case e := <-events:
					write(e)
				default:
					drained = true
				}
			}
			if o.err == nil {
				write(ShardEvent{Type: EventResult})
				return
			}
			if ctx.Err() != nil {
				return
			}
			status, msg := h.status(o.err)
			if status >= 500 {
				h.log.ErrorContext(ctx, "Xet shard upload failed", "error", o.err)
			}
			write(ShardEvent{Type: EventError, Message: msg, Retryable: status >= 500})
			return
		}
	}
}

// telemetry accepts hf_xet's fire-and-forget telemetry and discards it.
func (h *Handler) telemetry(w http.ResponseWriter, r *http.Request) {
	httpx.SetRoute(r.Context(), "/v1/telemetry")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		httpx.WriteError(w, r, http.StatusMethodNotAllowed, "", "Method not allowed.")
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxTelemetryBody))
	w.WriteHeader(http.StatusOK)
}
