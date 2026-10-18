package auth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"golang.org/x/oauth2"

	"github.com/hangar-hub/hangar/internal/audit"
	"github.com/hangar-hub/hangar/internal/config"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/org"
)

// oidcStateTTL is how long a user has to finish signing in at the provider.
const oidcStateTTL = 10 * time.Minute

// OIDC sign-in errors. Each has a short code for the login page's
// `?error=` parameter.
var (
	// ErrOIDCUnknownProvider means no provider has that ID.
	ErrOIDCUnknownProvider = fmt.Errorf("%w: unknown sign-in provider", ErrNotFound)
	// ErrOIDCState means the state is unknown, used, expired or for
	// another provider.
	ErrOIDCState = errors.New("the sign-in attempt expired or was already used; start again")
	// ErrOIDCLinkRequired means a local account has the same verified
	// email; its owner must sign in locally and link the provider first.
	ErrOIDCLinkRequired = errors.New("an account with this email already exists; sign in with your password, then link this provider in Settings")
	// ErrOIDCNoAccount means the provider does not create accounts and the
	// identity is not linked to one.
	ErrOIDCNoAccount = errors.New("no Hangar account is linked to this identity; ask an admin to create one")
	// ErrOIDCIdentityTaken means the identity is linked to another account.
	ErrOIDCIdentityTaken = errors.New("this identity is already linked to another Hangar account")
)

// OIDCErrorCode returns the login page error code for an OIDC error.
func OIDCErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrOIDCState):
		return "oidc_state"
	case errors.Is(err, ErrOIDCLinkRequired):
		return "oidc_link_required"
	case errors.Is(err, ErrOIDCNoAccount):
		return "oidc_no_account"
	case errors.Is(err, ErrOIDCIdentityTaken):
		return "oidc_identity_taken"
	case errors.Is(err, ErrDisabled):
		return "account_disabled"
	case errors.Is(err, ErrOIDCUnknownProvider):
		return "oidc_unknown_provider"
	default:
		return "oidc_failed"
	}
}

// OIDCService runs OpenID Connect sign-in (AUTH-2): discovery, the
// authorisation code flow with PKCE, state and nonce checks, claim
// mapping, just-in-time accounts, admin groups and Organisation mappings.
// It owns the identities and oidc_states tables.
type OIDCService struct {
	s         *Service
	providers []config.OIDCProvider

	mu   sync.Mutex
	live map[string]*oidcProvider
}

type oidcProvider struct {
	cfg      config.OIDCProvider
	provider *oidc.Provider
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
}

func newOIDCService(s *Service) (*OIDCService, error) {
	o := &OIDCService{s: s, providers: s.cfg.OIDC.Providers, live: map[string]*oidcProvider{}}
	seen := map[string]bool{}
	for _, p := range o.providers {
		if p.ID == "" || seen[p.ID] {
			return nil, fmt.Errorf("auth: OIDC provider IDs must be set and unique (%q)", p.ID)
		}
		seen[p.ID] = true
	}
	return o, nil
}

// ProviderInfo describes a provider for the sign-in page.
type ProviderInfo struct {
	ID       string
	Name     string
	Issuer   string
	LoginURL string
}

// Providers lists the configured providers.
func (o *OIDCService) Providers() []ProviderInfo {
	out := make([]ProviderInfo, 0, len(o.providers))
	for _, p := range o.providers {
		name := p.Name
		if name == "" {
			name = p.ID
		}
		out = append(out, ProviderInfo{ID: p.ID, Name: name, Issuer: p.Issuer, LoginURL: "/auth/oidc/" + p.ID})
	}
	return out
}

func (o *OIDCService) config(id string) (config.OIDCProvider, bool) {
	for _, p := range o.providers {
		if p.ID == id {
			return p, true
		}
	}
	return config.OIDCProvider{}, false
}

// CallbackURL is the redirect URI to register with a provider.
func (o *OIDCService) CallbackURL(id string) string {
	return o.s.publicURL() + "/auth/oidc/" + id + "/callback"
}

