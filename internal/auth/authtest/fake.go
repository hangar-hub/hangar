// Package authtest provides fakes of the auth interfaces: an
// Authenticator over a map of credentials, an Authz that applies a
// simplified form of the permission table in docs/spec/05-auth.md, and
// in-memory Tokens, DeviceFlow and OllamaAuth.
package authtest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/auth"
	"github.com/jamesagarside/hangar/internal/model"
)

// SessionCookie is the session cookie name, as in 05-auth.md.
const SessionCookie = "hangar_session"

// Authenticator is a fake auth.Authenticator. Credentials map to Principals:
// a Bearer token, a Basic password (any username) or a session cookie value.
// Unknown credentials give auth.ErrInvalidCredentials.
type Authenticator struct {
	mu          sync.Mutex
	credentials map[string]model.Principal
}

var _ auth.Authenticator = (*Authenticator)(nil)

// NewAuthenticator returns an Authenticator with no credentials.
func NewAuthenticator() *Authenticator {
	return &Authenticator{credentials: map[string]model.Principal{}}
}

// Add makes secret authenticate as p, as a token, password or cookie.
func (a *Authenticator) Add(secret string, p model.Principal) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.credentials[secret] = p
}

// Remove forgets a secret, as a revocation would.
func (a *Authenticator) Remove(secret string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.credentials, secret)
}

// Authenticate implements auth.Authenticator.
func (a *Authenticator) Authenticate(r *http.Request) (model.Principal, error) {
	secret := ""
	if h := r.Header.Get("Authorization"); h != "" {
		if tok, ok := strings.CutPrefix(h, "Bearer "); ok {
			secret = strings.TrimSpace(tok)
		} else if _, pw, ok := r.BasicAuth(); ok {
			secret = pw
		} else {
			return model.Principal{}, fmt.Errorf("%w: unsupported Authorization scheme", auth.ErrInvalidCredentials)
		}
	} else if c, err := r.Cookie(SessionCookie); err == nil {
		secret = c.Value
	}
	if secret == "" {
		return model.Anonymous(), nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.credentials[secret]
	if !ok {
		return model.Principal{}, auth.ErrInvalidCredentials
	}
	return p, nil
}

type roleKey struct {
	user   uuid.UUID
	target uuid.UUID
}

// Authz is a fake auth.Authz. Roles come from Namespace memberships and Repo
// collaborators set with SetRole and SetCollaborator; a user is admin of the
// Namespace whose name equals their username; Instance admins can do
// anything.
type Authz struct {
	// AnonymousRead lets anonymous users read public Repos.
	AnonymousRead bool

	mu        sync.Mutex
	roles     map[roleKey]model.Role
	collabs   map[roleKey]model.Role
	approvals map[roleKey]bool
}

var _ auth.Authz = (*Authz)(nil)

// NewAuthz returns an Authz with no roles.
func NewAuthz() *Authz {
	return &Authz{roles: map[roleKey]model.Role{}, collabs: map[roleKey]model.Role{}, approvals: map[roleKey]bool{}}
}

// SetRole gives a user a Role in a Namespace.
func (a *Authz) SetRole(userID, namespaceID uuid.UUID, r model.Role) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.roles[roleKey{userID, namespaceID}] = r
}

// SetCollaborator gives a user a Role on one Repo.
func (a *Authz) SetCollaborator(userID, repoUUID uuid.UUID, r model.Role) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.collabs[roleKey{userID, repoUUID}] = r
}

// Approve grants a user access to a gated Repo.
func (a *Authz) Approve(userID, repoUUID uuid.UUID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.approvals[roleKey{userID, repoUUID}] = true
}

