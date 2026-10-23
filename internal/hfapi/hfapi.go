// Package hfapi serves the Hugging Face Hub HTTP API: file downloads
// (`resolve`), Repo info, trees, refs, Commits, uploads (preupload and the
// NDJSON commit endpoint), Repo management, discussions and pull requests,
// likes, Collections, device-code login, the Xet token endpoints and the
// dataset Parquet listing. Real clients (`huggingface_hub`, the `hf` CLI,
// `transformers`, `datasets`) use it unmodified with HF_ENDPOINT pointing at
// Hangar.
//
// The wire contract is docs/research/hf-hub-api-contract.md, and Hangar's
// decisions on top of it are in docs/spec/03-hf-api.md. Handlers never
// touch the database: they go through the service interfaces in Deps.
package hfapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/hangar-hub/hangar/internal/auth"
	"github.com/hangar-hub/hangar/internal/collections"
	"github.com/hangar-hub/hangar/internal/community"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/datasets"
	"github.com/hangar-hub/hangar/internal/httpx"
	"github.com/hangar-hub/hangar/internal/index"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/org"
	"github.com/hangar-hub/hangar/internal/repo"
	"github.com/hangar-hub/hangar/internal/scan"
	"github.com/hangar-hub/hangar/internal/telemetry"
	"github.com/hangar-hub/hangar/internal/xet"
)

// Settings are the configuration values the Hub API reads. FromConfig
// builds them from the Instance configuration.
type Settings struct {
	// PublicURL is server.public_url. Link headers, Repo URLs and Commit
	// URLs are absolute on it.
	PublicURL string
	// RegularUploadMax is hfapi.regular_upload_max: preupload answers
	// `regular` below it unless .gitattributes stores the path in LFS.
	RegularUploadMax int64
	// XetReject is true when hfapi.xet is `reject`.
	XetReject bool
	// DefaultNamespace is hfapi.default_namespace, the target of
	// single-segment Repo ID redirects. Empty turns them off.
	DefaultNamespace string
	// RedirectDownloads is true when LFS downloads redirect to presigned
	// Blob store URLs (storage.backend s3 with download_mode redirect).
	RedirectDownloads bool
	// PresignTTL is the lifetime of presigned download URLs.
	PresignTTL time.Duration
	// CommitLineMax caps one line of an NDJSON commit body. Zero means
	// twice RegularUploadMax plus 1 MiB, enough for a base64 regular file.
	CommitLineMax int64
	// CommitInlineMax is hfapi.commit_inline_max: the most bytes of regular
	// file content one NDJSON commit may carry. Zero means 1 GiB.
	CommitInlineMax int64
	// BodyMax caps buffered JSON request bodies (server.max_request_body).
	BodyMax int64
	// VerifyWait is lfs.verify_wait: how long a create_pr=1 commit waits
	// for an LFS object that is still being verified before it opens the
	// pull request. Zero means 10 minutes.
	VerifyWait time.Duration
}

// FromConfig returns the Hub API settings of an Instance configuration.
func FromConfig(c *config.Config) Settings {
	return Settings{
		PublicURL:         strings.TrimRight(c.Server.PublicURL, "/"),
		RegularUploadMax:  c.HFAPI.RegularUploadMax.Int64(),
		CommitInlineMax:   c.HFAPI.CommitInlineMax.Int64(),
		XetReject:         c.HFAPI.Xet == config.XetReject,
		DefaultNamespace:  c.HFAPI.DefaultNamespace,
		RedirectDownloads: c.Storage.Backend == config.BackendS3 && c.Storage.S3.DownloadMode == config.DownloadRedir,
		PresignTTL:        c.Storage.S3.PresignTTL,
		BodyMax:           c.Server.MaxRequestBody.Int64(),
		VerifyWait:        c.LFS.VerifyWait,
	}
}

// PullThrough starts Pulls on demand for the Hub API (PULL-10). It is
// optional: a nil PullThrough turns Pull-through off.
type PullThrough interface {
	// Repo starts or joins a history-only Pull of a Repo that is not in the
	// Arsenal, waits for planning and returns the new Repo. It returns
	// repo.ErrNotFound when Pull-through does not apply (turned off, not in
	// the allowlist, not allowed for the caller, or missing upstream), and
	// ErrPullPending when planning has not finished in time. The Hub API
	// also calls it for a Mirror the caller can read whose Revision does
	// not resolve, since planning creates the Mirror before it stores the
	// history; for an existing Repo it waits the same way while the Mirror
	// has no history, and otherwise returns the Repo as it is.
	Repo(ctx context.Context, p model.Principal, id model.RepoID) (*repo.Repo, error)
	// File starts or joins the transfer of a file that is not present in a
	// Partial mirror and streams its bytes as they arrive. It returns
	// repo.ErrNotPresent when Pull-through does not apply.
	File(ctx context.Context, p model.Principal, r *repo.Repo, commit model.CommitSHA, e model.Entry) (io.ReadCloser, error)
}

