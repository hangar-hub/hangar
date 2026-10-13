package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/jamesagarside/hangar/internal/blob"
	"github.com/jamesagarside/hangar/internal/model"
)

// HTTPOptions configure an HTTPClient.
type HTTPOptions struct {
	// Version goes in the User-Agent: `hangar/<version>`.
	Version string
	// Transport sends requests. Nil means a clone of http.DefaultTransport.
	Transport http.RoundTripper
	// MaxAttempts bounds tries per request on 429, 5xx and network
	// errors. Zero means 4.
	MaxAttempts int
	// BaseBackoff is the first backoff step. Zero means one second.
	BaseBackoff time.Duration
	// MaxBackoff caps one wait, and a Retry-After longer than it is
	// returned as a RateLimitError instead of waited for. Zero means 5 minutes.
	MaxBackoff time.Duration
	// RequestTimeout bounds metadata requests (not downloads or fetches).
	// Zero means 60 seconds.
	RequestTimeout time.Duration
	// MaxJSONBytes bounds JSON responses. Zero means 64 MiB.
	MaxJSONBytes int64
	// Sleep waits for d or until ctx ends. Nil means a timer.
	Sleep func(ctx context.Context, d time.Duration) error
	// Tracer records one span per upstream request. Nil means the global
	// tracer provider.
	Tracer trace.Tracer
}

// HTTPClient is the Client for Upstreams that speak the Hugging Face Hub
// API (huggingface.co, another Hangar, a corporate mirror). It uses the
// Hub HTTP API for metadata and files, git smart HTTP for history, follows
// classic `resolve` redirects, never asks for Xet, honours Retry-After and
// backs off with full jitter on 429 and 5xx (06-upstreams-and-pulls.md).
//
// Tokens travel in the Authorization header only, which the HTTP client
// drops on redirects to another host. Errors never contain URLs with query
// strings, so presigned redirect targets do not reach logs or spans.
type HTTPClient struct {
	opts HTTPOptions
	hc   *http.Client
}

var _ Client = (*HTTPClient)(nil)

// NewHTTPClient returns an HTTPClient.
func NewHTTPClient(opts HTTPOptions) *HTTPClient {
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.Transport == nil {
		opts.Transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 4
	}
	if opts.BaseBackoff <= 0 {
		opts.BaseBackoff = time.Second
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = 5 * time.Minute
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = 60 * time.Second
	}
	if opts.MaxJSONBytes <= 0 {
		opts.MaxJSONBytes = 64 << 20
	}
	if opts.Sleep == nil {
		opts.Sleep = sleepCtx
	}
	if opts.Tracer == nil {
		opts.Tracer = otel.Tracer("github.com/jamesagarside/hangar/internal/upstream")
	}
	hc := &http.Client{Transport: opts.Transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		// Never send credentials to another origin. Go only drops them
		// when the host name changes, not the port or scheme, and a
		// presigned Blob store URL on the same host must not get them.
		prev := via[len(via)-1].URL
		if !strings.EqualFold(prev.Scheme, req.URL.Scheme) || !strings.EqualFold(prev.Host, req.URL.Host) {
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
		}
		return nil
	}}
	return &HTTPClient{opts: opts, hc: hc}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *HTTPClient) userAgent() string { return "hangar/" + c.opts.Version }

// request describes one upstream call.
type request struct {
	method string
	url    string
	// route is the URL template for spans, such as /api/{type}s/{repo}.
	route   string
	body    []byte
	header  http.Header
	cred    Credentials
	git     bool
	upName  string
	stream  bool
	noRetry bool
}

// do sends r, retrying on 429, 5xx and network errors before any body is
// read. The caller closes the body of a successful (2xx) response.
// Non-2xx answers are turned into errors.
func (c *HTTPClient) do(ctx context.Context, r request) (*http.Response, error) {
	ctx, span := c.opts.Tracer.Start(ctx, "upstream "+r.method+" "+r.route, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("upstream", r.upName), attribute.String("http.request.method", r.method),
			attribute.String("url.template", r.route)))
	resp, err := c.doRetry(ctx, r)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	} else {
		span.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
	}
	span.End()
	return resp, err
}

