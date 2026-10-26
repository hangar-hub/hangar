package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/hangar-hub/hangar/internal/audit"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
)

// Ollama registry auth timings (docs/spec/08-local-tools.md "Auth").
const (
	ollamaMaxSkew       = 5 * time.Minute
	ollamaNonceTTL      = 10 * time.Minute
	ollamaRegistryTTL   = 5 * time.Minute
	maxOllamaKeysByUser = 50
)

// RepoResolver describes a Repo for a permission check. The integration
// wiring adapts repo.Service to it. It returns an error wrapping
// ErrNotFound for Repos that do not exist.
type RepoResolver interface {
	RepoTarget(ctx context.Context, id model.RepoID) (*RepoTarget, error)
}

// OllamaKey is a registered Ollama public key.
type OllamaKey struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	Name        string
	PublicKey   string
	Fingerprint string
	CreatedAt   time.Time
	LastUsedAt  *time.Time
}

// OllamaStore manages Ollama keys (AUTH-7) and issues Ollama registry
// tokens. It owns the ollama_keys and ollama_nonces tables and implements
// OllamaAuth.
type OllamaStore struct {
	s *Service
	// Repos describes Repos for the read check. Set it before serving
	// registry token requests.
	Repos RepoResolver
}

var _ OllamaAuth = (*OllamaStore)(nil)

// ParseOllamaPublicKey parses the contents of ~/.ollama/id_ed25519.pub
// (`ssh-ed25519 <base64> [comment]`) and returns the key without its
// comment and its SHA-256 fingerprint.
func ParseOllamaPublicKey(text string) (normalised, fingerprint string, err error) {
	text = strings.TrimSpace(text)
	if len(text) > 4096 {
		return "", "", fmt.Errorf("%w: the key is too long", ErrInvalid)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(text))
	if err != nil {
		return "", "", fmt.Errorf("%w: not an SSH public key; paste the contents of ~/.ollama/id_ed25519.pub", ErrInvalid)
	}
	if pub.Type() != ssh.KeyAlgoED25519 {
		return "", "", fmt.Errorf("%w: Ollama keys are ed25519; this key is %s", ErrInvalid, pub.Type())
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))), ssh.FingerprintSHA256(pub), nil
}

// ParseOllamaAuthorization splits Ollama's `<base64 public key>:<base64
// signature>` header value into an ed25519 public key and a signature.
func ParseOllamaAuthorization(h string) (ssh.PublicKey, []byte, error) {
	keyPart, sigPart, ok := strings.Cut(strings.TrimSpace(h), ":")
	if !ok || keyPart == "" || sigPart == "" || len(h) > 4096 {
		return nil, nil, errors.New("malformed Ollama authorization")
	}
	wire, err := base64.StdEncoding.DecodeString(keyPart)
	if err != nil {
		return nil, nil, errors.New("malformed Ollama public key")
	}
	pub, err := ssh.ParsePublicKey(wire)
	if err != nil || pub.Type() != ssh.KeyAlgoED25519 {
		return nil, nil, errors.New("the Ollama key must be ed25519")
	}
	sig, err := base64.StdEncoding.DecodeString(sigPart)
	if err != nil || len(sig) != 64 {
		return nil, nil, errors.New("malformed Ollama signature")
	}
	return pub, sig, nil
}

// OllamaSignedData returns the bytes Ollama signs for a token request:
// `<method>,<full URL>,<base64 of the hex SHA-256 of the empty body>`.
func OllamaSignedData(method, fullURL string) []byte {
	sum := sha256.Sum256(nil)
	return []byte(method + "," + fullURL + "," + base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(sum[:]))))
}

// OllamaRequestURL rebuilds the URL Ollama signed: the realm from
// server.public_url (scheme and host) with the request's path and raw
// query. The Ollama handler passes it as OllamaTokenRequest.URL.
func OllamaRequestURL(publicURL string, r *http.Request) string {
	base := strings.TrimRight(publicURL, "/")
	if u, err := url.Parse(publicURL); err == nil {
		base = u.Scheme + "://" + u.Host
	}
	return base + r.URL.RequestURI()
}

// liveOllamaKey selects keys that still grant access: those added without
// a Token, and those whose Token is neither revoked nor expired. It takes
// the current time as its argument.
const liveOllamaKey = "(?TableAlias.token_id IS NULL OR EXISTS (SELECT 1 FROM tokens AS lt WHERE lt.id = ?TableAlias.token_id" +
	" AND lt.revoked_at IS NULL AND (lt.expires_at IS NULL OR lt.expires_at > ?)))"

