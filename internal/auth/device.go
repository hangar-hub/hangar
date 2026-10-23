package auth

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
)

// Device-code login timings (RFC 8628).
const (
	deviceCodeTTL      = 15 * time.Minute
	devicePollInterval = 5 * time.Second
	// DeviceTokenTTL is how long a Token from a device login lasts
	// (05-auth.md "Device login"), so a login approved by mistake, or
	// through a phishing link, does not give access for ever.
	DeviceTokenTTL = 30 * 24 * time.Hour
	// DeviceVerificationPath is the UI page where users enter the code.
	DeviceVerificationPath = "/settings/device"
)

// userCodeAlphabet has no vowels or look-alike characters (RFC 8628 6.1).
const userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

// DeviceStore is the database implementation of DeviceFlow. It owns the
// oauth_device_codes table. The Token is created when the client polls
// after approval, so no Token secret is ever stored.
type DeviceStore struct{ s *Service }

var _ DeviceFlow = (*DeviceStore)(nil)

func newUserCode() (string, error) {
	var b strings.Builder
	limit := big.NewInt(int64(len(userCodeAlphabet)))
	for i := range 8 {
		if i == 4 {
			b.WriteByte('-')
		}
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		b.WriteByte(userCodeAlphabet[n.Int64()])
	}
	return b.String(), nil
}

// NormaliseUserCode upper-cases a user code and restores its dash, so
// users can type it in any case, with or without the dash.
func NormaliseUserCode(code string) string {
	c := strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(code))
	if len(c) != 8 {
		return c
	}
	return c[:4] + "-" + c[4:]
}

// Start implements DeviceFlow. The verification URL carries no code:
// the approval page asks the user to type the code their terminal shows,
// so a link someone else sends cannot approve their login in one click.
func (d *DeviceStore) Start(ctx context.Context, clientID, clientIP string) (*DeviceCode, error) {
	if len(clientID) > 200 {
		return nil, fmt.Errorf("%w: client_id is too long", ErrInvalid)
	}
	deviceCode, err := randomString(32)
	if err != nil {
		return nil, err
	}
	now := d.s.nowUTC()
	for range 5 {
		userCode, err := newUserCode()
		if err != nil {
			return nil, err
		}
		row := &db.OAuthDeviceCode{DeviceCodeHash: hashSecret(deviceCode), UserCode: userCode, ClientID: clientID,
			ClientIP: truncate(clientIP, 64), Status: "pending", Scope: string(model.ScopeRead),
			PollInterval: int(devicePollInterval / time.Second), ExpiresAt: now.Add(deviceCodeTTL),
			Timestamps: db.Timestamps{CreatedAt: now, UpdatedAt: now}}
		if _, err := d.s.db.NewInsert().Model(row).Exec(ctx); err != nil {
			if db.IsUniqueViolation(err) {
				continue
			}
			return nil, err
		}
		uri := d.s.publicURL() + DeviceVerificationPath
		return &DeviceCode{DeviceCode: deviceCode, UserCode: userCode, VerificationURI: uri,
			Interval: devicePollInterval, ExpiresIn: deviceCodeTTL}, nil
	}
	return nil, fmt.Errorf("could not allocate a unique user code")
}

func (d *DeviceStore) pending(ctx context.Context, idb bun.IDB, userCode string) (*db.OAuthDeviceCode, error) {
	var row db.OAuthDeviceCode
	err := idb.NewSelect().Model(&row).Where("user_code = ?", NormaliseUserCode(userCode)).Scan(ctx)
	if db.IsNotFound(err) {
		return nil, fmt.Errorf("%w: unknown or expired code", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	if row.Status != "pending" || !d.s.nowUTC().Before(row.ExpiresAt) {
		return nil, fmt.Errorf("%w: unknown or expired code", ErrNotFound)
	}
	return &row, nil
}

// answerer checks that p may see and answer device logins: a signed-in
// user in a UI session, never a Token.
func answerer(p model.Principal) error {
	if !p.IsUser() {
		return fmt.Errorf("%w: sign in to answer a device login", ErrForbidden)
	}
	if p.Token != nil {
		return fmt.Errorf("%w: answer device logins in the web UI, not with a token", ErrForbidden)
	}
	return nil
}

// Lookup implements DeviceFlow.
func (d *DeviceStore) Lookup(ctx context.Context, userCode string, p model.Principal) (*DeviceRequest, error) {
	if err := answerer(p); err != nil {
		return nil, err
	}
	row, err := d.pending(ctx, d.s.db, userCode)
	if err != nil {
		return nil, err
	}
	return &DeviceRequest{UserCode: row.UserCode, ClientID: row.ClientID, ClientIP: row.ClientIP,
		RequestedAt: row.CreatedAt.UTC(), ExpiresAt: row.ExpiresAt.UTC()}, nil
}

func (d *DeviceStore) decide(ctx context.Context, userCode string, p model.Principal, status string, scope model.Scope) error {
	if err := answerer(p); err != nil {
		return err
	}
	return d.s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		row, err := d.pending(ctx, tx, userCode)
		if err != nil {
			return err
		}
		uid := p.UserID
		res, err := tx.NewUpdate().Model((*db.OAuthDeviceCode)(nil)).Where("device_code_hash = ? AND status = ?", row.DeviceCodeHash, "pending").
			Set("status = ?", status).Set("scope = ?", string(scope)).Set("user_id = ?", uid).Set("updated_at = ?", d.s.nowUTC()).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: unknown or expired code", ErrNotFound)
		}
		return nil
	})
}