func (c *HTTPClient) doRetry(ctx context.Context, r request) (*http.Response, error) {
	var lastErr error
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, r.method, r.url, bytes.NewReader(r.body))
		if err != nil {
			return nil, fmt.Errorf("build upstream request: %w", redactErr(err))
		}
		if r.body == nil {
			req.Body, req.ContentLength = http.NoBody, 0
		}
		for k, v := range r.header {
			req.Header[k] = v
		}
		req.Header.Set("User-Agent", c.userAgent())
		if r.cred.Token != "" {
			if r.git {
				req.SetBasicAuth("hangar", r.cred.Token)
			} else {
				req.Header.Set("Authorization", "Bearer "+r.cred.Token)
			}
		}
		resp, err := c.hc.Do(req)
		var wait time.Duration
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("%w: %w", ErrUnavailable, redactErr(err))
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			return resp, nil
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			retryAfter, hasRA := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
			lastErr = c.statusError(resp)
			drain(resp)
			if hasRA {
				if retryAfter > c.opts.MaxBackoff {
					return nil, &RateLimitError{RetryAfter: retryAfter}
				}
				wait = retryAfter
			}
			if resp.StatusCode == http.StatusTooManyRequests && attempt >= c.opts.MaxAttempts {
				if !hasRA {
					retryAfter = c.backoff(attempt)
				}
				return nil, &RateLimitError{RetryAfter: retryAfter}
			}
		default:
			err := c.statusError(resp)
			drain(resp)
			return nil, err
		}
		if r.noRetry || attempt >= c.opts.MaxAttempts {
			return nil, lastErr
		}
		if wait == 0 {
			wait = c.backoff(attempt)
		}
		if err := c.opts.Sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
}

// backoff is exponential with full jitter, capped at MaxBackoff.
func (c *HTTPClient) backoff(attempt int) time.Duration {
	ceil := c.opts.BaseBackoff
	for i := 1; i < attempt && ceil < c.opts.MaxBackoff; i++ {
		ceil *= 2
	}
	ceil = min(ceil, c.opts.MaxBackoff)
	return time.Duration(rand.Int64N(int64(ceil)) + 1)
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
}

// parseRetryAfter reads a Retry-After value in seconds or as an HTTP date.
func parseRetryAfter(v string, now time.Time) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	if s, err := strconv.ParseInt(v, 10, 64); err == nil && s >= 0 {
		return time.Duration(s) * time.Second, true
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

// statusError maps an upstream error answer to the package errors, using
// the Hub's X-Error-Code first (huggingface.co answers 401 for Repos it
// will not show anonymous callers).
func (c *HTTPClient) statusError(resp *http.Response) error {
	code := resp.Header.Get("X-Error-Code")
	msg := strings.TrimSpace(resp.Header.Get("X-Error-Message"))
	if msg == "" {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			msg = e.Error
		}
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	detail := fmt.Sprintf("upstream answered %d", resp.StatusCode)
	if msg != "" {
		detail += ": " + msg
	}
	switch {
	case code == "GatedRepo":
		return fmt.Errorf("%w (%s)", ErrGated, detail)
	case code == "RepoNotFound" || code == "RevisionNotFound" || code == "EntryNotFound":
		return fmt.Errorf("%w (%s)", ErrNotFound, detail)
	case resp.StatusCode == http.StatusUnauthorized:
		return fmt.Errorf("%w (%s)", ErrUnauthorized, detail)
	case resp.StatusCode == http.StatusForbidden:
		if strings.Contains(strings.ToLower(msg), "gated") {
			return fmt.Errorf("%w (%s)", ErrGated, detail)
		}
		return fmt.Errorf("%w (%s)", ErrUnauthorized, detail)
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return fmt.Errorf("%w (%s)", ErrNotFound, detail)
	case resp.StatusCode == http.StatusTooManyRequests:
		ra, _ := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		return &RateLimitError{RetryAfter: ra}
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w (%s)", ErrUnavailable, detail)
	default:
		return errors.New(detail)
	}
}

func apiBase(u Upstream) string { return strings.TrimRight(u.BaseURL, "/") }

func typePath(t model.RepoType) string {
	if t == "" {
		t = model.RepoTypeModel
	}
	return t.Plural()
}

// escapeRepo escapes `ns/name` for a URL path, keeping the slash.
func escapeRepo(id model.RepoID) string {
	return url.PathEscape(id.Namespace) + "/" + url.PathEscape(id.Name)
}

func escapeFilePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func (c *HTTPClient) getJSON(ctx context.Context, r request, v any) (http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, c.opts.RequestTimeout)
	defer cancel()
	r.method = http.MethodGet
	if r.header == nil {
		r.header = http.Header{}
	}
	r.header.Set("Accept", "application/json")
	resp, err := c.do(ctx, r)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.opts.MaxJSONBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read upstream answer: %w", ErrUnavailable, redactErr(err))
	}
	if int64(len(body)) > c.opts.MaxJSONBytes {
		return nil, fmt.Errorf("upstream answer is larger than %d bytes", c.opts.MaxJSONBytes)
	}
	if v != nil {
		if raw, ok := v.(*json.RawMessage); ok {
			if !json.Valid(body) {
				return nil, errors.New("upstream answered with invalid JSON")
			}
			*raw = append((*raw)[:0], body...)
		} else if err := json.Unmarshal(body, v); err != nil {
			return nil, fmt.Errorf("decode upstream answer: %w", err)
		}
	}
	return resp.Header, nil
}

