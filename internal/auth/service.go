package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/crypto/hkdf"

	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/config"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
	"github.com/jamesagarside/hangar/internal/org"
	"github.com/jamesagarside/hangar/internal/secrets"
	"github.com/jamesagarside/hangar/internal/telemetry"
)

// Options configures New.
type Options struct {
	// DB is the database. Required.
	DB *db.DB
	// Config is the server configuration. Required: security, oidc,
	// server.public_url and the Instance setting defaults come from it.
	Config *config.Config
	// Orgs manages Namespaces, Members and collaborators. Required.
	Orgs *org.DBService
	// Audit records audit events. Required.
	Audit audit.Recorder
	// Instruments, if set, counts authentication attempts.
	Instruments *telemetry.Instruments
	// Log receives operational messages. Nil discards them.
	Log *slog.Logger
	// Upstreams resolves Upstream names for upstream tokens. Nil means no
	// Upstream can be named, so saving an upstream token fails.
	Upstreams UpstreamLookup
	// HTTPClient is used for OIDC discovery and token exchange. Nil means
	// http.DefaultClient.
	HTTPClient *http.Client
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time
}

// UpstreamLookup finds an Upstream's ID by its name (such as
// "huggingface"). internal/upstream implements it; it returns an error
// wrapping ErrNotFound for unknown names.
type UpstreamLookup interface {
	UpstreamID(ctx context.Context, name string) (uuid.UUID, error)
}

// Service is the database-backed implementation of every interface in this
// package. Its fields expose the parts each caller needs.
type Service struct {
	db       *db.DB
	cfg      *config.Config
	orgs     *org.DBService
	audit    audit.Recorder
	inst     *telemetry.Instruments
	log      *slog.Logger
	now      func() time.Time
	sealer   *secrets.Sealer
	jwtKey   []byte
	csrfKey  []byte
	client   *http.Client
	upstream UpstreamLookup
	// origins refuses cross-origin requests that carry browser credentials.
	origins *originGuard
	// viewerHost is datasets-server.<public host>, for httpx.Classify.
	viewerHost string

	// Settings reads and changes Instance settings.
	Settings *SettingsStore
	// Users manages Users and local accounts.
	Users *UserStore
	// Sessions manages UI sessions.
	Sessions *SessionStore
	// TokenStore manages API and git Tokens; it implements Tokens.
	TokenStore *TokenStore
	// Authorizer decides permissions; it implements Authz.
	Authorizer *Authorizer
	// Device runs device-code logins; it implements DeviceFlow.
	Device *DeviceStore
	// Ollama manages Ollama keys and registry tokens; it implements OllamaAuth.
	Ollama *OllamaStore
	// UpstreamTokens stores users' tokens for Upstreams.
	UpstreamTokens *UpstreamTokenStore
	// AccessRequests manages gated Repo access requests (REPO-8).
	AccessRequests *AccessRequestStore
	// Login runs password sign-in with throttling.
	Login *LoginService
	// OIDC runs OpenID Connect sign-in.
	OIDC *OIDCService
}

var (
	_ Authenticator       = (*Service)(nil)
	_ httpx.Authenticator = (*Service)(nil)
)

