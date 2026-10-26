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

// Start implements DeviceFlow.
func (d *DeviceStore) Start(ctx context.Context, clientID string) (*DeviceCode, error) {
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
			Status: "pending", PollInterval: int(devicePollInterval / time.Second), ExpiresAt: now.Add(deviceCodeTTL)}
		if _, err := d.s.db.NewInsert().Model(row).Exec(ctx); err != nil {
			if db.IsUniqueViolation(err) {
				continue
			}
			return nil, err
		}
		uri := d.s.publicURL() + DeviceVerificationPath
		return &DeviceCode{DeviceCode: deviceCode, UserCode: userCode, VerificationURI: uri,
			VerificationURIComplete: uri + "?code=" + userCode, Interval: devicePollInterval, ExpiresIn: deviceCodeTTL}, nil
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

func (d *DeviceStore) decide(ctx context.Context, userCode string, p model.Principal, status string) error {
	if !p.IsUser() {
		return fmt.Errorf("%w: sign in to answer a device login", ErrForbidden)
	}
	if p.Token != nil {
		return fmt.Errorf("%w: answer device logins in the web UI, not with a Token", ErrForbidden)
	}
	return d.s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		row, err := d.pending(ctx, tx, userCode)
		if err != nil {
			return err
		}
		uid := p.UserID
		res, err := tx.NewUpdate().Model((*db.OAuthDeviceCode)(nil)).Where("device_code_hash = ? AND status = ?", row.DeviceCodeHash, "pending").
			Set("status = ?", status).Set("user_id = ?", uid).Set("updated_at = ?", d.s.nowUTC()).Exec(ctx)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("%w: unknown or expired code", ErrNotFound)
		}
		return nil
	})
}

// Approve implements DeviceFlow.
func (d *DeviceStore) Approve(ctx context.Context, userCode string, p model.Principal) error {
	return d.decide(ctx, userCode, p, "approved")
}

// Deny implements DeviceFlow.
func (d *DeviceStore) Deny(ctx context.Context, userCode string, p model.Principal) error {
	return d.decide(ctx, userCode, p, "denied")
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
// ErrSlowDown. Once approved, the first poll creates a write Token for the
// approving user and returns its secret; later polls give ErrExpiredToken.
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
	tok, secret, err := d.s.TokenStore.CreateAs(ctx, NewToken{UserID: u.ID, Name: deviceTokenName(row.ClientID), Scope: model.ScopeWrite},
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