// provider runs discovery once per provider, retrying on later calls if
// it failed.
func (o *OIDCService) provider(ctx context.Context, id string) (*oidcProvider, error) {
	cfg, ok := o.config(id)
	if !ok {
		return nil, ErrOIDCUnknownProvider
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if p, ok := o.live[id]; ok {
		return p, nil
	}
	dctx, cancel := context.WithTimeout(oidc.ClientContext(context.WithoutCancel(ctx), o.s.client), 15*time.Second)
	defer cancel()
	prov, err := oidc.NewProvider(dctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery for %s: %w", id, err)
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	if !slices.Contains(scopes, oidc.ScopeOpenID) {
		scopes = append([]string{oidc.ScopeOpenID}, scopes...)
	}
	p := &oidcProvider{
		cfg: cfg, provider: prov,
		oauth: oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret, Endpoint: prov.Endpoint(),
			RedirectURL: o.CallbackURL(id), Scopes: scopes},
		verifier: prov.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
	}
	o.live[id] = p
	return p, nil
}

// SafeRedirect keeps only local paths, so a sign-in cannot redirect to
// another site.
func SafeRedirect(to string) string {
	if to == "" || !strings.HasPrefix(to, "/") || strings.HasPrefix(to, "//") || strings.HasPrefix(to, "/\\") ||
		strings.ContainsAny(to, "\r\n") || len(to) > 2000 {
		return "/"
	}
	return to
}

// Start begins a sign-in and returns the provider's authorisation URL.
// linkUser, when set, links the identity to that signed-in User instead
// of signing in.
func (o *OIDCService) Start(ctx context.Context, providerID, redirectTo string, linkUser uuid.UUID) (string, error) {
	p, err := o.provider(ctx, providerID)
	if err != nil {
		return "", err
	}
	state, err := randomString(32)
	if err != nil {
		return "", err
	}
	nonce, err := randomString(32)
	if err != nil {
		return "", err
	}
	verifier := oauth2.GenerateVerifier()
	row := &db.OIDCState{StateHash: hashSecret(state), Provider: providerID, Nonce: nonce, PKCEVerifier: verifier,
		RedirectTo: SafeRedirect(redirectTo), ExpiresAt: o.s.nowUTC().Add(oidcStateTTL)}
	if linkUser != uuid.Nil {
		row.LinkUserID = &linkUser
	}
	if _, err := o.s.db.NewDelete().Model((*db.OIDCState)(nil)).Where("expires_at < ?", o.s.nowUTC()).Exec(ctx); err != nil {
		return "", err
	}
	if _, err := o.s.db.NewInsert().Model(row).Exec(ctx); err != nil {
		return "", err
	}
	return p.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), nil
}

// CallbackResult is a finished OIDC sign-in.
type CallbackResult struct {
	User       *User
	RedirectTo string
	// Linked is true when the callback linked an identity to a signed-in
	// User rather than signing in.
	Linked bool
}

// Callback finishes a sign-in: it checks the state, exchanges the code
// with the PKCE verifier, verifies the ID token and nonce, and finds,
// links or creates the User. It records login.succeeded or login.failed.
func (o *OIDCService) Callback(ctx context.Context, providerID, state, code, ip, userAgent string) (*CallbackResult, error) {
	res, err := o.callback(ctx, providerID, state, code)
	ev := audit.Event{IP: ip, UserAgent: userAgent, TargetType: "user"}
	if err != nil {
		ev.Action, ev.Actor = audit.LoginFailed, model.Anonymous()
		ev.Data = map[string]any{"method": string(model.AuthOIDC), "provider": providerID, "reason": OIDCErrorCode(err)}
		o.s.record(ctx, ev)
		o.s.countAttempt(ctx, model.AuthOIDC, "failure")
		return nil, err
	}
	ev.Actor, ev.TargetID = res.User.Principal(model.AuthOIDC), res.User.Username
	if res.Linked {
		ev.Action = audit.IdentityLinked
		ev.Data = map[string]any{"provider": providerID}
	} else {
		ev.Action = audit.LoginSucceeded
		ev.Data = map[string]any{"method": string(model.AuthOIDC), "provider": providerID}
		o.s.countAttempt(ctx, model.AuthOIDC, "success")
	}
	o.s.record(ctx, ev)
	return res, nil
}

// identityClaims are the mapped claims of one sign-in.
type identityClaims struct {
	Subject       string
	Username      string
	Email         string
	EmailVerified bool
	Name          string
	Groups        []string
	HasGroups     bool
	Raw           map[string]any
}

func claimName(cfg config.OIDCProvider, key, def string) string {
	if v := cfg.Claims[key]; v != "" {
		return v
	}
	return def
}