// ErrPullPending means a Pull-through has not finished planning; the Hub
// API answers 503 with Retry-After and the client retries.
var ErrPullPending = errors.New("the pull has not finished planning yet")

// Deps are the services the Hub API uses. Every field except PullThrough,
// Instruments, Log and Now is required. The Hub API records no audit events
// and emits no webhooks itself: repo.Service does for the Repo changes it
// makes, and community.Service for discussions.
type Deps struct {
	Settings    Settings
	Repos       repo.Service
	LFS         repo.LFSStore
	Authz       auth.Authz
	Orgs        org.Service
	Community   community.Service
	Collections collections.Service
	Index       index.Indexer
	Scan        scan.Scanner
	Xet         xet.Ingestor
	Viewer      datasets.Viewer
	Devices     auth.DeviceFlow
	PullThrough PullThrough
	// BlockUnsafe reads the Instance setting scan.block_unsafe_downloads
	// (07-indexing-and-scanning.md). When it is true, a GET of a file the
	// scan marked unsafe answers 403 unless the request has accept_risk=1.
	// Nil never blocks.
	BlockUnsafe func(ctx context.Context) (bool, error)
	// Downloads is the shared gate for that setting, so every protocol
	// decides alike. Nil builds one from BlockUnsafe and Scan.
	Downloads   *scan.Gate
	Instruments *telemetry.Instruments
	Log         *slog.Logger
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

// Handler is the Hub API. It implements http.Handler and is mounted as
// httpx.Protocols.Hub, which receives /api/..., the resolve paths and
// /oauth/device and /oauth/token.
type Handler struct {
	d         Deps
	log       *slog.Logger
	now       func() time.Time
	dataFiles dataFileCache
	// verifies shares LFS object verifications between requests.
	verifies singleflight.Group
}

// New returns the Hub API handler.
func New(d Deps) (*Handler, error) {
	var missing []string
	check := func(ok bool, name string) {
		if !ok {
			missing = append(missing, name)
		}
	}
	check(d.Repos != nil, "Repos")
	check(d.LFS != nil, "LFS")
	check(d.Authz != nil, "Authz")
	check(d.Orgs != nil, "Orgs")
	check(d.Community != nil, "Community")
	check(d.Collections != nil, "Collections")
	check(d.Index != nil, "Index")
	check(d.Scan != nil, "Scan")
	check(d.Xet != nil, "Xet")
	check(d.Viewer != nil, "Viewer")
	check(d.Devices != nil, "Devices")
	check(d.Settings.PublicURL != "", "Settings.PublicURL")
	if len(missing) > 0 {
		return nil, errors.New("hfapi: missing dependencies: " + strings.Join(missing, ", "))
	}
	d.Settings.PublicURL = strings.TrimRight(d.Settings.PublicURL, "/")
	if d.Settings.RegularUploadMax <= 0 {
		d.Settings.RegularUploadMax = 10 << 20
	}
	if d.Settings.CommitLineMax <= 0 {
		d.Settings.CommitLineMax = 2*d.Settings.RegularUploadMax + 1<<20
	}
	if d.Settings.CommitInlineMax <= 0 {
		d.Settings.CommitInlineMax = defaultCommitInlineMax
	}
	if d.Settings.BodyMax <= 0 {
		d.Settings.BodyMax = 64 << 20
	}
	if d.Settings.PresignTTL <= 0 {
		d.Settings.PresignTTL = time.Hour
	}
	if d.Settings.VerifyWait <= 0 {
		d.Settings.VerifyWait = defaultVerifyWait
	}
	if d.Downloads == nil {
		d.Downloads = &scan.Gate{Block: d.BlockUnsafe, Results: d.Scan.Results, Repos: d.Repos}
	}
	h := &Handler{d: d, log: d.Log, now: d.Now}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	if h.now == nil {
		h.now = time.Now
	}
	return h, nil
}

// ServeHTTP routes a Hub request on its escaped path, so revisions such as
// `refs%2Fpr%2F1` stay one segment (03-hf-api.md rule 1).
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p, err := parsePath(r)
	if err != nil {
		h.writeBadRequest(w, r, "The request path is not valid URL encoding.")
		return
	}
	if len(p.segs) == 0 {
		h.notFound(w, r)
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		// A read looks its Repo up once, not once per repo.Service call:
		// a download makes three or four (02-architecture.md "Scale
		// targets").
		r = r.WithContext(repo.WithReadCache(r.Context()))
	}
	switch p.segs[0] {
	case "api":
		h.serveAPI(w, r, p.shift(1))
	case "oauth":
		h.serveOAuth(w, r, p.shift(1))
	default:
		h.serveResolve(w, r, p)
	}
}