func (a *Authz) roleIn(p model.Principal, nsID uuid.UUID, nsName string, repoUUID uuid.UUID) model.Role {
	if p.IsAnonymous() {
		return ""
	}
	if p.IsAdmin || (nsName != "" && strings.EqualFold(nsName, p.Username)) {
		return model.RoleAdmin
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	best := a.roles[roleKey{p.UserID, nsID}]
	if c := a.collabs[roleKey{p.UserID, repoUUID}]; c.Includes(model.RoleRead) && !best.Includes(c) {
		best = c
	}
	return best
}

func scopeFor(action auth.Action) model.Scope {
	switch action {
	case auth.ActionWrite, auth.ActionPullMirror, auth.ActionDiscuss, auth.ActionCreateRepo:
		return model.ScopeWrite
	case auth.ActionAdmin, auth.ActionManageNamespace, auth.ActionInstanceAdmin:
		return model.ScopeAdmin
	default:
		return model.ScopeRead
	}
}

// Can implements auth.Authz.
func (a *Authz) Can(_ context.Context, p model.Principal, action auth.Action, t auth.Target) error {
	if !p.IsAnonymous() && !p.MaxScope().Includes(scopeFor(action)) {
		if t.Repo != nil && !a.readable(p, t.Repo) {
			return auth.ErrNotFound
		}
		return fmt.Errorf("%w: the token's scope does not allow %s", auth.ErrForbidden, action)
	}
	switch {
	case action == auth.ActionInstanceAdmin:
		if p.IsAdmin {
			return nil
		}
		return auth.ErrForbidden
	case t.Repo != nil:
		return a.canRepo(p, action, t.Repo)
	case t.Namespace != nil:
		role := a.roleIn(p, t.Namespace.ID, t.Namespace.Name, uuid.Nil)
		need := model.RoleWrite
		if action == auth.ActionManageNamespace || action == auth.ActionAdmin {
			need = model.RoleAdmin
		}
		if role.Includes(need) {
			return nil
		}
		return auth.ErrForbidden
	default:
		return fmt.Errorf("%w: no target", auth.ErrForbidden)
	}
}

func (a *Authz) readable(p model.Principal, r *auth.RepoTarget) bool {
	if !p.AllowsRepo(r.ID) {
		return false
	}
	switch r.Visibility {
	case model.VisibilityPublic:
		return a.AnonymousRead || !p.IsAnonymous()
	case model.VisibilityInternal:
		return !p.IsAnonymous()
	default:
		return a.roleIn(p, r.NamespaceID, r.ID.Namespace, r.UUID).Includes(model.RoleRead)
	}
}

func (a *Authz) canRepo(p model.Principal, action auth.Action, r *auth.RepoTarget) error {
	if !a.readable(p, r) {
		return auth.ErrNotFound
	}
	role := a.roleIn(p, r.NamespaceID, r.ID.Namespace, r.UUID)
	switch action {
	case auth.ActionRead:
		return nil
	case auth.ActionReadFiles:
		if !r.Gated || role.Includes(model.RoleWrite) {
			return nil
		}
		a.mu.Lock()
		ok := a.approvals[roleKey{p.UserID, r.UUID}]
		a.mu.Unlock()
		if ok && !p.IsAnonymous() {
			return nil
		}
		return auth.ErrGated
	case auth.ActionDiscuss:
		if p.IsAnonymous() {
			return auth.ErrForbidden
		}
		return nil
	case auth.ActionWrite, auth.ActionPullMirror:
		if role.Includes(model.RoleWrite) {
			return nil
		}
	case auth.ActionAdmin:
		if role.Includes(model.RoleAdmin) {
			return nil
		}
	}
	return auth.ErrForbidden
}

// Tokens is a fake auth.Tokens. When Authenticator is set, created Tokens
// authenticate through it and revoked ones stop working.
type Tokens struct {
	Authenticator *Authenticator
	// Principals maps user IDs to the Principal a new Token authenticates as.
	Principals map[uuid.UUID]model.Principal

	mu      sync.Mutex
	tokens  map[uuid.UUID]*auth.Token
	secrets map[uuid.UUID]string
}

var _ auth.Tokens = (*Tokens)(nil)

// NewTokens returns an empty Tokens fake linked to authn (which may be nil).
func NewTokens(authn *Authenticator) *Tokens {
	return &Tokens{Authenticator: authn, Principals: map[uuid.UUID]model.Principal{}, tokens: map[uuid.UUID]*auth.Token{}, secrets: map[uuid.UUID]string{}}
}

const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// NewSecret returns a Token secret in Hangar's format: hgr_ and 40 base62 characters.
func NewSecret() (string, error) {
	var b strings.Builder
	b.WriteString("hgr_")
	for range 40 {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(base62))))
		if err != nil {
			return "", err
		}
		b.WriteByte(base62[n.Int64()])
	}
	return b.String(), nil
}