// Add registers a key for a User and records an ollama_key.changed event.
// A key added with a Token is bound to it (docs/spec/05-auth.md "Ollama
// keys"): its registry tokens get that Token's scope, restrictions and
// expiry, and stop when the Token is revoked or expires, so a restricted
// or expiring Token cannot use a key to read more, or for longer, than it
// could itself. Keys added in a UI session have the user's read access.
func (o *OllamaStore) Add(ctx context.Context, userID uuid.UUID, name, publicKey string, actor model.Principal) (*OllamaKey, error) {
	norm, fp, err := ParseOllamaPublicKey(publicKey)
	if err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if len(name) > 100 {
		return nil, fmt.Errorf("%w: the name can be at most 100 characters", ErrInvalid)
	}
	if name == "" {
		// Use the key's comment, usually user@host.
		if fields := strings.Fields(publicKey); len(fields) >= 3 {
			name = truncate(strings.Join(fields[2:], " "), 100)
		}
	}
	now := o.s.nowUTC()
	// A key whose Token was revoked or has expired grants nothing; remove
	// it, so the key can be registered again with a working credential.
	if _, err := o.s.db.NewDelete().Model((*db.OllamaKey)(nil)).Where("fingerprint = ?", fp).Where("NOT "+liveOllamaKey, now).Exec(ctx); err != nil {
		return nil, err
	}
	n, err := o.s.db.NewSelect().Model((*db.OllamaKey)(nil)).Where("user_id = ?", userID).Where(liveOllamaKey, now).Count(ctx)
	if err != nil {
		return nil, err
	}
	if n >= maxOllamaKeysByUser {
		return nil, fmt.Errorf("%w: at most %d Ollama keys per user", ErrInvalid, maxOllamaKeysByUser)
	}
	row := &db.OllamaKey{ID: db.NewID(), UserID: userID, Name: name, PublicKey: norm, Fingerprint: fp}
	if actor.Token != nil {
		row.TokenID = &actor.Token.ID
	}
	if _, err := o.s.db.NewInsert().Model(row).Exec(ctx); err != nil {
		if db.IsUniqueViolation(err) {
			return nil, fmt.Errorf("%w: this key is already registered", ErrConflict)
		}
		return nil, err
	}
	data := map[string]any{"change": "added", "fingerprint": fp}
	if row.TokenID != nil {
		data["token_id"] = row.TokenID.String()
	}
	o.s.record(ctx, audit.Event{Action: audit.OllamaKeyChanged, Actor: actor, TargetType: "ollama_key", TargetID: row.ID.String(), Data: data})
	return toOllamaKey(row), nil
}

func toOllamaKey(r *db.OllamaKey) *OllamaKey {
	return &OllamaKey{ID: r.ID, UserID: r.UserID, Name: r.Name, PublicKey: r.PublicKey, Fingerprint: r.Fingerprint,
		CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt}
}

// List returns a User's keys that still grant access, oldest first. Keys
// whose Token was revoked or has expired are left out, so clients such as
// `hangar ollama key register` register the key again.
func (o *OllamaStore) List(ctx context.Context, userID uuid.UUID) ([]OllamaKey, error) {
	var rows []db.OllamaKey
	if err := o.s.db.NewSelect().Model(&rows).Where("user_id = ?", userID).Where(liveOllamaKey, o.s.nowUTC()).
		OrderExpr("created_at ASC, id ASC").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]OllamaKey, 0, len(rows))
	for i := range rows {
		out = append(out, *toOllamaKey(&rows[i]))
	}
	return out, nil
}

// Delete removes one of a User's keys.
func (o *OllamaStore) Delete(ctx context.Context, userID, id uuid.UUID, actor model.Principal) error {
	var row db.OllamaKey
	err := o.s.db.NewSelect().Model(&row).Where("id = ? AND user_id = ?", id, userID).Scan(ctx)
	if db.IsNotFound(err) {
		return fmt.Errorf("%w: key", ErrNotFound)
	}
	if err != nil {
		return err
	}
	if _, err := o.s.db.NewDelete().Model((*db.OllamaKey)(nil)).Where("id = ?", id).Exec(ctx); err != nil {
		return err
	}
	o.s.record(ctx, audit.Event{Action: audit.OllamaKeyChanged, Actor: actor, TargetType: "ollama_key", TargetID: id.String(),
		Data: map[string]any{"change": "removed", "fingerprint": row.Fingerprint}})
	return nil
}

// IssueRegistryToken implements OllamaAuth. It verifies the ed25519
// signature over the realm URL, rejects a ts more than 5 minutes away and
// a nonce seen in the last 10 minutes, checks the key's owner can read the
// Repo's files (within the limits of the Token that registered the key, if
// any), and returns a JWT for that Repo valid for 5 minutes, or until that
// Token expires if sooner.
// Errors wrap ErrInvalidCredentials (bad signature, unknown key, stale or
// replayed request), ErrNotFound or ErrGated.
func (o *OllamaStore) IssueRegistryToken(ctx context.Context, req OllamaTokenRequest) (string, time.Time, error) {
	token, exp, err := o.issue(ctx, req)
	outcome := "success"
	if err != nil {
		outcome = "failure"
	}
	o.s.countAttempt(ctx, model.AuthOllamaKey, outcome)
	return token, exp, err
}

