// Package gitserver serves git smart HTTP and the git-lfs API
// (docs/spec/04-git.md, docs/research/git-server-research.md).
//
// Fetches and clones run the real `git upload-pack --stateless-rpc` on a
// temporary view of the Repo that repo.Service.OpenView materialises.
// Pushes run go-git's receive-pack into a private quarantine directory whose
// alternates point at the view, with refs held in memory. A pre-receive hook
// checks the push policy and then hands the received pack and the ref
// updates to repo.Service.ApplyPack, which stores the pack and moves every
// ref in one compare-and-swap transaction. The git server never writes
// Commits or refs itself.
//
// The git-lfs API answers batch requests with presigned Blob store URLs where
// the store supports them, and otherwise with Hangar URLs that carry a signed
// token, because Hugging Face clients send no Authorization header on part
// uploads and completions. Locking is not supported: every /locks endpoint
// answers 404, which makes git-lfs quietly turn lock verification off.
package gitserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/repo"
	"github.com/jamesagarside/hangar/internal/scan"
	"github.com/jamesagarside/hangar/internal/telemetry"
)

// Realm is the HTTP Basic realm in authentication challenges.
const Realm = "Hangar"

// Config holds the settings the git server reads from the Instance
// configuration. ConfigFrom fills it from config.Config.
type Config struct {
	// PublicURL is server.public_url; LFS action URLs are built on it.
	PublicURL string
	// MaxBlobSize is the largest non-LFS file a push may add (git.max_blob_size).
	MaxBlobSize int64
	// MaxPackSize is the largest pack a push may send (git.max_pack_size).
	MaxPackSize int64
	// AllowForcePush lets anyone with write access force-push the default
	// branch (git.allow_force_push). Namespace admins always can.
	AllowForcePush bool
	// Timeout is the wall-clock and CPU limit of one git subprocess (git.timeout).
	Timeout time.Duration
	// MultipartThreshold is the object size above which clients that offer
	// the multipart transfer get one (lfs.multipart_threshold).
	MultipartThreshold int64
	// UploadURLTTL is the lifetime of signed and presigned upload URLs
	// (lfs.upload_url_ttl).
	UploadURLTTL time.Duration
	// VerifyWait is how long a push waits for an LFS object that is still
	// being verified (lfs.verify_wait).
	VerifyWait time.Duration
	// DownloadMode is storage.s3.download_mode: redirect hands out presigned
	// URLs where the Blob store supports them; proxy always streams through
	// Hangar.
	DownloadMode string
	// DownloadURLTTL is the lifetime of LFS download URLs (storage.s3.presign_ttl).
	DownloadURLTTL time.Duration
	// PartSize is the multipart part size. Zero means
	// max(16 MiB, ceil(size/10000)) rounded up to a MiB, which keeps every
	// object within S3's 10,000-part limit.
	PartSize int64
	// GitBinary is the git executable. Empty means "git" on PATH.
	GitBinary string
	// TempDir holds push quarantines and the empty HOME of git
	// subprocesses. Empty means the system temporary directory.
	TempDir string
}

// ConfigFrom builds a Config from the Instance configuration.
func ConfigFrom(c *config.Config) Config {
	return Config{
		PublicURL:          c.Server.PublicURL,
		MaxBlobSize:        c.Git.MaxBlobSize.Int64(),
		MaxPackSize:        c.Git.MaxPackSize.Int64(),
		AllowForcePush:     c.Git.AllowForcePush,
		Timeout:            c.Git.Timeout,
		MultipartThreshold: c.LFS.MultipartThreshold.Int64(),
		UploadURLTTL:       c.LFS.UploadURLTTL,
		VerifyWait:         c.LFS.VerifyWait,
		DownloadMode:       c.Storage.S3.DownloadMode,
		DownloadURLTTL:     c.Storage.S3.PresignTTL,
	}
}

// QuotaChecker enforces Namespace storage quotas (OPS-4). CheckQuota returns
// an error, whose message is shown to the client, when adding bytes to the
// Namespace would exceed its quota.
type QuotaChecker interface {
	CheckQuota(ctx context.Context, namespaceID uuid.UUID, addBytes int64) error
}

// PullThrough starts Pull-through transfers of LFS objects that a Partial
// mirror references but does not hold (PULL-10, docs/spec/04-git.md
// "Batch"). DownloadURL returns a URL that authorises itself and streams the
// object as it arrives, or ok false when Pull-through is off or the caller
// may not use it.
type PullThrough interface {
	DownloadURL(ctx context.Context, p model.Principal, r *repo.Repo, oid string, size int64) (href string, ok bool, err error)
}