func (h *Handler) serveAPI(w http.ResponseWriter, r *http.Request, p reqPath) {
	if len(p.segs) == 0 {
		h.notFound(w, r)
		return
	}
	switch p.segs[0] {
	case "whoami-v2":
		h.route(w, r, "/api/whoami-v2", p.len() == 1, map[string]http.HandlerFunc{http.MethodGet: h.whoami})
	case "validate-yaml":
		h.route(w, r, "/api/validate-yaml", p.len() == 1, map[string]http.HandlerFunc{http.MethodPost: h.validateYAML})
	case "repos":
		h.serveRepos(w, r, p.shift(1))
	case "collections":
		h.serveCollections(w, r, p.shift(1))
	case "users":
		h.route(w, r, "/api/users/{user}/likes", p.len() == 3 && p.at(2) == "likes", map[string]http.HandlerFunc{
			http.MethodGet: func(w http.ResponseWriter, r *http.Request) { h.userLikes(w, r, p.at(1)) }})
	case "models", "datasets":
		t := model.RepoTypeModel
		if p.segs[0] == "datasets" {
			t = model.RepoTypeDataset
		}
		if p.len() == 1 {
			h.route(w, r, "/api/"+t.Plural(), true, map[string]http.HandlerFunc{http.MethodGet: func(w http.ResponseWriter, r *http.Request) { h.listRepos(w, r, t) }})
			return
		}
		h.serveRepoAPI(w, r, t, p.shift(1))
	default:
		h.notFound(w, r)
	}
}

// route dispatches on the method once a path has matched. ok false means
// the path did not match after all.
func (h *Handler) route(w http.ResponseWriter, r *http.Request, template string, ok bool, methods map[string]http.HandlerFunc) {
	if !ok {
		h.notFound(w, r)
		return
	}
	httpx.SetRoute(r.Context(), template)
	fn, found := methods[r.Method]
	if !found && r.Method == http.MethodHead {
		fn, found = methods[http.MethodGet]
	}
	if !found {
		allow := make([]string, 0, len(methods))
		for m := range methods {
			allow = append(allow, m)
		}
		w.Header().Set("Allow", strings.Join(allow, ", "))
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusMethodNotAllowed, Message: "Method " + r.Method + " is not allowed on this endpoint."})
		return
	}
	fn(w, r)
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	if httpx.Info(r.Context()) != nil && httpx.Info(r.Context()).Route() == "" {
		httpx.SetRoute(r.Context(), "/{hub-unknown}")
	}
	httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusNotFound, Message: "Not found."})
}

func (h *Handler) writeBadRequest(w http.ResponseWriter, r *http.Request, msg string) {
	httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusBadRequest, Message: msg})
}

func (h *Handler) writeForbidden(w http.ResponseWriter, r *http.Request, msg string) {
	if httpx.Principal(r.Context()).IsAnonymous() {
		httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusUnauthorized, Message: "You must be authenticated to do this. Pass a token with HF_TOKEN or `hf auth login`."})
		return
	}
	httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusForbidden, Message: msg})
}

// internalError logs err and answers 500 without details. A request the
// client closed is answered 499 and logged at debug level instead.
func (h *Handler) internalError(w http.ResponseWriter, r *http.Request, err error) {
	if httpx.ClientGone(r, err) {
		h.log.DebugContext(r.Context(), "client closed the request", "error", err.Error(), "route", routeOf(r))
		httpx.WriteHubError(w, r, &httpx.HubError{Status: httpx.StatusClientClosedRequest, Message: httpx.ClientClosedMessage})
		return
	}
	h.log.ErrorContext(r.Context(), "hub api request failed", "error", err, "route", routeOf(r))
	httpx.WriteHubError(w, r, &httpx.HubError{Status: http.StatusInternalServerError, Message: "Internal server error. The request ID is " + httpx.RequestID(r.Context()) + "."})
}

func routeOf(r *http.Request) string {
	if i := httpx.Info(r.Context()); i != nil {
		return i.Route()
	}
	return ""
}