// hubTime parses the Hub's timestamps.
type hubTime struct{ time.Time }

func (t *hubTime) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil || s == "" {
		return nil
	}
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	t.Time = v.UTC()
	return nil
}

// gatedValue reads the Hub's `gated`: false, "auto" or "manual".
type gatedValue string

func (g *gatedValue) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*g = gatedValue(s)
		return nil
	}
	var v bool
	if json.Unmarshal(b, &v) == nil && v {
		*g = "manual"
		return nil
	}
	*g = ""
	return nil
}

type hubSibling struct {
	RFilename string `json:"rfilename"`
	Size      *int64 `json:"size"`
	LFS       *struct {
		SHA256      string `json:"sha256"`
		Size        int64  `json:"size"`
		PointerSize int    `json:"pointerSize"`
	} `json:"lfs"`
}

type hubRepo struct {
	ID           string         `json:"id"`
	SHA          string         `json:"sha"`
	Private      bool           `json:"private"`
	Gated        gatedValue     `json:"gated"`
	Tags         []string       `json:"tags"`
	PipelineTag  string         `json:"pipeline_tag"`
	LibraryName  string         `json:"library_name"`
	Downloads    int64          `json:"downloads"`
	Likes        int64          `json:"likes"`
	LastModified hubTime        `json:"lastModified"`
	CardData     map[string]any `json:"cardData"`
	Siblings     []hubSibling   `json:"siblings"`
}

// RepoInfo implements Client with GET /api/{type}s/{repo}[/revision/{rev}]?blobs=true.
func (c *HTTPClient) RepoInfo(ctx context.Context, u Upstream, cred Credentials, id model.RepoID, revision string) (*RepoInfo, error) {
	p := "/api/" + typePath(id.Type) + "/" + escapeRepo(id)
	route := "/api/{type}s/{repo}"
	if revision != "" {
		p += "/revision/" + url.PathEscape(revision)
		route += "/revision/{rev}"
	}
	var raw json.RawMessage
	if _, err := c.getJSON(ctx, request{url: apiBase(u) + p + "?blobs=true", route: route, cred: cred, upName: u.Name}, &raw); err != nil {
		return nil, err
	}
	var h hubRepo
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, fmt.Errorf("decode upstream repo info: %w", err)
	}
	sha, err := model.ParseCommitSHA(h.SHA)
	if err != nil {
		return nil, fmt.Errorf("upstream repo info has no valid sha: %w", err)
	}
	info := &RepoInfo{ID: id, SHA: sha, Private: h.Private, Gated: string(h.Gated), Tags: h.Tags, PipelineTag: h.PipelineTag,
		LibraryName: h.LibraryName, Downloads: h.Downloads, Likes: h.Likes, LastModified: h.LastModified.Time, CardData: h.CardData, Raw: raw}
	if h.ID != "" {
		if got, err := model.ParseRepoID(id.Type, h.ID); err == nil {
			info.ID = got
		}
	}
	for _, s := range h.Siblings {
		sib := Sibling{Path: s.RFilename}
		if s.Size != nil {
			sib.Size = *s.Size
		}
		if s.LFS != nil && model.IsSHA256(s.LFS.SHA256) {
			sib.LFS = &model.LFSInfo{OID: s.LFS.SHA256, Size: s.LFS.Size, PointerSize: s.LFS.PointerSize}
			sib.Size = s.LFS.Size
		}
		info.Siblings = append(info.Siblings, sib)
	}
	return info, nil
}

// searchSorts maps Hangar's sort names to the Hub's.
var searchSorts = map[string]string{"trending": "trendingScore", "downloads": "downloads", "likes": "likes", "lastModified": "lastModified"}

