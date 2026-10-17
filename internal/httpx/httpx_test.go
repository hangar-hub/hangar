package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/telemetry"
)

func TestRequestIDsKeepsValidAndReplacesInvalid(t *testing.T) {
	var seen string
	h := httpx.RequestIDs(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = httpx.RequestID(r.Context())
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "abc-123")
	h.ServeHTTP(rec, req)
	require.Equal(t, "abc-123", seen)
	require.Equal(t, "abc-123", rec.Header().Get("X-Request-Id"))

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "bad id\nwith newline")
	h.ServeHTTP(rec, req)
	require.Len(t, seen, 32)
	require.Equal(t, seen, rec.Header().Get("X-Request-Id"))
	require.Empty(t, httpx.RequestID(context.Background()))
}

func TestRecoverWritesErrorInProtocolFormat(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	panicky := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.SetProtocol(r.Context(), telemetry.ProtocolHub)
		panic("boom")
	})
	h := httpx.RequestIDs(httpx.Recover(log)(panicky))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.Equal(t, "Internal server error. The request ID is "+rec.Header().Get("X-Request-Id")+".", rec.Header().Get("X-Error-Message"))
	require.Contains(t, logs.String(), "boom")

	abort := httpx.Recover(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) }))
	require.PanicsWithValue(t, http.ErrAbortHandler, func() {
		abort.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	})
}

func TestAccessLogUsesRouteTemplateAndRenamesSpan(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))

	repo := model.RepoID{Type: model.RepoTypeModel, Namespace: "ns", Name: "secret-model"}
	h := httpx.RequestIDs(httpx.AccessLog(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.SetProtocol(r.Context(), telemetry.ProtocolHub)
		httpx.SetRoute(r.Context(), "/{ns}/{name}/resolve/{rev}/{path}")
		httpx.SetRepo(r.Context(), repo)
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("hello"))
	})))
	ctx, span := tp.Tracer("t").Start(context.Background(), "raw")
	req := httptest.NewRequest(http.MethodGet, "/ns/secret-model/resolve/main/x.bin?token=hgr_secret", nil).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), req)
	span.End()

	var line map[string]any
	require.NoError(t, json.Unmarshal(logs.Bytes(), &line))
	require.Equal(t, "/{ns}/{name}/resolve/{rev}/{path}", line["route"])
	require.EqualValues(t, 206, line["status"])
	require.EqualValues(t, 5, line["bytes_out"])
	require.Equal(t, "hub", line["protocol"])
	require.Equal(t, "ns/secret-model", line["repo_id"])
	require.NotContains(t, logs.String(), "hgr_secret", "never log the query string")
	require.NotContains(t, logs.String(), "x.bin", "never log the raw path")

	spans := rec.Ended()
	require.Len(t, spans, 1)
	require.Equal(t, "GET /{ns}/{name}/resolve/{rev}/{path}", spans[0].Name())
}

type fakeAuthn struct {
	p   model.Principal
	err error
}

func (f fakeAuthn) Authenticate(*http.Request) (model.Principal, error) { return f.p, f.err }

func TestAuthenticateStoresPrincipalOrRejects(t *testing.T) {
	user := model.Principal{Kind: model.PrincipalUser, Username: "james"}
	var got model.Principal
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = httpx.Principal(r.Context()) })

	h := httpx.RequestIDs(httpx.Authenticate(fakeAuthn{p: user}, nil)(next))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	require.Equal(t, "james", got.Username)

	// Bad tokens on the Hub API get the exact message huggingface_hub expects.
	bad := httpx.RequestIDs(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.SetProtocol(r.Context(), telemetry.ProtocolHub)
		httpx.Authenticate(fakeAuthn{err: httpx.ErrInvalidCredentials}, nil)(next).ServeHTTP(w, r)
	}))
	rec := httptest.NewRecorder()
	bad.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/whoami-v2", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, httpx.MsgInvalidCredentials, rec.Header().Get("X-Error-Message"))
	require.Empty(t, rec.Header().Get("X-Error-Code"))

	// git asks for a password.
	gitBad := httpx.RequestIDs(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpx.SetProtocol(r.Context(), telemetry.ProtocolGit)
		httpx.Authenticate(fakeAuthn{err: errors.New("nope")}, nil)(next).ServeHTTP(w, r)
	}))
	rec = httptest.NewRecorder()
	gitBad.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ns/n.git/info/refs", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	require.Equal(t, `Basic realm="Hangar"`, rec.Header().Get("WWW-Authenticate"))

	// Without a RequestInfo, Principal is anonymous.
	require.True(t, httpx.Principal(context.Background()).IsAnonymous())
}