// Create implements auth.Tokens.
func (t *Tokens) Create(_ context.Context, nt auth.NewToken) (*auth.Token, string, error) {
	if nt.Name == "" || !nt.Scope.Valid() {
		return nil, "", fmt.Errorf("a token needs a name and a read, write or admin scope")
	}
	secret, err := NewSecret()
	if err != nil {
		return nil, "", err
	}
	tok := &auth.Token{ID: uuid.Must(uuid.NewV7()), UserID: nt.UserID, Name: nt.Name, Prefix: secret[:8], Scope: nt.Scope,
		Restrictions: nt.Restrictions, ExpiresAt: nt.ExpiresAt, CreatedAt: time.Now().UTC()}
	t.mu.Lock()
	p, ok := t.Principals[nt.UserID]
	if ok {
		tok.Username = p.Username
	}
	t.tokens[tok.ID] = tok
	t.secrets[tok.ID] = secret
	t.mu.Unlock()
	if t.Authenticator != nil && ok {
		p.Method = model.AuthToken
		p.Token = &model.TokenInfo{ID: tok.ID, Name: tok.Name, Scope: tok.Scope, Restrictions: tok.Restrictions}
		t.Authenticator.Add(secret, p)
	}
	out := *tok
	return &out, secret, nil
}

// List implements auth.Tokens.
func (t *Tokens) List(_ context.Context, userID uuid.UUID) ([]auth.Token, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []auth.Token
	for _, tok := range t.tokens {
		if userID == uuid.Nil || tok.UserID == userID {
			out = append(out, *tok)
		}
	}
	slices.SortFunc(out, func(a, b auth.Token) int { return strings.Compare(a.ID.String(), b.ID.String()) })
	return out, nil
}

// Revoke implements auth.Tokens. Users may revoke their own Tokens; Instance
// admins may revoke any.
func (t *Tokens) Revoke(_ context.Context, id uuid.UUID, actor model.Principal) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	tok, ok := t.tokens[id]
	if !ok {
		return auth.ErrNotFound
	}
	if !actor.IsAdmin && actor.UserID != tok.UserID {
		return auth.ErrNotFound
	}
	if t.Authenticator != nil {
		t.Authenticator.Remove(t.secrets[id])
	}
	delete(t.tokens, id)
	delete(t.secrets, id)
	return nil
}

type device struct {
	code     auth.DeviceCode
	status   string
	user     model.Principal
	secret   string
	expires  time.Time
	clientID string
}

// DeviceFlow is a fake auth.DeviceFlow that creates Tokens through Tokens.
type DeviceFlow struct {
	Tokens  *Tokens
	BaseURL string

	mu     sync.Mutex
	byCode map[string]*device
	byUser map[string]*device
	now    func() time.Time
}

var _ auth.DeviceFlow = (*DeviceFlow)(nil)

// NewDeviceFlow returns a DeviceFlow issuing Tokens through tokens.
func NewDeviceFlow(tokens *Tokens, baseURL string) *DeviceFlow {
	return &DeviceFlow{Tokens: tokens, BaseURL: baseURL, byCode: map[string]*device{}, byUser: map[string]*device{}, now: time.Now}
}

// SetClock makes the fake use now for expiry.
func (d *DeviceFlow) SetClock(now func() time.Time) { d.mu.Lock(); d.now = now; d.mu.Unlock() }

func randomCode(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return strings.ToUpper(base64.RawURLEncoding.EncodeToString(b))[:n]
}