// Approve implements DeviceFlow. The scope is read or write: a device
// login never gives the admin scope.
func (d *DeviceStore) Approve(ctx context.Context, userCode string, scope model.Scope, p model.Principal) error {
	if scope != model.ScopeRead && scope != model.ScopeWrite {
		return fmt.Errorf("%w: a device login gets the read or write scope", ErrInvalid)
	}
	return d.decide(ctx, userCode, p, "approved", scope)
}

// Deny implements DeviceFlow.
func (d *DeviceStore) Deny(ctx context.Context, userCode string, p model.Principal) error {
	return d.decide(ctx, userCode, p, "denied", model.ScopeRead)
}

func deviceTokenName(clientID string) string {
	if clientID == "" || len(clientID) > 60 {
		return "device login"
	}
	if _, err := uuid.Parse(clientID); err == nil {
		return "device login"
	}
	return "device login (" + clientID + ")"
}

// Poll implements DeviceFlow. Polling faster than the interval gives
// ErrSlowDown. Once approved, the first poll creates a Token for the
// approving user, with the scope they chose and an expiry DeviceTokenTTL
// away, and returns its secret; later polls give ErrExpiredToken.
func (d *DeviceStore) Poll(ctx context.Context, deviceCode string) (string, error) {
	var row db.OAuthDeviceCode
	err := d.s.db.NewSelect().Model(&row).Where("device_code_hash = ?", hashSecret(deviceCode)).Scan(ctx)
	if db.IsNotFound(err) {
		return "", ErrExpiredToken
	}
	if err != nil {
		return "", err
	}
	now := d.s.nowUTC()
	if !now.Before(row.ExpiresAt) || row.Status == "used" {
		return "", ErrExpiredToken
	}
	interval := time.Duration(row.PollInterval) * time.Second
	tooSoon := row.LastPolledAt != nil && now.Sub(*row.LastPolledAt) < interval
	if _, err := d.s.db.NewUpdate().Model((*db.OAuthDeviceCode)(nil)).Where("device_code_hash = ?", row.DeviceCodeHash).
		Set("last_polled_at = ?", now).Exec(ctx); err != nil {
		return "", err
	}
	switch row.Status {
	case "denied":
		return "", ErrAccessDenied
	case "pending":
		if tooSoon {
			return "", ErrSlowDown
		}
		return "", ErrAuthorizationPending
	}
	// Approved: claim it so only one poll creates a Token.
	res, err := d.s.db.NewUpdate().Model((*db.OAuthDeviceCode)(nil)).
		Where("device_code_hash = ? AND status = ?", row.DeviceCodeHash, "approved").
		Set("status = ?", "used").Set("updated_at = ?", now).Exec(ctx)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 || row.UserID == nil {
		return "", ErrExpiredToken
	}
	u, err := d.s.Users.Get(ctx, *row.UserID)
	if err != nil {
		return "", err
	}
	scope := model.Scope(row.Scope)
	if scope != model.ScopeWrite {
		scope = model.ScopeRead
	}
	expires := now.Add(DeviceTokenTTL)
	tok, secret, err := d.s.TokenStore.CreateAs(ctx, NewToken{UserID: u.ID, Name: deviceTokenName(row.ClientID), Scope: scope, ExpiresAt: &expires},
		u.Principal(model.AuthSession))
	if err != nil {
		return "", err
	}
	if _, err := d.s.db.NewUpdate().Model((*db.OAuthDeviceCode)(nil)).Where("device_code_hash = ?", row.DeviceCodeHash).
		Set("token_id = ?", tok.ID).Exec(ctx); err != nil {
		d.s.log.WarnContext(ctx, "record device login token", "error", err)
	}
	return secret, nil
}

// DeleteExpired removes device codes that expired more than a day ago.
func (d *DeviceStore) DeleteExpired(ctx context.Context) (int64, error) {
	res, err := d.s.db.NewDelete().Model((*db.OAuthDeviceCode)(nil)).Where("expires_at < ?", d.s.nowUTC().Add(-24*time.Hour)).Exec(ctx)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