func TestHubErrors(t *testing.T) {
	sha := model.CommitSHA(strings.Repeat("a", 40))
	withProto := func(p model.Principal) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/x", nil)
		ctx := httpx.WithInfo(r.Context(), "rid")
		httpx.SetPrincipal(ctx, p)
		return r.WithContext(ctx)
	}
	user := model.Principal{Kind: model.PrincipalUser}
	id := model.RepoID{Type: model.RepoTypeDataset, Namespace: "ns", Name: "n"}

	rec := httptest.NewRecorder()
	httpx.HubEntryNotFound(rec, withProto(user), "config.json", "main", sha)
	require.Equal(t, 404, rec.Code)
	require.Equal(t, "EntryNotFound", rec.Header().Get("X-Error-Code"))
	require.Equal(t, string(sha), rec.Header().Get("X-Repo-Commit"))
	require.JSONEq(t, `{"error":"config.json does not exist on \"main\""}`, rec.Body.String())

	rec = httptest.NewRecorder()
	httpx.HubRepoNotFound(rec, withProto(model.Anonymous()), id)
	require.Equal(t, 401, rec.Code, "anonymous callers get 401")
	require.Equal(t, "RepoNotFound", rec.Header().Get("X-Error-Code"))
	rec = httptest.NewRecorder()
	httpx.HubRepoNotFound(rec, withProto(user), id)
	require.Equal(t, 404, rec.Code)

	rec = httptest.NewRecorder()
	httpx.HubRevisionNotFound(rec, withProto(user), "nope")
	require.Equal(t, "RevisionNotFound", rec.Header().Get("X-Error-Code"))

	rec = httptest.NewRecorder()
	httpx.HubGated(rec, withProto(user), id)
	require.Equal(t, 403, rec.Code)
	require.Equal(t, "GatedRepo", rec.Header().Get("X-Error-Code"))
	rec = httptest.NewRecorder()
	httpx.HubGated(rec, withProto(model.Anonymous()), id)
	require.Equal(t, 401, rec.Code)

	rec = httptest.NewRecorder()
	httpx.WriteHubError(rec, withProto(user), &httpx.HubError{Status: 409, Message: "You already created this model repo", Extra: map[string]any{"url": "http://h/ns/n"}})
	require.JSONEq(t, `{"error":"You already created this model repo","url":"http://h/ns/n"}`, rec.Body.String())

	head := httptest.NewRequest(http.MethodHead, "/x", nil)
	rec = httptest.NewRecorder()
	httpx.WriteHubError(rec, head, &httpx.HubError{Status: 404, Code: "EntryNotFound", Message: "line1\nline2"})
	require.Empty(t, rec.Body.String(), "HEAD has no body")
	require.Equal(t, "line1 line2", rec.Header().Get("X-Error-Message"))
}

