package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/telemetry"
)

// otlpCollector is a stand-in OTLP/HTTP collector that keeps the raw
// request bodies. OTLP protobuf stores strings as raw bytes, so a secret
// in any span, metric or log attribute shows up in the bodies verbatim.
type otlpCollector struct {
	mu     sync.Mutex
	bodies map[string][][]byte
}

func (c *otlpCollector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.bodies[r.URL.Path] = append(c.bodies[r.URL.Path], b)
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

func (c *otlpCollector) all() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []byte
	for _, bs := range c.bodies {
		for _, b := range bs {
			out = append(out, b...)
		}
	}
	return out
}

// TestSecretsStayOutOfLogsAndTelemetry runs a full server with debug logs
// and every signal exported over OTLP to a local collector, drives the
// flows that carry secrets (password sign-in, the session cookie and its
// CSRF token, Bearer and Basic Tokens, wrong credentials, signed LFS
// links, upstream tokens, webhook secrets, device codes, and invite, reset
// and email confirmation codes in URL paths), and checks
// that none of them reaches stdout or the collector (CLAUDE.md: never log
// or record tokens, passwords, cookies, presigned URLs or Authorization
// headers).
func TestSecretsStayOutOfLogsAndTelemetry(t *testing.T) {
	col := &otlpCollector{bodies: map[string][][]byte{}}
	colSrv := httptest.NewServer(col)
	defer colSrv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", colSrv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "none")
	t.Setenv("OTEL_METRIC_EXPORT_INTERVAL", "200")

	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	pub := "http://" + srv.Listener.Addr().String()
	cfg := testConfig(t)
	cfg.Server.PublicURL = pub
	cfg.Telemetry.Traces, cfg.Telemetry.Metrics, cfg.Telemetry.Logs = "otlp", "otlp", "otlp"
	cfg.Telemetry.LogLevel, cfg.Telemetry.LogFormat = "debug", "json"
	ctx := context.Background()
	_, err := config.EnsureSecrets(cfg)
	require.NoError(t, err)
	var stdout syncBuffer
	tel, err := telemetry.Setup(ctx, telemetry.Options{Config: cfg.Telemetry, Version: version, Role: cfg.Server.Role, Stdout: &stdout})
	require.NoError(t, err)
	require.Equal(t, "otlp", tel.Exporters["traces"])
	require.Equal(t, "otlp", tel.Exporters["logs"])
	d, err := openDatabase(ctx, cfg, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, d.Migrate(ctx))
	blobs, err := openBlobStore(ctx, cfg, tel)
	require.NoError(t, err)
	in, err := newInstance(ctx, cfg, tel, d, blobs)
	require.NoError(t, err)
	t.Cleanup(func() { in.close(ctx) })
	h, err := newHandler(ctx, in, newHealth(in.db, in.blobs), false)
	require.NoError(t, err)
	srv.Config.Handler = h
	srv.Start()
	t.Cleanup(srv.Close)

	const password = "a long enough password 1"
	_, err = in.auth.Users.Create(ctx, auth.NewUser{Username: "alice", Password: password, IsAdmin: true}, model.System())
	require.NoError(t, err)
	secrets := map[string]string{"password": password}

	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	plain := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	send := func(c *http.Client, method, path, body string, set func(*http.Request)) (int, []byte) {
		t.Helper()
		target := path
		if !strings.HasPrefix(path, "http") {
			target = pub + path
		}
		req, err := http.NewRequest(method, target, strings.NewReader(body))
		require.NoError(t, err)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if set != nil {
			set(req)
		}
		resp, err := c.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, raw
	}

	// Password sign-in, then the session cookie and its CSRF token.
	status, raw := send(browser, http.MethodPost, "/api/hangar/v1/auth/login", `{"username":"alice","password":"`+password+`"}`, nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	u, _ := url.Parse(pub)
	for _, c := range jar.Cookies(u) {
		secrets["cookie "+c.Name] = c.Value
	}
	var me struct {
		CSRF string `json:"csrf_token"`
	}
	status, raw = send(browser, http.MethodGet, "/api/hangar/v1/me", "", nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	require.NoError(t, json.Unmarshal(raw, &me))
	require.NotEmpty(t, me.CSRF)
	secrets["csrf"] = me.CSRF
	withCSRF := func(r *http.Request) { r.Header.Set(auth.CSRFHeader, me.CSRF) }

	// A Token, shown once.
	status, raw = send(browser, http.MethodPost, "/api/hangar/v1/me/tokens", `{"name":"laptop","scope":"admin"}`, withCSRF)
	require.Equal(t, http.StatusCreated, status, string(raw))
	var created struct {
		Secret string `json:"secret"`
	}
	require.NoError(t, json.Unmarshal(raw, &created))
	token := created.Secret
	secrets["token"] = token
	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
	basic := func(r *http.Request) { r.SetBasicAuth("alice", token) }

	// An Upstream with an Instance token, a user's upstream token and a
	// webhook secret, all stored encrypted.
	secrets["instance upstream token"] = "hf_instancesecret0123456789abcdef"
	status, raw = send(plain, http.MethodPost, "/api/hangar/v1/admin/upstreams",
		`{"name":"hub","base_url":"https://upstream.example.com","enabled":true,"instance_token":"`+secrets["instance upstream token"]+`"}`, bearer)
	require.Equal(t, http.StatusCreated, status, string(raw))
	secrets["upstream token"] = "hf_upstreamsecret0123456789abcdef"
	status, raw = send(plain, http.MethodPut, "/api/hangar/v1/me/upstream-tokens/hub", `{"token":"`+secrets["upstream token"]+`"}`, bearer)
	require.Equal(t, http.StatusOK, status, string(raw))
	status, raw = send(plain, http.MethodPost, "/api/repos/create", `{"name":"m","type":"model","private":true}`, bearer)
	require.Equal(t, http.StatusOK, status, string(raw))
	secrets["webhook secret"] = "webhook-secret-5b9c1e7d"
	status, raw = send(plain, http.MethodPost, "/api/hangar/v1/webhooks",
		`{"repo":"model/alice/m","url":"https://hooks.example.com/x","events":["repo.created"],"secret":"`+secrets["webhook secret"]+`"}`, bearer)
	require.Equal(t, http.StatusCreated, status, string(raw))

	// An LFS upload and download through signed links, with Basic.
	content := []byte("signed link content")
	sum := sha256.Sum256(content)
	oid := hex.EncodeToString(sum[:])
	lfsHeaders := func(r *http.Request) {
		basic(r)
		r.Header.Set("Content-Type", "application/vnd.git-lfs+json")
		r.Header.Set("Accept", "application/vnd.git-lfs+json")
	}
	batch := func(op string) map[string]struct {
		Href   string            `json:"href"`
		Header map[string]string `json:"header"`
	} {
		t.Helper()
		status, raw := send(plain, http.MethodPost, "/alice/m.git/info/lfs/objects/batch",
			`{"operation":"`+op+`","transfers":["basic"],"objects":[{"oid":"`+oid+`","size":`+jsonInt(int64(len(content)))+`}]}`, lfsHeaders)
		require.Equal(t, http.StatusOK, status, string(raw))
		var out struct {
			Objects []struct {
				Actions map[string]struct {
					Href   string            `json:"href"`
					Header map[string]string `json:"header"`
				} `json:"actions"`
			} `json:"objects"`
		}
		require.NoError(t, json.Unmarshal(raw, &out), string(raw))
		require.Len(t, out.Objects, 1, string(raw))
		return out.Objects[0].Actions
	}
	up := batch("upload")
	require.Contains(t, up, "upload")
	addSig := func(name, href string) {
		hu, err := url.Parse(href)
		require.NoError(t, err)
		if s := hu.Query().Get("sig"); s != "" {
			secrets[name] = s
		}
		for _, k := range []string{"X-Amz-Signature", "X-Amz-Credential"} {
			if s := hu.Query().Get(k); s != "" {
				secrets[name+" "+k] = s
			}
		}
	}
	addSig("upload link signature", up["upload"].Href)
	status, raw = send(plain, http.MethodPut, up["upload"].Href, string(content), func(r *http.Request) {
		for k, v := range up["upload"].Header {
			r.Header.Set(k, v)
		}
	})
	require.Less(t, status, 300, string(raw))
	if v, ok := up["verify"]; ok {
		addSig("verify link signature", v.Href)
		status, raw = send(plain, http.MethodPost, v.Href, `{"oid":"`+oid+`","size":`+jsonInt(int64(len(content)))+`}`, func(r *http.Request) {
			lfsHeaders(r)
			for k, val := range v.Header {
				r.Header.Set(k, val)
			}
		})
		require.Less(t, status, 300, string(raw))
	}
	status, raw = send(plain, http.MethodPost, "/api/models/alice/m/commit/main",
		`{"key":"header","value":{"summary":"Add weights"}}`+"\n"+`{"key":"lfsFile","value":{"path":"w.bin","algo":"sha256","oid":"`+oid+`","size":`+jsonInt(int64(len(content)))+`}}`+"\n",
		func(r *http.Request) { bearer(r); r.Header.Set("Content-Type", "application/x-ndjson") })
	require.Equal(t, http.StatusOK, status, string(raw))
	down := batch("download")
	require.Contains(t, down, "download")
	addSig("download link signature", down["download"].Href)
	status, raw = send(plain, http.MethodGet, down["download"].Href, "", nil)
	require.Equal(t, http.StatusOK, status, string(raw))
	require.Equal(t, content, raw)
	status, _ = send(plain, http.MethodGet, "/alice/m/resolve/main/w.bin", "", bearer)
	require.Less(t, status, 400)

	// Wrong credentials of every kind, and secrets in places a careless
	// log line would copy: the query string and an unknown device code.
	secrets["wrong token"] = "hgr_" + strings.Repeat("Z", 40)
	secrets["wrong basic password"] = "not-the-token-7f3a"
	secrets["device code"] = "devicecode-secret-91c2"
	secrets["query secret"] = "querysecret-3e8d"
	send(plain, http.MethodGet, "/api/whoami-v2", "", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secrets["wrong token"]) })
	send(plain, http.MethodGet, "/alice/m.git/info/refs?service=git-upload-pack", "", func(r *http.Request) { r.SetBasicAuth("alice", secrets["wrong basic password"]) })
	send(plain, http.MethodGet, "/api/hangar/v1/me", "", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: "stale-cookie-6a1f"}) })
	secrets["stale cookie"] = "stale-cookie-6a1f"
	send(plain, http.MethodPost, "/oauth/token", "grant_type=urn:ietf:params:oauth:grant-type:device_code&device_code="+secrets["device code"],
		func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") })
	send(plain, http.MethodGet, "/api/models/alice/m?token="+secrets["query secret"], "", bearer)
	send(plain, http.MethodGet, "/api/models/alice/m/tree/main?access_token="+secrets["query secret"], "", bearer)
	// A cross-origin change with browser credentials, refused.
	status, _ = send(browser, http.MethodDelete, "/api/hangar/v1/repos/models/alice/m", "", func(r *http.Request) {
		withCSRF(r)
		r.Header.Set("Origin", "https://evil.example")
		r.Header.Set("Sec-Fetch-Site", "cross-site")
	})
	require.Equal(t, http.StatusForbidden, status)

	// Invite, reset and email confirmation links carry their code in the
	// path. Open each page and post to each, as the UI does; the invite and
	// reset posts fail the password rules, so those codes stay unused.
	pathCode := func(link *auth.Link) string {
		return link.URL[strings.LastIndex(link.URL, "/")+1:]
	}
	invite, err := in.auth.CreateInvite(ctx, auth.InviteParams{MakeAdmin: true}, model.System())
	require.NoError(t, err)
	secrets["invite code"] = pathCode(invite)
	alice, err := in.auth.Users.GetByUsername(ctx, "alice")
	require.NoError(t, err)
	reset, err := in.auth.CreateResetLink(ctx, alice.ID, model.System())
	require.NoError(t, err)
	secrets["reset code"] = pathCode(reset)
	email := "alice@example.com"
	_, err = in.auth.UpdateProfile(ctx, alice.Principal(model.AuthSession), auth.ProfileUpdate{Email: &email}, "", "")
	require.NoError(t, err)
	confirm, err := in.auth.CreateEmailLink(ctx, alice.ID, email, model.System())
	require.NoError(t, err)
	secrets["email confirmation code"] = pathCode(confirm)
	for _, path := range []string{auth.InvitePath + secrets["invite code"], auth.ResetPath + secrets["reset code"]} {
		send(plain, http.MethodGet, path, "", nil)
		status, raw = send(plain, http.MethodPost, path, `{"username":"newcomer","password":"short"}`, nil)
		require.Equal(t, http.StatusUnprocessableEntity, status, string(raw))
	}
	send(plain, http.MethodGet, auth.EmailPath+secrets["email confirmation code"], "", nil)
	status, raw = send(plain, http.MethodPost, auth.EmailPath+secrets["email confirmation code"], "", nil)
	require.Equal(t, http.StatusNoContent, status, string(raw))

	// A passing health probe is logged at DEBUG, which shows the level is on.
	status, _ = send(plain, http.MethodGet, "/healthz", "", nil)
	require.Equal(t, http.StatusOK, status)

	// Flush every exporter, then look everywhere.
	srv.Close()
	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	require.NoError(t, tel.Shutdown(shutdownCtx))
	exported := col.all()
	require.NotEmpty(t, col.bodies["/v1/traces"], "spans reached the collector")
	require.NotEmpty(t, col.bodies["/v1/logs"], "log records reached the collector")
	logs := stdout.Bytes()
	require.Contains(t, string(logs), `"level":"DEBUG"`, "debug logging is on")
	require.True(t, bytes.Contains(exported, []byte("/{ns}/{name}.git/info/lfs/objects/batch")), "the collector got the LFS spans")
	require.True(t, bytes.Contains(exported, []byte(auth.ResetPath+httpx.RedactedSegment)), "the collector got the reset link spans")
	for name, s := range secrets {
		require.NotEmpty(t, s, name)
		require.False(t, bytes.Contains(logs, []byte(s)), "stdout logs contain the %s", name)
		require.False(t, bytes.Contains(exported, []byte(s)), "OTLP export contains the %s", name)
	}
	for _, header := range []string{"Bearer hgr_", "Basic "} {
		require.False(t, bytes.Contains(logs, []byte(header)), "stdout logs contain %q", header)
		require.False(t, bytes.Contains(exported, []byte(header)), "OTLP export contains %q", header)
	}
	t.Logf("checked %d secrets against %d bytes of logs and %d bytes of OTLP", len(secrets), len(logs), len(exported))
}

// syncBuffer is a bytes.Buffer safe for concurrent writers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b.Bytes()...)
}