// Start implements auth.DeviceFlow.
func (d *DeviceFlow) Start(_ context.Context, clientID string) (*auth.DeviceCode, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	code := auth.DeviceCode{
		DeviceCode: uuid.NewString(), UserCode: randomCode(4) + "-" + randomCode(4),
		VerificationURI: strings.TrimRight(d.BaseURL, "/") + "/settings/device",
		Interval:        5 * time.Second, ExpiresIn: 15 * time.Minute,
	}
	code.VerificationURIComplete = code.VerificationURI + "?code=" + code.UserCode
	dev := &device{code: code, status: "pending", expires: d.now().Add(code.ExpiresIn), clientID: clientID}
	d.byCode[code.DeviceCode] = dev
	d.byUser[code.UserCode] = dev
	out := code
	return &out, nil
}

// Approve implements auth.DeviceFlow.
func (d *DeviceFlow) Approve(ctx context.Context, userCode string, p model.Principal) error {
	if p.IsAnonymous() {
		return auth.ErrForbidden
	}
	d.mu.Lock()
	dev, ok := d.byUser[userCode]
	if !ok || dev.status != "pending" || d.now().After(dev.expires) {
		d.mu.Unlock()
		return auth.ErrNotFound
	}
	d.mu.Unlock()
	name := "device login"
	if dev.clientID != "" {
		name = dev.clientID
	}
	_, secret, err := d.Tokens.Create(ctx, auth.NewToken{UserID: p.UserID, Name: name, Scope: model.ScopeWrite})
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	dev.status, dev.user, dev.secret = "approved", p, secret
	return nil
}

// Deny implements auth.DeviceFlow.
func (d *DeviceFlow) Deny(_ context.Context, userCode string, _ model.Principal) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	dev, ok := d.byUser[userCode]
	if !ok || dev.status != "pending" {
		return auth.ErrNotFound
	}
	dev.status = "denied"
	return nil
}

// Poll implements auth.DeviceFlow.
func (d *DeviceFlow) Poll(_ context.Context, deviceCode string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	dev, ok := d.byCode[deviceCode]
	if !ok {
		return "", auth.ErrExpiredToken
	}
	switch {
	case dev.status == "approved":
		dev.status = "used"
		return dev.secret, nil
	case dev.status == "denied":
		return "", auth.ErrAccessDenied
	case dev.status == "used" || d.now().After(dev.expires):
		return "", auth.ErrExpiredToken
	default:
		return "", auth.ErrAuthorizationPending
	}
}

// OllamaAuth is a fake auth.OllamaAuth. It accepts an Authorization value
// registered with AddKey for the given user, and issues a registry token
// that authenticates through Authenticator as that user, limited to the Repo.
type OllamaAuth struct {
	Authenticator *Authenticator

	mu   sync.Mutex
	keys map[string]model.Principal
}

var _ auth.OllamaAuth = (*OllamaAuth)(nil)

// NewOllamaAuth returns an OllamaAuth that registers tokens with authn.
func NewOllamaAuth(authn *Authenticator) *OllamaAuth {
	return &OllamaAuth{Authenticator: authn, keys: map[string]model.Principal{}}
}

// AddKey makes requests with this Authorization header value authenticate as p.
func (o *OllamaAuth) AddKey(authorization string, p model.Principal) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.keys[authorization] = p
}

// IssueRegistryToken implements auth.OllamaAuth.
func (o *OllamaAuth) IssueRegistryToken(_ context.Context, req auth.OllamaTokenRequest) (string, time.Time, error) {
	o.mu.Lock()
	p, ok := o.keys[req.Authorization]
	o.mu.Unlock()
	if !ok {
		return "", time.Time{}, auth.ErrInvalidCredentials
	}
	token := "ollama." + uuid.NewString()
	p.Method = model.AuthOllamaKey
	p.Token = &model.TokenInfo{Name: "ollama", Scope: model.ScopeRead, Restrictions: model.TokenRestrictions{Repos: []string{string(req.Repo.Type) + ":" + req.Repo.String()}}}
	if o.Authenticator != nil {
		o.Authenticator.Add(token, p)
	}
	return token, time.Now().Add(5 * time.Minute), nil
}
