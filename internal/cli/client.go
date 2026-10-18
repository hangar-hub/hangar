package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hangar-hub/hangar/internal/api/gen"
)

// nativePrefix is where the native API lives on an Instance.
const nativePrefix = "/api/hangar/v1"

// client talks to one Instance through the Hub API and the native API.
type client struct {
	base  *url.URL
	token string
	ua    string
	http  *http.Client
	api   *gen.ClientWithResponses
}

// newHTTPClient returns the HTTP client the CLI uses: no overall timeout,
// because downloads and uploads can take hours, but bounded connection
// set-up and a bounded wait for each answer (responseTimeout).
func newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = 30 * time.Second
	return &http.Client{Transport: responseTimeout{next: tr, wait: defaultResponseTimeout}, CheckRedirect: checkRedirect}
}

// defaultResponseTimeout is how long a request waits for the answer's
// headers, unless its context sets another wait (withResponseTimeout).
const defaultResponseTimeout = 5 * time.Minute

type responseTimeoutKey struct{}

// withResponseTimeout returns ctx with the wait for an answer's headers
// set to d, for a request the Instance takes longer to answer, such as the
// completion of a large upload.
func withResponseTimeout(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, responseTimeoutKey{}, d)
}

// responseTimeout is http.Transport's ResponseHeaderTimeout, with a wait
// that each request's context can set. Once the headers arrive, reading
// the body has no time limit.
type responseTimeout struct {
	next http.RoundTripper
	wait time.Duration
}

// errNoAnswer is the cause of a request cancelled by responseTimeout.
var errNoAnswer = errors.New("no answer")

// ResponseTimeoutError is a request that got no answer within its wait.
// The Instance may still be working on it.
type ResponseTimeoutError struct{ Wait time.Duration }

func (e *ResponseTimeoutError) Error() string {
	return fmt.Sprintf("no answer from the Instance within %s", formatDuration(e.Wait))
}

// Timeout reports true, as net.Error does for a timeout.
func (e *ResponseTimeoutError) Timeout() bool { return true }

// Temporary reports true: the request may succeed if it is sent again.
func (e *ResponseTimeoutError) Temporary() bool { return true }

func (rt responseTimeout) RoundTrip(req *http.Request) (*http.Response, error) {
	wait := rt.wait
	if d, ok := req.Context().Value(responseTimeoutKey{}).(time.Duration); ok && d > 0 {
		wait = d
	}
	ctx, cancel := context.WithCancelCause(req.Context())
	timer := time.AfterFunc(wait, func() { cancel(errNoAnswer) })
	resp, err := rt.next.RoundTrip(req.WithContext(ctx)) // #nosec G704 -- the request the caller built.
	timer.Stop()
	if err != nil {
		cancel(nil)
		if errors.Is(context.Cause(ctx), errNoAnswer) && req.Context().Err() == nil {
			return nil, &ResponseTimeoutError{Wait: wait}
		}
		return nil, err
	}
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: func() { cancel(nil) }}
	return resp, nil
}

// cancelOnClose ends a request's context when its body is closed.
type cancelOnClose struct {
	io.ReadCloser
	cancel func()
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// maxRedirects is how many redirects one request may follow, as in
// net/http.
const maxRedirects = 10

// checkRedirect drops the Token from a redirect to another origin. net/http
// keeps the Authorization header on redirects to a subdomain, so without
// this a download redirected to a presigned URL on, for example,
// s3.hangar.example.com would send the Token to the storage endpoint, which
// also refuses a request that is signed twice.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if !sameOrigin(req.URL, via[0].URL) {
		req.Header.Del("Authorization")
	}
	return nil
}

// sameOrigin reports whether two URLs have the same scheme and host.
func sameOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// parseInstanceURL checks an Instance URL and strips any trailing slash.
func parseInstanceURL(raw string) (*url.URL, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, usagef("%q is not an Instance URL; use the form https://hangar.example.com", raw)
	}
	u.RawQuery, u.Fragment = "", ""
	return u, nil
}

func newClient(base *url.URL, token, version string, hc *http.Client) (*client, error) {
	c := &client{base: base, token: token, ua: "hangar/" + version, http: hc}
	api, err := gen.NewClientWithResponses(base.String()+nativePrefix,
		gen.WithHTTPClient(redactingDoer{hc}),
		gen.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			c.authorise(req)
			return nil
		}))
	if err != nil {
		return nil, err
	}
	c.api = api
	return c, nil
}

// sameOrigin reports whether u is on the Instance, so it may carry the Token.
func (c *client) sameOrigin(u *url.URL) bool {
	return sameOrigin(u, c.base)
}

// authorise adds the User-Agent, and the Token for requests to the Instance.
func (c *client) authorise(req *http.Request) {
	req.Header.Set("User-Agent", c.ua)
	if c.token != "" && c.sameOrigin(req.URL) {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// url returns an absolute URL on the Instance for an escaped path.
func (c *client) url(escapedPath string) string {
	return c.base.String() + escapedPath
}

// send sends a request with the CLI's HTTP client, redacting the URLs in
// its errors (see redactURLError).
func (c *client) send(req *http.Request) (*http.Response, error) {
	return redactingDoer{c.http}.Do(req)
}

// do sends a request to the Instance and returns the response if its status
// is one of ok; otherwise it returns the mapped error.
func (c *client) do(ctx context.Context, method, rawURL string, body io.Reader, header http.Header, ok ...int) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, redactURLError(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	c.authorise(req)
	resp, err := c.send(req)
	if err != nil {
		return nil, fmt.Errorf("reach %s: %w", c.base.Host, err)
	}
	for _, s := range ok {
		if resp.StatusCode == s {
			return resp, nil
		}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return nil, apiError(resp.StatusCode, resp.Header, b)
}

// check returns nil if a generated client's response has one of the wanted
// statuses, and the mapped error otherwise.
func check(hr *http.Response, body []byte, want ...int) error {
	if hr == nil {
		return fmt.Errorf("no response from the Instance")
	}
	for _, w := range want {
		if hr.StatusCode == w {
			return nil
		}
	}
	return apiError(hr.StatusCode, hr.Header, body)
}

// app holds what every client command shares: the global flags and output.
type app struct {
	opts   Options
	url    string
	token  string
	json   bool
	stdout io.Writer
	stderr io.Writer
	http   *http.Client
}

// instanceURL resolves the Instance URL: --url, then HANGAR_CLI_URL, then
// the saved configuration.
func (a *app) instanceURL(s *Settings) (*url.URL, error) {
	raw := a.url
	if raw == "" {
		raw = os.Getenv(EnvURL)
	}
	if raw == "" && s != nil {
		raw = s.URL
	}
	if raw == "" {
		return nil, usagef("no Instance URL: run hangar login --url <url>, or pass --url")
	}
	return parseInstanceURL(raw)
}

// client builds a client for the configured Instance and Token. The Token
// comes from --token, then HANGAR_CLI_TOKEN, then the saved login for the
// same URL.
func (a *app) client() (*client, error) {
	s, _, err := loadSettings()
	if err != nil {
		return nil, err
	}
	base, err := a.instanceURL(s)
	if err != nil {
		return nil, err
	}
	token := a.token
	if token == "" {
		token = os.Getenv(EnvToken)
	}
	if token == "" && s.URL != "" {
		if saved, err := parseInstanceURL(s.URL); err == nil && saved.String() == base.String() {
			if token, err = s.token(); err != nil {
				return nil, err
			}
		}
	}
	return newClient(base, token, a.opts.Version, a.http)
}