// Deps are the services the git server uses.
type Deps struct {
	Repos  repo.Service
	LFS    repo.LFSStore
	Authz  auth.Authz
	Signer *httpx.Signer
	// Quota is optional; without it no quota is enforced.
	Quota QuotaChecker
	// PullThrough is optional; without it objects that are not in the
	// Arsenal get a 404 with instructions.
	PullThrough PullThrough
	// Downloads enforces scan.block_unsafe_downloads on LFS downloads. It
	// is optional; without it every object downloads.
	Downloads *scan.Gate
	// Log defaults to slog.Default().
	Log *slog.Logger
	// Tracer defaults to the global tracer provider.
	Tracer trace.Tracer
	// Instruments is optional; without it no metrics are recorded.
	Instruments *telemetry.Instruments
}

// Server serves git smart HTTP and the git-lfs API.
type Server struct {
	cfg  Config
	deps Deps
	log  *slog.Logger
	home string
	now  func() time.Time
}

// New returns a Server. It creates an empty directory to use as HOME for git
// subprocesses, so no user configuration reaches them.
func New(cfg Config, deps Deps) (*Server, error) {
	if deps.Repos == nil || deps.LFS == nil || deps.Authz == nil || deps.Signer == nil {
		return nil, errors.New("gitserver: Repos, LFS, Authz and Signer are required")
	}
	if cfg.PublicURL == "" {
		return nil, errors.New("gitserver: the public URL is required")
	}
	if cfg.GitBinary == "" {
		cfg.GitBinary = "git"
	}
	if cfg.TempDir == "" {
		cfg.TempDir = os.TempDir()
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = time.Hour
	}
	if cfg.UploadURLTTL <= 0 {
		cfg.UploadURLTTL = 24 * time.Hour
	}
	if cfg.DownloadURLTTL <= 0 {
		cfg.DownloadURLTTL = time.Hour
	}
	if cfg.DownloadMode == "" {
		cfg.DownloadMode = config.DownloadRedir
	}
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	if deps.Tracer == nil {
		deps.Tracer = otel.GetTracerProvider().Tracer(telemetry.ScopeName)
	}
	if err := os.MkdirAll(cfg.TempDir, 0o700); err != nil {
		return nil, fmt.Errorf("gitserver: create temporary directory: %w", err)
	}
	home, err := os.MkdirTemp(cfg.TempDir, "hangar-git-home-")
	if err != nil {
		return nil, fmt.Errorf("gitserver: create git home: %w", err)
	}
	return &Server{cfg: cfg, deps: deps, log: deps.Log, home: home, now: time.Now}, nil
}

// Close removes the Server's temporary HOME directory.
func (s *Server) Close() error { return os.RemoveAll(s.home) }

// Git returns the handler for git smart HTTP, mounted as httpx.Protocols.Git.
func (s *Server) Git() http.Handler { return http.HandlerFunc(s.serveGit) }

// LFS returns the handler for the git-lfs API, mounted as httpx.Protocols.LFS.
func (s *Server) LFS() http.Handler { return http.HandlerFunc(s.serveLFS) }

// repoPath is a request path split into the Repo ID and what follows it.
type repoPath struct {
	ID model.RepoID
	// Rest holds the unescaped segments after `ns/name[.git]`.
	Rest []string
}

var errBadPath = errors.New("not a repo path")

// parseRepoPath splits `/[datasets/]ns/name[.git]/rest...`. Segments are
// unescaped one by one, so an escaped slash cannot change the structure.
func parseRepoPath(escaped string) (repoPath, error) {
	segs, err := httpx.SplitEscapedPath(escaped)
	if err != nil {
		return repoPath{}, errBadPath
	}
	typ := model.RepoTypeModel
	if len(segs) > 0 && segs[0] == "datasets" {
		typ = model.RepoTypeDataset
		segs = segs[1:]
	}
	if len(segs) < 2 {
		return repoPath{}, errBadPath
	}
	name := segs[1]
	if trimmed, ok := strings.CutSuffix(name, ".git"); ok {
		name = trimmed
	}
	id := model.RepoID{Type: typ, Namespace: segs[0], Name: name}
	if err := id.Validate(); err != nil {
		return repoPath{}, errBadPath
	}
	for _, s := range segs[2:] {
		if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\\") {
			return repoPath{}, errBadPath
		}
	}
	return repoPath{ID: id, Rest: segs[2:]}, nil
}

