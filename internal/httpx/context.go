// Package httpx holds the HTTP plumbing every protocol shares: request IDs,
// panic recovery, the access log, the authentication hook, Hub-style and
// RFC 9457 error writers, trusted proxy handling, body limits, escaped-path
// helpers for the Hub routes, and signed URLs.
package httpx

import (
	"context"
	"sync"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/telemetry"
)

type ctxKey int

const (
	keyInfo ctxKey = iota
)

// RequestInfo describes a request as it moves through the middleware and
// handlers. Handlers fill in Route and Repo so the access log and the span
// name use the route template, never the raw path.
type RequestInfo struct {
	mu        sync.Mutex
	id        string
	route     string
	protocol  telemetry.Protocol
	repo      model.RepoID
	principal model.Principal
}

// ID returns the request ID.
func (i *RequestInfo) ID() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.id
}

// Route returns the route template, such as `GET /{ns}/{name}/resolve/{rev}/{path}`
// without the method, or "" if no handler set one.
func (i *RequestInfo) Route() string {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.route
}

// Protocol returns the protocol the request was dispatched to.
func (i *RequestInfo) Protocol() telemetry.Protocol {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.protocol
}

// Repo returns the Repo the request is about, if a handler set one.
func (i *RequestInfo) Repo() model.RepoID {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.repo
}

// Principal returns the authenticated caller.
func (i *RequestInfo) Principal() model.Principal {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.principal
}

func infoFrom(ctx context.Context) *RequestInfo {
	if i, ok := ctx.Value(keyInfo).(*RequestInfo); ok {
		return i
	}
	return nil
}

// Info returns the RequestInfo of the request, or nil outside a request.
func Info(ctx context.Context) *RequestInfo { return infoFrom(ctx) }

// WithInfo returns a context with a fresh RequestInfo. The request ID
// middleware calls it; tests can call it directly. Log records written
// with the context carry the ID as request_id.
func WithInfo(ctx context.Context, requestID string) context.Context {
	ctx = telemetry.WithRequestID(ctx, requestID)
	return context.WithValue(ctx, keyInfo, &RequestInfo{id: requestID, protocol: telemetry.ProtocolUnknown})
}

// RequestID returns the request ID, or "".
func RequestID(ctx context.Context) string {
	if i := infoFrom(ctx); i != nil {
		return i.ID()
	}
	return ""
}

// InternalErrorMessage is the message of a 500 response. It names the
// request ID, which every log line written for the request carries as
// request_id, so the caller can quote it and an operator can find the cause.
func InternalErrorMessage(ctx context.Context) string {
	if id := RequestID(ctx); id != "" {
		return "Internal server error. The request ID is " + id + "."
	}
	return "Internal server error."
}

// SetRoute records the route template, for example `/{ns}/{name}/resolve/{rev}/{path}`.
func SetRoute(ctx context.Context, route string) {
	if i := infoFrom(ctx); i != nil {
		i.mu.Lock()
		i.route = route
		i.mu.Unlock()
	}
}

// SetProtocol records the protocol. It also adds hangar.protocol to the
// HTTP server metrics otelhttp records for the request, so dashboards can
// show traffic by protocol (11-observability.md "Dashboards").
func SetProtocol(ctx context.Context, p telemetry.Protocol) {
	if l, ok := otelhttp.LabelerFromContext(ctx); ok {
		l.Add(telemetry.AttrProtocol.String(string(p)))
	}
	if i := infoFrom(ctx); i != nil {
		i.mu.Lock()
		i.protocol = p
		i.mu.Unlock()
	}
}

// SetRepo records the Repo the request is about, for spans and the access log.
func SetRepo(ctx context.Context, id model.RepoID) {
	if i := infoFrom(ctx); i != nil {
		i.mu.Lock()
		i.repo = id
		i.mu.Unlock()
	}
}

// SetPrincipal records the authenticated caller.
func SetPrincipal(ctx context.Context, p model.Principal) {
	if i := infoFrom(ctx); i != nil {
		i.mu.Lock()
		i.principal = p
		i.mu.Unlock()
	}
}

// Principal returns the authenticated caller, or an anonymous Principal.
func Principal(ctx context.Context) model.Principal {
	if i := infoFrom(ctx); i != nil {
		return i.Principal()
	}
	return model.Anonymous()
}

// ProtocolOf returns the protocol of the request.
func ProtocolOf(ctx context.Context) telemetry.Protocol {
	if i := infoFrom(ctx); i != nil {
		return i.Protocol()
	}
	return telemetry.ProtocolUnknown
}