// Search implements Client with GET /api/{type}s.
func (c *HTTPClient) Search(ctx context.Context, u Upstream, cred Credentials, q SearchQuery) (*SearchPage, error) {
	v := url.Values{}
	if q.Search != "" {
		v.Set("search", q.Search)
	}
	if q.Author != "" {
		v.Set("author", q.Author)
	}
	for _, f := range q.Filter {
		v.Add("filter", f)
	}
	if q.Sort != "" {
		s, ok := searchSorts[q.Sort]
		if !ok {
			return nil, fmt.Errorf("unknown sort %q: use trending, downloads, likes or lastModified", q.Sort)
		}
		v.Set("sort", s)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	v.Set("limit", strconv.Itoa(limit))
	if q.Cursor != "" {
		v.Set("cursor", q.Cursor)
	}
	for _, e := range []string{"sha", "private", "gated", "tags", "pipeline_tag", "library_name", "downloads", "likes", "lastModified"} {
		v.Add("expand", e)
	}
	var list []hubRepo
	hdr, err := c.getJSON(ctx, request{url: apiBase(u) + "/api/" + typePath(q.Type) + "?" + v.Encode(), route: "/api/{type}s", cred: cred, upName: u.Name}, &list)
	if err != nil {
		return nil, err
	}
	page := &SearchPage{}
	if next := linkNext(hdr); next != "" {
		if nu, err := url.Parse(next); err == nil {
			page.NextCursor = nu.Query().Get("cursor")
		}
	}
	for _, h := range list {
		id, err := model.ParseRepoID(q.Type, h.ID)
		if err != nil {
			continue
		}
		sum := RepoSummary{ID: id, Tags: h.Tags, PipelineTag: h.PipelineTag, LibraryName: h.LibraryName, Downloads: h.Downloads,
			Likes: h.Likes, LastModified: h.LastModified.Time, Private: h.Private, Gated: string(h.Gated)}
		if sha, err := model.ParseCommitSHA(h.SHA); err == nil {
			sum.SHA = sha
		}
		page.Repos = append(page.Repos, sum)
	}
	return page, nil
}

// linkNext returns the rel="next" target of a Link header.
func linkNext(h http.Header) string {
	for _, l := range h.Values("Link") {
		for _, part := range strings.Split(l, ",") {
			seg := strings.Split(part, ";")
			if len(seg) < 2 {
				continue
			}
			target := strings.Trim(strings.TrimSpace(seg[0]), "<>")
			for _, p := range seg[1:] {
				if strings.ReplaceAll(strings.TrimSpace(p), " ", "") == `rel="next"` {
					return target
				}
			}
		}
	}
	return ""
}

// sameOrigin reports whether target is on the Upstream's origin, so
// pagination links never send credentials elsewhere.
func sameOrigin(u Upstream, target string) bool {
	a, err1 := url.Parse(u.BaseURL)
	b, err2 := url.Parse(target)
	return err1 == nil && err2 == nil && strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

type hubTreeEntry struct {
	Type string `json:"type"`
	OID  string `json:"oid"`
	Size int64  `json:"size"`
	Path string `json:"path"`
	LFS  *struct {
		OID         string `json:"oid"`
		Size        int64  `json:"size"`
		PointerSize int    `json:"pointerSize"`
	} `json:"lfs"`
}

// maxTreePages bounds tree pagination (1,000 entries a page on the Hub).
const maxTreePages = 1000

// Tree implements Client with GET /api/{type}s/{repo}/tree/{rev}[/{path}],
// following Link pages on the Upstream's origin.
func (c *HTTPClient) Tree(ctx context.Context, u Upstream, cred Credentials, id model.RepoID, revision, path string, recursive bool) ([]model.Entry, error) {
	if revision == "" {
		revision = model.DefaultBranch
	}
	p := "/api/" + typePath(id.Type) + "/" + escapeRepo(id) + "/tree/" + url.PathEscape(revision)
	if path != "" {
		p += "/" + url.PathEscape(path)
	}
	next := apiBase(u) + p + "?recursive=" + strconv.FormatBool(recursive) + "&expand=false"
	var out []model.Entry
	for page := 0; next != ""; page++ {
		if page >= maxTreePages {
			return nil, fmt.Errorf("upstream tree has more than %d pages", maxTreePages)
		}
		var list []hubTreeEntry
		hdr, err := c.getJSON(ctx, request{url: next, route: "/api/{type}s/{repo}/tree/{rev}", cred: cred, upName: u.Name}, &list)
		if err != nil {
			return nil, err
		}
		for _, e := range list {
			clean, err := model.CleanRepoPath(e.Path)
			if err != nil || clean == "" {
				return nil, fmt.Errorf("upstream tree has an invalid path %q", e.Path)
			}
			en := model.Entry{Path: clean, OID: e.OID, Size: e.Size, Present: true}
			if e.Type == "file" {
				en.Type = model.EntryFile
			} else {
				en.Type = model.EntryDirectory
				en.Size = 0
			}
			if e.LFS != nil && model.IsSHA256(e.LFS.OID) {
				en.LFS = &model.LFSInfo{OID: e.LFS.OID, Size: e.LFS.Size, PointerSize: e.LFS.PointerSize}
				en.Size = e.LFS.Size
			}
			out = append(out, en)
		}
		next = linkNext(hdr)
		if next != "" && !sameOrigin(u, next) {
			return nil, errors.New("upstream tree pagination points at another origin")
		}
	}
	return out, nil
}

// Download implements Client with GET /{repo}/resolve/{commit}/{path},
// following redirects. File metadata comes from the first response that
// carries it, because CDN targets do not repeat the Hub's headers.
func (c *HTTPClient) Download(ctx context.Context, u Upstream, cred Credentials, id model.RepoID, commit model.CommitSHA, path string, rng *blob.Range) (io.ReadCloser, FileMeta, error) {
	target := apiBase(u) + "/" + id.Type.URLPrefix() + escapeRepo(id) + "/resolve/" + url.PathEscape(string(commit)) + "/" + escapeFilePath(path)
	h := http.Header{"Accept-Encoding": {"identity"}}
	if rng != nil {
		end := ""
		if rng.End >= 0 {
			end = strconv.FormatInt(rng.End, 10)
		}
		h.Set("Range", "bytes="+strconv.FormatInt(rng.Start, 10)+"-"+end)
	}
	resp, err := c.do(ctx, request{method: http.MethodGet, url: target, route: "/{repo}/resolve/{rev}/{path}", header: h, cred: cred, upName: u.Name, stream: true})
	if err != nil {
		return nil, FileMeta{}, err
	}
	meta := FileMeta{Commit: commit}
	for r := resp; r != nil; {
		if meta.ETag == "" {
			meta.ETag = strings.Trim(strings.TrimPrefix(firstHeader(r.Header, "X-Linked-Etag", "ETag"), "W/"), `"`)
		}
		if meta.Size == 0 {
			if s, err := strconv.ParseInt(r.Header.Get("X-Linked-Size"), 10, 64); err == nil {
				meta.Size = s
			}
		}
		if c := r.Header.Get("X-Repo-Commit"); c != "" {
			if sha, err := model.ParseCommitSHA(c); err == nil {
				meta.Commit = sha
			}
		}
		if r.Request == nil {
			break
		}
		r = r.Request.Response
	}
	if meta.Size == 0 {
		meta.Size = totalSize(resp)
	}
	if rng != nil && rng.Start > 0 && resp.StatusCode != http.StatusPartialContent {
		_ = resp.Body.Close()
		return nil, FileMeta{}, fmt.Errorf("%w: upstream ignored the Range request", ErrUnavailable)
	}
	meta.LFS = model.IsSHA256(meta.ETag)
	return &redactingBody{rc: resp.Body}, meta, nil
}

func firstHeader(h http.Header, keys ...string) string {
	for _, k := range keys {
		if v := h.Get(k); v != "" {
			return v
		}
	}
	return ""
}

// totalSize reads the whole object size from Content-Range or Content-Length.
func totalSize(resp *http.Response) int64 {
	if cr := resp.Header.Get("Content-Range"); cr != "" {
		if i := strings.LastIndex(cr, "/"); i >= 0 {
			if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
				return n
			}
		}
	}
	if resp.StatusCode == http.StatusOK && resp.ContentLength >= 0 {
		return resp.ContentLength
	}
	return 0
}

// redactingBody makes read errors safe to log.
type redactingBody struct{ rc io.ReadCloser }

func (b *redactingBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = fmt.Errorf("%w: read from upstream: %w", ErrUnavailable, redactErr(err))
	}
	return n, err
}

func (b *redactingBody) Close() error { return b.rc.Close() }

// Viewer implements Client with GET <viewer>/<endpoint>.
func (c *HTTPClient) Viewer(ctx context.Context, u Upstream, cred Credentials, endpoint string, params url.Values) (json.RawMessage, error) {
	base := u.DatasetViewerURL()
	if base == "" {
		return nil, fmt.Errorf("%w: the Upstream has no Dataset Viewer URL", ErrNotFound)
	}
	if !strings.HasPrefix(endpoint, "/") || strings.Contains(endpoint, "..") || strings.ContainsAny(endpoint, "?#") {
		return nil, fmt.Errorf("invalid viewer endpoint %q", endpoint)
	}
	target := base + endpoint
	if len(params) > 0 {
		target += "?" + params.Encode()
	}
	var raw json.RawMessage
	if _, err := c.getJSON(ctx, request{url: target, route: "viewer " + endpoint, cred: cred, upName: u.Name}, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}