func TestProblemAndWriteError(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/hangar/v1/repos", nil)
	ctx := httpx.WithInfo(r.Context(), "rid-1")
	r = r.WithContext(ctx)
	rec := httptest.NewRecorder()
	httpx.WriteProblem(rec, r, &httpx.Problem{Status: 422, Detail: "Name is taken.", Errors: []httpx.ProblemError{{Pointer: "/name", Detail: "taken"}}})
	require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
	require.JSONEq(t, `{"type":"about:blank","title":"Unprocessable Entity","status":422,"detail":"Name is taken.","request_id":"rid-1","errors":[{"pointer":"/name","detail":"taken"}]}`, rec.Body.String())

	for proto, check := range map[telemetry.Protocol]func(*httptest.ResponseRecorder){
		telemetry.ProtocolNative: func(rec *httptest.ResponseRecorder) {
			require.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
		},
		telemetry.ProtocolHub: func(rec *httptest.ResponseRecorder) {
			require.Equal(t, "msg", rec.Header().Get("X-Error-Message"))
		},
		telemetry.ProtocolGit: func(rec *httptest.ResponseRecorder) {
			require.Equal(t, "msg\n", rec.Body.String())
		},
		telemetry.ProtocolLFS: func(rec *httptest.ResponseRecorder) {
			require.Equal(t, "application/vnd.git-lfs+json", rec.Header().Get("Content-Type"))
			require.Contains(t, rec.Body.String(), `"message":"msg"`)
		},
		telemetry.ProtocolOllama: func(rec *httptest.ResponseRecorder) {
			require.JSONEq(t, `{"errors":[{"code":"NAME_UNKNOWN","message":"msg"}]}`, rec.Body.String())
		},
		telemetry.ProtocolViewer: func(rec *httptest.ResponseRecorder) {
			require.JSONEq(t, `{"error":"msg"}`, rec.Body.String())
		},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		ctx := httpx.WithInfo(r.Context(), "x")
		httpx.SetProtocol(ctx, proto)
		rec := httptest.NewRecorder()
		httpx.WriteError(rec, r.WithContext(ctx), 404, "", "msg")
		require.Equal(t, 404, rec.Code, proto)
		check(rec)
	}
	require.Equal(t, "msg", (&httpx.Problem{Detail: "msg"}).Error())
	require.Equal(t, "t", (&httpx.Problem{Title: "t"}).Error())
	require.Equal(t, "m", (&httpx.HubError{Message: "m"}).Error())
}

func TestTrustedProxies(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	var got *http.Request
	h := httpx.TrustedProxies(trusted)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r }))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.1.2.3:5555"
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.9.9.9")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "hangar.example.com")
	h.ServeHTTP(httptest.NewRecorder(), req)
	require.Equal(t, "203.0.113.9", httpx.ClientIP(got))
	require.True(t, httpx.IsHTTPS(got))
	require.Equal(t, "hangar.example.com", got.Host)

	// An untrusted peer cannot spoof any of it.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.7:4444"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("X-Forwarded-Proto", "https")
	h.ServeHTTP(httptest.NewRecorder(), req)
	require.Equal(t, "198.51.100.7", httpx.ClientIP(got))
	require.False(t, httpx.IsHTTPS(got))

	// A bad X-Forwarded-Host is ignored.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.1:1"
	req.Host = "orig"
	req.Header.Set("X-Forwarded-Host", "evil.com/path")
	h.ServeHTTP(httptest.NewRecorder(), req)
	require.Equal(t, "orig", got.Host)
}

func TestMaxBody(t *testing.T) {
	h := httpx.MaxBody(4)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(bytes.Buffer)
		if _, err := buf.ReadFrom(r.Body); err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("12345")))
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("123")))
	require.Equal(t, http.StatusOK, rec.Code)

	// Chunked bodies without a length are cut by MaxBytesReader.
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("123456"))
	req.ContentLength = -1
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestSegmentsKeepsEncodedSlashes(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/models/ns/n/tree/refs%2Fpr%2F1/a%2Fb%20c", nil)
	segs, err := httpx.Segments(req)
	require.NoError(t, err)
	require.Equal(t, []string{"api", "models", "ns", "n", "tree", "refs/pr/1", "a/b c"}, segs)
	segs, err = httpx.SplitEscapedPath("/")
	require.NoError(t, err)
	require.Empty(t, segs)
	_, err = httpx.SplitEscapedPath("/bad%zz")
	require.Error(t, err)
}

func TestQueryHelpers(t *testing.T) {
	for in, want := range map[string]bool{"true": true, "True": true, "1": true, "false": false, "False": false, "0": false} {
		v, ok := httpx.ParseBool(in)
		require.True(t, ok, in)
		require.Equal(t, want, v, in)
	}
	_, ok := httpx.ParseBool("yes")
	require.False(t, ok)

	q := url.Values{"expand": {"a", "b,c"}, "expand[]": {"d"}, "recursive": {"True"}, "limit": {"5000"}, "bad": {"x"}}
	require.Equal(t, []string{"a", "b", "c", "d"}, httpx.QueryList(q, "expand"))
	require.True(t, httpx.QueryBool(q, "recursive", false))
	require.True(t, httpx.QueryBool(q, "missing", true))
	require.False(t, httpx.QueryBool(q, "bad", false))
	require.Equal(t, 1000, httpx.QueryInt(q, "limit", 50, 1, 1000))
	require.Equal(t, 50, httpx.QueryInt(q, "bad", 50, 1, 1000))

	u := httpx.AbsoluteURL("https://hangar.example.com/", "/api/models", url.Values{"cursor": {"abc="}})
	require.Equal(t, "https://hangar.example.com/api/models?cursor=abc%3D", u)
	rec := httptest.NewRecorder()
	httpx.SetLinkNext(rec, u)
	require.Equal(t, `<`+u+`>; rel="next"`, rec.Header().Get("Link"))
}