func (o *OllamaStore) issue(ctx context.Context, req OllamaTokenRequest) (string, time.Time, error) {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	pub, sig, err := ParseOllamaAuthorization(req.Authorization)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %w", ErrInvalidCredentials, err)
	}
	if err := pub.Verify(OllamaSignedData(method, req.URL), &ssh.Signature{Format: ssh.KeyAlgoED25519, Blob: sig}); err != nil {
		return "", time.Time{}, fmt.Errorf("%w: bad signature", ErrInvalidCredentials)
	}
	u, err := url.Parse(req.URL)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: bad URL", ErrInvalidCredentials)
	}
	q := u.Query()
	now := o.s.nowUTC()
	ts, err := strconv.ParseInt(q.Get("ts"), 10, 64)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: missing ts", ErrInvalidCredentials)
	}
	if d := now.Sub(time.Unix(ts, 0)); d > ollamaMaxSkew || d < -ollamaMaxSkew {
		return "", time.Time{}, fmt.Errorf("%w: the request time is more than 5 minutes from the server's; check the clock", ErrInvalidCredentials)
	}
	nonce := q.Get("nonce")
	if nonce == "" || len(nonce) > 256 {
		return "", time.Time{}, fmt.Errorf("%w: missing nonce", ErrInvalidCredentials)
	}
	var key db.OllamaKey
	err = o.s.db.NewSelect().Model(&key).Where("fingerprint = ?", ssh.FingerprintSHA256(pub)).Scan(ctx)
	if db.IsNotFound(err) {
		return "", time.Time{}, fmt.Errorf("%w: this Ollama key is not registered; add it under Settings, Ollama keys", ErrInvalidCredentials)
	}
	if err != nil {
		return "", time.Time{}, err
	}
	if err := o.useNonce(ctx, nonce, now); err != nil {
		return "", time.Time{}, err
	}
	user, err := o.s.Users.Get(ctx, key.UserID)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: unknown user", ErrInvalidCredentials)
	}
	if user.Disabled() {
		return "", time.Time{}, fmt.Errorf("%w: %w", ErrInvalidCredentials, ErrDisabled)
	}
	if o.Repos == nil {
		return "", time.Time{}, errors.New("auth: OllamaStore.Repos is not set")
	}
	target, err := o.Repos.RepoTarget(ctx, req.Repo)
	if err != nil {
		return "", time.Time{}, err
	}
	p := user.Principal(model.AuthOllamaKey)
	exp := now.Add(ollamaRegistryTTL)
	if key.TokenID != nil {
		// The key was registered with a Token: it reads only what that
		// Token can read, and only while the Token works.
		var tok db.Token
		err := o.s.db.NewSelect().Model(&tok).Where("id = ?", *key.TokenID).Scan(ctx)
		if err != nil && !db.IsNotFound(err) {
			return "", time.Time{}, err
		}
		if err != nil || tok.RevokedAt != nil || (tok.ExpiresAt != nil && !now.Before(*tok.ExpiresAt)) {
			return "", time.Time{}, fmt.Errorf("%w: the token that registered this Ollama key was revoked or has expired; register the key again", ErrInvalidCredentials)
		}
		p.Token = tokenInfo(&tok)
		if tok.ExpiresAt != nil && tok.ExpiresAt.Before(exp) {
			exp = tok.ExpiresAt.UTC()
		}
	}
	if err := o.s.Authorizer.Can(ctx, p, ActionReadFiles, Target{Repo: target}); err != nil {
		return "", time.Time{}, err
	}
	if _, err := o.s.db.NewUpdate().Model((*db.OllamaKey)(nil)).Where("id = ?", key.ID).Set("last_used_at = ?", now).Exec(ctx); err != nil {
		o.s.log.WarnContext(ctx, "record Ollama key use", "error", err)
	}
	tok, err := signJWT(o.s.jwtKey, registryClaims{Issuer: o.s.publicURL(), Audience: registryAudience, Subject: user.ID.String(),
		IssuedAt: now.Unix(), ExpiresAt: exp.Unix(), ID: uuid.NewString(), Repo: repoKey(target.ID)})
	if err != nil {
		return "", time.Time{}, err
	}
	return tok, exp, nil
}

// useNonce stores a nonce, failing if it was used in the last 10 minutes.
func (o *OllamaStore) useNonce(ctx context.Context, nonce string, now time.Time) error {
	if _, err := o.s.db.NewDelete().Model((*db.OllamaNonce)(nil)).Where("expires_at < ?", now).Exec(ctx); err != nil {
		return err
	}
	row := &db.OllamaNonce{Nonce: nonce, ExpiresAt: now.Add(ollamaNonceTTL)}
	if _, err := o.s.db.NewInsert().Model(row).Exec(ctx); err != nil {
		if db.IsUniqueViolation(err) {
			return fmt.Errorf("%w: the nonce was already used", ErrInvalidCredentials)
		}
		return err
	}
	return nil
}