// routePrefix is the route template of a Repo path, for logs and spans.
func routePrefix(id model.RepoID) string {
	if id.Type == model.RepoTypeDataset {
		return "/datasets/{ns}/{name}"
	}
	return "/{ns}/{name}"
}

// target builds the permission-check target of a Repo.
func target(r *repo.Repo) auth.Target {
	return auth.Target{Repo: &auth.RepoTarget{
		ID:          r.ID,
		UUID:        r.UUID,
		NamespaceID: r.NamespaceID,
		Visibility:  r.Visibility,
		Gated:       r.Gating.Enabled(),
		Mirror:      r.Mirror,
	}}
}

// accessError is a refusal to serve a Repo, already mapped to an HTTP status.
type accessError struct {
	status  int
	message string
}

func (e *accessError) Error() string { return e.message }

func notFoundMessage(id model.RepoID) string {
	return "Repository " + id.Path() + " not found. If it is private, check that your token can read it."
}

// lookup finds a Repo and checks that the caller may perform action on it.
// Callers without credentials get 401, so git and git-lfs ask for them,
// whether or not the Repo exists; callers with credentials and no read
// access get 404, so private names do not leak (docs/spec/05-auth.md).
func (s *Server) lookup(ctx context.Context, id model.RepoID, action auth.Action) (*repo.Repo, error) {
	p := httpx.Principal(ctx)
	r, err := s.deps.Repos.Get(ctx, id)
	if err != nil {
		if errors.Is(err, repo.ErrNotFound) {
			if p.IsAnonymous() {
				return nil, &accessError{status: http.StatusUnauthorized, message: "Authentication required."}
			}
			return nil, &accessError{status: http.StatusNotFound, message: notFoundMessage(id)}
		}
		return nil, err
	}
	httpx.SetRepo(ctx, r.ID)
	if action == auth.ActionWrite && p.IsAnonymous() {
		return nil, &accessError{status: http.StatusUnauthorized, message: "Authentication required to push. Use a Hangar token with write scope as the password."}
	}
	if err := s.deps.Authz.Can(ctx, p, action, target(r)); err != nil {
		switch {
		case errors.Is(err, auth.ErrNotFound):
			if p.IsAnonymous() {
				return nil, &accessError{status: http.StatusUnauthorized, message: "Authentication required."}
			}
			return nil, &accessError{status: http.StatusNotFound, message: notFoundMessage(id)}
		case errors.Is(err, auth.ErrGated):
			if p.IsAnonymous() {
				return nil, &accessError{status: http.StatusUnauthorized, message: "Authentication required."}
			}
			return nil, &accessError{status: http.StatusForbidden, message: "Access to " + r.ID.Path() + " is restricted. Request access on the Repo page."}
		case errors.Is(err, auth.ErrForbidden):
			if action == auth.ActionWrite {
				return nil, &accessError{status: http.StatusForbidden, message: "You do not have write access to " + r.ID.Path() + ". Check that your token has write scope and covers this Repo."}
			}
			return nil, &accessError{status: http.StatusForbidden, message: "You do not have access to " + r.ID.Path() + "."}
		}
		return nil, err
	}
	return r, nil
}

// canForcePush reports whether the caller holds the admin Role on the
// Repo, which lets them force-push the default branch. The push itself has
// already passed the write check with the caller's real Token; this asks
// only about the Role, so an admin pushing with a write-scope Token, the
// usual git credential, counts as an admin. Token Repo and Namespace
// restrictions still apply.
func (s *Server) canForcePush(ctx context.Context, r *repo.Repo) bool {
	p := httpx.Principal(ctx)
	if p.Token != nil {
		tok := *p.Token
		tok.Scope = model.ScopeAdmin
		p.Token = &tok
	}
	return s.deps.Authz.Can(ctx, p, auth.ActionAdmin, target(r)) == nil
}

// tempDir creates a private temporary directory under the configured TempDir.
func (s *Server) tempDir(pattern string) (string, error) {
	return os.MkdirTemp(s.cfg.TempDir, pattern)
}

// absPath returns path made absolute and with symbolic links resolved where
// possible, so alternates files hold stable paths.
func absPath(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return path
}