func TestSigner(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	s, err := httpx.NewSigner(key)
	require.NoError(t, err)
	signed, err := s.Sign("https://h/ns/n.git/info/lfs/objects/upload/abc?part=2", "lfs-upload:abc", time.Hour)
	require.NoError(t, err)
	u, _ := url.Parse(signed)
	scope, err := s.Verify(u)
	require.NoError(t, err)
	require.Equal(t, "lfs-upload:abc", scope)

	// Any change to the path or the query breaks the signature.
	for _, mutate := range []func(*url.URL){
		func(u *url.URL) { u.Path = "/ns/other.git/info/lfs/objects/upload/abc" },
		func(u *url.URL) { q := u.Query(); q.Set("part", "3"); u.RawQuery = q.Encode() },
		func(u *url.URL) { q := u.Query(); q.Set("scope", "admin"); u.RawQuery = q.Encode() },
		func(u *url.URL) { q := u.Query(); q.Del("sig"); u.RawQuery = q.Encode() },
	} {
		m, _ := url.Parse(signed)
		mutate(m)
		_, err := s.Verify(m)
		require.ErrorIs(t, err, httpx.ErrBadSignature)
	}

	later := s.WithClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	_, err = later.Verify(u)
	require.ErrorIs(t, err, httpx.ErrBadSignature)

	other, err := httpx.NewSigner(bytes.Repeat([]byte{8}, 32))
	require.NoError(t, err)
	_, err = other.Verify(u)
	require.ErrorIs(t, err, httpx.ErrBadSignature)

	_, err = httpx.NewSigner([]byte("short"))
	require.Error(t, err)
}

func TestClassify(t *testing.T) {
	cases := map[string]httpx.Target{
		"/healthz":          httpx.TargetHealth,
		"/readyz":           httpx.TargetHealth,
		"/api/hangar/v1/me": httpx.TargetNative,
		"/api/models/ns/n":  httpx.TargetHub,
		"/api/whoami-v2":    httpx.TargetHub,
		"/api/datasets/ns/n/xet-write-token/main": httpx.TargetHub,
		"/oauth/device":                                     httpx.TargetHub,
		"/ns/n/resolve/main/config.json":                    httpx.TargetHub,
		"/datasets/ns/n/resolve/refs%2Fconvert%2Fparquet/x": httpx.TargetHub,
		"/gpt2/resolve/main/config.json":                    httpx.TargetHub,
		"/ns/n.git/info/refs":                               httpx.TargetGit,
		"/ns/n/info/refs":                                   httpx.TargetGit,
		"/ns/n/git-upload-pack":                             httpx.TargetGit,
		"/datasets/ns/n.git/git-receive-pack":               httpx.TargetGit,
		"/ns/n.git/info/lfs/objects/batch":                  httpx.TargetLFS,
		"/datasets/ns/n.git/info/lfs/locks/verify":          httpx.TargetLFS,
		"/v2/ns/n/manifests/Q4_K_M":                         httpx.TargetOllama,
		"/v2/token":                                         httpx.TargetOllama,
		"/xet-cas/v1/xorbs/default/abc":                     httpx.TargetXet,
		"/v1/telemetry":                                     httpx.TargetXet,
		"/datasets-server/splits":                           httpx.TargetViewer,
		"/auth/oidc/unifi/callback":                         httpx.TargetAuth,
		"/auth/login":                                       httpx.TargetUI,
		"/auth/invite/abc":                                  httpx.TargetUI,
		"/assets/index.js":                                  httpx.TargetUI,
		"/":                                                 httpx.TargetUI,
		"/arsenal":                                          httpx.TargetUI,
		"/models":                                           httpx.TargetUI,
		"/datasets":                                         httpx.TargetUI,
		"/ns":                                               httpx.TargetUI,
		"/ns/n":                                             httpx.TargetUI,
		"/ns/n/tree/main/sub":                               httpx.TargetUI,
		"/datasets/ns/n/viewer/default/train":               httpx.TargetUI,
		"/ns/resolve":                                       httpx.TargetUI,
	}
	for path, want := range cases {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		require.Equal(t, want, httpx.Classify(req, "datasets-server.h"), path)
	}
	post := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	require.Equal(t, httpx.TargetAuth, httpx.Classify(post, ""), "form posts go to the auth handler")

	viewer := httptest.NewRequest(http.MethodGet, "/splits", nil)
	viewer.Host = "Datasets-Server.H"
	require.Equal(t, httpx.TargetViewer, httpx.Classify(viewer, "datasets-server.h"))
}