// New builds the Service. It does not contact OIDC providers; discovery
// happens on first use.
func New(opts Options) (*Service, error) {
	if opts.DB == nil || opts.Config == nil || opts.Orgs == nil || opts.Audit == nil {
		return nil, errors.New("auth: DB, Config, Orgs and Audit are required")
	}
	key, err := opts.Config.InstanceKeyBytes()
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	sealer, err := secrets.New(key)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	s := &Service{
		db: opts.DB, cfg: opts.Config, orgs: opts.Orgs, audit: opts.Audit, inst: opts.Instruments,
		log: opts.Log, now: opts.Now, sealer: sealer, client: opts.HTTPClient, upstream: opts.Upstreams,
		jwtKey: deriveKey(key, "hangar ollama registry jwt v1"), csrfKey: deriveKey(key, "hangar csrf v1"),
	}
	if s.origins, err = newOriginGuard(opts.Config.Server.PublicURL); err != nil {
		return nil, err
	}
	if u, perr := url.Parse(opts.Config.Server.PublicURL); perr == nil && u.Host != "" {
		s.viewerHost = "datasets-server." + u.Host
	}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.client == nil {
		s.client = http.DefaultClient
	}
	s.Settings = &SettingsStore{s: s}
	s.Users = &UserStore{s: s}
	s.Sessions = &SessionStore{s: s}
	s.TokenStore = &TokenStore{s: s}
	s.Authorizer = &Authorizer{s: s}
	s.Device = &DeviceStore{s: s}
	s.Ollama = &OllamaStore{s: s}
	s.UpstreamTokens = &UpstreamTokenStore{s: s}
	s.AccessRequests = &AccessRequestStore{s: s}
	s.Login = &LoginService{s: s}
	oidcSvc, err := newOIDCService(s)
	if err != nil {
		return nil, err
	}
	s.OIDC = oidcSvc
	return s, nil
}

// Tokens returns the Tokens implementation.
func (s *Service) Tokens() Tokens { return s.TokenStore }

// Authz returns the Authz implementation.
func (s *Service) Authz() Authz { return s.Authorizer }

// DeviceFlow returns the DeviceFlow implementation.
func (s *Service) DeviceFlow() DeviceFlow { return s.Device }

// OllamaAuth returns the OllamaAuth implementation.
func (s *Service) OllamaAuth() OllamaAuth { return s.Ollama }

// Config returns the configuration the Service was built with.
func (s *Service) Config() *config.Config { return s.cfg }

// nowUTC returns the current time in UTC at the precision both databases store.
func (s *Service) nowUTC() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// record writes an audit event, logging (never failing the request) when
// the audit store is unavailable.
func (s *Service) record(ctx context.Context, e audit.Event) {
	if err := s.audit.Record(ctx, e); err != nil {
		s.log.ErrorContext(ctx, "record audit event", "action", string(e.Action), "error", err)
	}
}

// countAttempt adds to the hangar.auth.attempts metric.
func (s *Service) countAttempt(ctx context.Context, method model.AuthMethod, outcome string) {
	if s.inst == nil || s.inst.AuthAttempts == nil {
		return
	}
	s.inst.AuthAttempts.Add(ctx, 1, metric.WithAttributes(
		attribute.String(telemetry.MetricMethod, string(method)),
		attribute.String(telemetry.MetricOutcome, outcome)))
}

func deriveKey(master []byte, info string) []byte {
	out := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, master, nil, []byte(info)), out); err != nil {
		panic(err) // HKDF-SHA256 cannot fail for 32 bytes.
	}
	return out
}

// hashSecret returns the hex SHA-256 of a secret, as stored for Tokens,
// sessions, invites, resets and device codes.
func hashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// randomString returns n random bytes as unpadded base64url.
func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// TokenPrefix starts every Token secret.
const TokenPrefix = "hgr_"

// NewTokenSecret returns a Token secret: hgr_ and 40 base62 characters.
func NewTokenSecret() (string, error) {
	var b strings.Builder
	b.WriteString(TokenPrefix)
	limit := big.NewInt(int64(len(base62Alphabet)))
	for range 40 {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		b.WriteByte(base62Alphabet[n.Int64()])
	}
	return b.String(), nil
}

// LooksLikeToken reports whether s has the form of a Token secret.
func LooksLikeToken(s string) bool {
	if len(s) != len(TokenPrefix)+40 || !strings.HasPrefix(s, TokenPrefix) {
		return false
	}
	for _, c := range s[len(TokenPrefix):] {
		if !strings.ContainsRune(base62Alphabet, c) {
			return false
		}
	}
	return true
}

// publicURL returns server.public_url without a trailing slash.
func (s *Service) publicURL() string { return strings.TrimRight(s.cfg.Server.PublicURL, "/") }

// secureCookies reports whether cookies get the Secure attribute: when
// public_url is HTTPS.
func (s *Service) secureCookies() bool {
	return strings.HasPrefix(strings.ToLower(s.cfg.Server.PublicURL), "https://")
}
