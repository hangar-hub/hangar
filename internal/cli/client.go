package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jamesagarside/hangar/internal/api/gen"
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
// because downloads and uploads can take hours, but bounded connection set-up.
func newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = 30 * time.Second
	tr.ResponseHeaderTimeout = 5 * time.Minute
	return &http.Client{Transport: tr}
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
		gen.WithHTTPClient(hc),
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
	return strings.EqualFold(u.Scheme, c.base.Scheme) && strings.EqualFold(u.Host, c.base.Host)
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

// do sends a request to the Instance and returns the response if its status
// is one of ok; otherwise it returns the mapped error.
func (c *client) do(ctx context.Context, method, rawURL string, body io.Reader, header http.Header, ok ...int) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	c.authorise(req)
	resp, err := c.http.Do(req)
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