func TestDispatchRoutesAndStripsViewerPrefix(t *testing.T) {
	named := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(name + " " + r.URL.Path + " " + string(httpx.ProtocolOf(r.Context()))))
		})
	}
	h := httpx.RequestIDs(httpx.Dispatch(httpx.Protocols{
		Health: named("health"), Native: named("native"), Hub: named("hub"), Git: named("git"), LFS: named("lfs"),
		Ollama: named("ollama"), Xet: named("xet"), Viewer: named("viewer"), Auth: named("auth"), UI: named("ui"),
	}, "https://hangar.example.com"))

	get := func(host, path string) string {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if host != "" {
			req.Host = host
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	require.Equal(t, "viewer /rows viewer", get("", "/datasets-server/rows"))
	require.Equal(t, "viewer /rows viewer", get("datasets-server.hangar.example.com", "/rows"))
	require.Equal(t, "hub /ns/n/resolve/main/x hub", get("", "/ns/n/resolve/main/x"))
	require.Equal(t, "native /api/hangar/v1/me native", get("", "/api/hangar/v1/me"))
	require.Equal(t, "ui /ns/n ui", get("", "/ns/n"))

	// A missing handler is a 404, not a panic.
	empty := httpx.RequestIDs(httpx.Dispatch(httpx.Protocols{}, "http://h"))
	rec := httptest.NewRecorder()
	empty.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/x", nil))
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestResponseWriterPassThrough(t *testing.T) {
	var logs syncBuffer
	var isReaderFrom bool
	var flushErr error
	h := httpx.RequestIDs(httpx.AccessLog(slog.New(slog.NewJSONHandler(&logs, nil)))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, isReaderFrom = w.(io.ReaderFrom)
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte("abc"))
		flushErr = http.NewResponseController(w).Flush()
	})))
	// AccessLog writes its line after the handler returns, which can be after
	// the client has read the response. Wait for the whole chain to finish
	// before reading anything the server goroutine wrote.
	finished := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	require.NoError(t, err)
	resp.Body.Close()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the middleware chain did not finish")
	}
	require.Equal(t, 200, resp.StatusCode)
	require.True(t, isReaderFrom, "the wrapped writer keeps io.ReaderFrom")
	require.NoError(t, flushErr, "the wrapped writer keeps Flush through ResponseController")
	require.Contains(t, logs.String(), `"bytes_out":3`)
}

// syncBuffer is a bytes.Buffer that is safe to write from a server goroutine
// and read from the test.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The access log line and every other line logged with the request's
// context carry the request ID the response names, with the trace IDs.
func TestLogLinesCarryTheRequestID(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(telemetry.NewStdoutHandler(&logs, "json", slog.LevelInfo))
	tp := sdktrace.NewTracerProvider()
	h := httpx.RequestIDs(httpx.AccessLog(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.ErrorContext(r.Context(), "handler failed")
		httpx.WriteError(w, r, http.StatusInternalServerError, "", httpx.InternalErrorMessage(r.Context()))
	})))
	ctx, span := tp.Tracer("t").Start(context.Background(), "raw")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/hangar/v1/x", nil).WithContext(ctx))
	span.End()

	id := rec.Header().Get("X-Request-Id")
	require.Len(t, id, 32)
	require.Contains(t, rec.Body.String(), "Internal server error. The request ID is "+id+".")
	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	require.Len(t, lines, 2)
	for _, l := range lines {
		var line map[string]any
		require.NoError(t, json.Unmarshal([]byte(l), &line))
		require.Equal(t, id, line["request_id"], l)
		require.Equal(t, span.SpanContext().TraceID().String(), line["trace_id"], l)
	}
	require.Equal(t, "Internal server error.", httpx.InternalErrorMessage(context.Background()))
}