// lookupClaim reads a claim, following dots into nested objects
// (for example `realm_access.roles`).
func lookupClaim(raw map[string]any, name string) (any, bool) {
	if v, ok := raw[name]; ok {
		return v, true
	}
	cur := any(raw)
	for _, part := range strings.Split(name, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

func claimString(raw map[string]any, name string) string {
	v, _ := lookupClaim(raw, name)
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

func mapClaims(cfg config.OIDCProvider, raw map[string]any) identityClaims {
	c := identityClaims{Raw: raw, Subject: claimString(raw, "sub")}
	c.Username = claimString(raw, claimName(cfg, "username", "preferred_username"))
	c.Email = claimString(raw, claimName(cfg, "email", "email"))
	c.Name = claimString(raw, claimName(cfg, "name", "name"))
	switch v, _ := lookupClaim(raw, "email_verified"); t := v.(type) {
	case bool:
		c.EmailVerified = t
	case string:
		c.EmailVerified = t == "true"
	}
	if v, ok := lookupClaim(raw, claimName(cfg, "groups", "groups")); ok {
		c.HasGroups = true
		switch t := v.(type) {
		case []any:
			for _, g := range t {
				if s, ok := g.(string); ok {
					c.Groups = append(c.Groups, strings.TrimPrefix(s, "/"))
				}
			}
		case string:
			for _, g := range strings.FieldsFunc(t, func(r rune) bool { return r == ',' || r == ' ' }) {
				c.Groups = append(c.Groups, strings.TrimPrefix(g, "/"))
			}
		default:
			c.HasGroups = false
		}
	}
	return c
}

func (o *OIDCService) callback(ctx context.Context, providerID, state, code string) (*CallbackResult, error) {
	if state == "" || code == "" {
		return nil, ErrOIDCState
	}
	var st db.OIDCState
	err := o.s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		if err := tx.NewSelect().Model(&st).Where("state_hash = ?", hashSecret(state)).Scan(ctx); err != nil {
			if db.IsNotFound(err) {
				return ErrOIDCState
			}
			return err
		}
		_, err := tx.NewDelete().Model((*db.OIDCState)(nil)).Where("state_hash = ?", st.StateHash).Exec(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	if st.Provider != providerID || !o.s.nowUTC().Before(st.ExpiresAt) {
		return nil, ErrOIDCState
	}
	p, err := o.provider(ctx, providerID)
	if err != nil {
		return nil, err
	}
	hctx, cancel := context.WithTimeout(oidc.ClientContext(ctx, o.s.client), 30*time.Second)
	defer cancel()
	tok, err := p.oauth.Exchange(hctx, code, oauth2.VerifierOption(st.PKCEVerifier))
	if err != nil {
		return nil, fmt.Errorf("exchange the authorisation code: %w", err)
	}
	rawID, ok := tok.Extra("id_token").(string)
	if !ok || rawID == "" {
		return nil, errors.New("the provider returned no ID token")
	}
	idt, err := p.verifier.Verify(hctx, rawID)
	if err != nil {
		return nil, fmt.Errorf("verify the ID token: %w", err)
	}
	if idt.Nonce != st.Nonce {
		return nil, errors.New("the ID token nonce does not match")
	}
	raw := map[string]any{}
	if err := idt.Claims(&raw); err != nil {
		return nil, fmt.Errorf("read ID token claims: %w", err)
	}
	// Fill claims the ID token leaves out from the userinfo endpoint.
	if p.provider.UserInfoEndpoint() != "" {
		if ui, err := p.provider.UserInfo(hctx, oauth2.StaticTokenSource(tok)); err == nil {
			extra := map[string]any{}
			if err := ui.Claims(&extra); err == nil && extra["sub"] == raw["sub"] {
				for k, v := range extra {
					if _, ok := raw[k]; !ok {
						raw[k] = v
					}
				}
			}
		} else {
			o.s.log.WarnContext(ctx, "OIDC userinfo request failed", "provider", providerID, "error", err)
		}
	}
	claims := mapClaims(p.cfg, raw)
	if claims.Subject == "" {
		return nil, errors.New("the ID token has no subject")
	}
	return o.signIn(ctx, p.cfg, claims, st)
}

// signIn finds, links or creates the User for verified claims, then
// applies admin groups and Organisation mappings.
func (o *OIDCService) signIn(ctx context.Context, cfg config.OIDCProvider, c identityClaims, st db.OIDCState) (*CallbackResult, error) {
	res := &CallbackResult{RedirectTo: SafeRedirect(st.RedirectTo)}
	var ident db.Identity
	err := o.s.db.NewSelect().Model(&ident).Where("provider = ? AND subject = ?", cfg.ID, c.Subject).Scan(ctx)
	found := err == nil
	if err != nil && !db.IsNotFound(err) {
		return nil, err
	}
	var user *User
	switch {
	case st.LinkUserID != nil:
		if found && ident.UserID != *st.LinkUserID {
			return nil, ErrOIDCIdentityTaken
		}
		user, err = o.s.Users.Get(ctx, *st.LinkUserID)
		if err != nil {
			return nil, err
		}
		if !found {
			if err := o.insertIdentity(ctx, user.ID, cfg.ID, c); err != nil {
				return nil, err
			}
		}
		res.Linked = true
	case found:
		user, err = o.s.Users.Get(ctx, ident.UserID)
		if err != nil {
			return nil, err
		}
		// Some providers (Keycloak among them) leave the groups claim out
		// when the user is in no groups. If an earlier sign-in carried the
		// claim, its absence now means "no groups", not "no groups claim".
		if _, had := lookupClaim(ident.Claims, claimName(cfg, "groups", "groups")); had && !c.HasGroups {
			c.HasGroups, c.Groups = true, nil
		}
		if _, err := o.s.db.NewUpdate().Model((*db.Identity)(nil)).Where("id = ?", ident.ID).
			Set("claims = ?", jsonClaims(c.Raw)).Set("email = ?", nullable(c.Email)).
			Set("last_login_at = ?", o.s.nowUTC()).Set("updated_at = ?", o.s.nowUTC()).Exec(ctx); err != nil {
			return nil, err
		}
	default:
		if c.Email != "" && c.EmailVerified {
			// Only a verified email counts: an address a user typed
			// for themselves cannot block someone else's first sign-in.
			if existing, err := o.s.Users.GetByEmail(ctx, c.Email); err == nil && !existing.IsSystem && existing.EmailVerified {
				return nil, ErrOIDCLinkRequired
			} else if err != nil && !errors.Is(err, ErrNotFound) {
				return nil, err
			}
		}
		if !cfg.AutoCreateUsers {
			return nil, ErrOIDCNoAccount
		}
		user, err = o.createUser(ctx, cfg, c)
		if err != nil {
			return nil, err
		}
	}
	if user.Disabled() {
		return nil, ErrDisabled
	}
	if err := o.applyGroups(ctx, cfg, c, user); err != nil {
		return nil, err
	}
	user, err = o.s.Users.Get(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	res.User = user
	return res, nil
}

func jsonClaims(raw map[string]any) map[string]any {
	// Keep the claims an admin may want to see; drop token-like ones.
	out := map[string]any{}
	for k, v := range raw {
		switch k {
		case "at_hash", "c_hash", "nonce", "sid", "session_state":
			continue
		}
		out[k] = v
	}
	return out
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (o *OIDCService) insertIdentity(ctx context.Context, userID uuid.UUID, provider string, c identityClaims) error {
	now := o.s.nowUTC()
	row := &db.Identity{ID: db.NewID(), UserID: userID, Provider: provider, Subject: c.Subject, Email: nullable(c.Email),
		Claims: jsonClaims(c.Raw), LastLoginAt: &now}
	if _, err := o.s.db.NewInsert().Model(row).Exec(ctx); err != nil {
		if db.IsUniqueViolation(err) {
			return ErrOIDCIdentityTaken
		}
		return err
	}
	return nil
}

var usernameStrip = regexp.MustCompile(`[^A-Za-z0-9\-_.]+`)

// usernameBase turns a claim into a valid username stem.
func usernameBase(c identityClaims) string {
	for _, cand := range []string{c.Username, strings.Split(c.Email, "@")[0], c.Name} {
		s := usernameStrip.ReplaceAllString(strings.ReplaceAll(cand, " ", "-"), "")
		s = strings.TrimLeft(s, "-_.")
		if len(s) > 36 {
			s = s[:36]
		}
		if len(s) >= 2 {
			return s
		}
	}
	return "user"
}

// createUser creates a User from claims, adding a numeric suffix when the
// username is taken or reserved.
func (o *OIDCService) createUser(ctx context.Context, cfg config.OIDCProvider, c identityClaims) (*User, error) {
	base := usernameBase(c)
	email := c.Email
	if email != "" {
		if _, err := normaliseEmail(email); err != nil {
			email = ""
		} else if _, err := o.s.Users.GetByEmail(ctx, email); err == nil {
			// An unverified email that another account uses is not copied.
			email = ""
		}
	}
	for i := 0; i < 100; i++ {
		name := base
		if i > 0 {
			name = base + "-" + strconv.Itoa(i)
		}
		if model.ValidateNewNamespaceName(name) != nil {
			continue
		}
		var id uuid.UUID
		err := o.s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
			var err error
			id, err = o.s.Users.createTx(ctx, tx, NewUser{Username: name, Email: email, EmailVerified: c.EmailVerified && email != "", DisplayName: truncate(c.Name, 100)})
			if err != nil {
				return err
			}
			now := o.s.nowUTC()
			row := &db.Identity{ID: db.NewID(), UserID: id, Provider: cfg.ID, Subject: c.Subject, Email: nullable(c.Email),
				Claims: jsonClaims(c.Raw), LastLoginAt: &now}
			_, err = tx.NewInsert().Model(row).Exec(ctx)
			return err
		})
		if errors.Is(err, ErrConflict) {
			if taken, terr := org.NameTaken(ctx, o.s.db, name); terr == nil && !taken {
				// The email was taken in the meantime; retry without it.
				email = ""
			}
			continue
		}
		if err != nil {
			if db.IsUniqueViolation(err) {
				return nil, ErrOIDCIdentityTaken
			}
			return nil, err
		}
		u, err := o.s.Users.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		o.s.record(ctx, audit.Event{Action: audit.UserCreated, Actor: u.Principal(model.AuthOIDC), TargetType: "user", TargetID: u.Username,
			Data: map[string]any{"provider": cfg.ID, "source": "oidc"}})
		return u, nil
	}
	return nil, fmt.Errorf("%w: could not find a free username for %q", ErrConflict, base)
}

// applyGroups re-evaluates admin_groups and org_mappings. Providers whose
// tokens carry no groups claim skip both.
func (o *OIDCService) applyGroups(ctx context.Context, cfg config.OIDCProvider, c identityClaims, u *User) error {
	if !c.HasGroups {
		return nil
	}
	inGroup := func(g string) bool {
		return slices.ContainsFunc(c.Groups, func(x string) bool { return x == g })
	}
	actor := model.System()
	if len(cfg.AdminGroups) > 0 {
		want := slices.ContainsFunc(cfg.AdminGroups, inGroup)
		bootstrap := strings.EqualFold(u.Username, o.s.cfg.Security.BootstrapAdmin.Username)
		if want != u.IsAdmin && (!bootstrap || want) {
			if _, err := o.s.Users.Update(ctx, u.ID, UserUpdate{IsAdmin: &want}, actor); err != nil {
				if !errors.Is(err, ErrLastInstanceAdmin) {
					return err
				}
				o.s.log.WarnContext(ctx, "kept the last Instance admin despite admin_groups", "user_id", u.ID.String())
			}
		}
	}
	if len(cfg.OrgMappings) > 0 {
		var want []org.OIDCMapping
		for _, m := range cfg.OrgMappings {
			if inGroup(m.Group) {
				r, err := model.ParseRole(m.Role)
				if err != nil {
					return fmt.Errorf("org mapping for group %s: %w", m.Group, err)
				}
				want = append(want, org.OIDCMapping{Org: m.Org, Role: r})
			}
		}
		if err := o.s.orgs.SyncOIDCMemberships(ctx, u.ID, cfg.ID, want, actor); err != nil {
			return err
		}
	}
	return nil
}

// Unlink removes a provider from a User's account. A User without a
// password keeps at least one identity, so they can still sign in.
func (o *OIDCService) Unlink(ctx context.Context, userID uuid.UUID, provider string, actor model.Principal) error {
	u, err := o.s.Users.Get(ctx, userID)
	if err != nil {
		return err
	}
	if !slices.Contains(u.OIDCProviders, provider) {
		return fmt.Errorf("%w: %s is not linked", ErrNotFound, provider)
	}
	if !u.HasPassword && len(u.OIDCProviders) == 1 {
		return fmt.Errorf("%w: set a password before unlinking your only sign-in provider", ErrInvalid)
	}
	if _, err := o.s.db.NewDelete().Model((*db.Identity)(nil)).Where("user_id = ? AND provider = ?", userID, provider).Exec(ctx); err != nil {
		return err
	}
	o.s.record(ctx, audit.Event{Action: audit.IdentityLinked, Actor: actor, TargetType: "user", TargetID: u.Username,
		Data: map[string]any{"provider": provider, "change": "unlinked"}})
	return nil
}
