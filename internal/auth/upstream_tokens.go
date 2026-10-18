package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/audit"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
	"github.com/hangar-hub/hangar/internal/secrets"
)

// upstreamTokenPurpose binds upstream token ciphertexts to their use.
const upstreamTokenPurpose = "upstream_token"

// UpstreamTokenStatus says whether a user saved a token for an Upstream.
// The token itself is never returned by any API.
type UpstreamTokenStatus struct {
	Upstream  string
	Saved     bool
	UpdatedAt *time.Time
}

// UpstreamTokenStore keeps users' tokens for Upstreams (PULL-8),
// encrypted with AES-256-GCM under the Instance key. It owns the
// upstream_tokens table.
type UpstreamTokenStore struct{ s *Service }

func (ut *UpstreamTokenStore) upstreamID(ctx context.Context, name string) (uuid.UUID, error) {
	if ut.s.upstream == nil {
		return uuid.Nil, fmt.Errorf("%w: upstream %q", ErrNotFound, name)
	}
	id, err := ut.s.upstream.UpstreamID(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return uuid.Nil, err
		}
		return uuid.Nil, fmt.Errorf("%w: upstream %q: %w", ErrNotFound, name, err)
	}
	return id, nil
}

// Status reports whether the user saved a token for the Upstream.
func (ut *UpstreamTokenStore) Status(ctx context.Context, userID uuid.UUID, upstream string) (UpstreamTokenStatus, error) {
	id, err := ut.upstreamID(ctx, upstream)
	if err != nil {
		return UpstreamTokenStatus{}, err
	}
	var row db.UpstreamToken
	err = ut.s.db.NewSelect().Model(&row).Where("user_id = ? AND upstream_id = ?", userID, id).Scan(ctx)
	if db.IsNotFound(err) {
		return UpstreamTokenStatus{Upstream: upstream}, nil
	}
	if err != nil {
		return UpstreamTokenStatus{}, err
	}
	t := row.UpdatedAt
	return UpstreamTokenStatus{Upstream: upstream, Saved: true, UpdatedAt: &t}, nil
}

// Set saves (or replaces) the user's token for the Upstream.
func (ut *UpstreamTokenStore) Set(ctx context.Context, userID uuid.UUID, upstream, token string, actor model.Principal) (UpstreamTokenStatus, error) {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 4096 {
		return UpstreamTokenStatus{}, fmt.Errorf("%w: the token must be 1 to 4096 characters", ErrInvalid)
	}
	id, err := ut.upstreamID(ctx, upstream)
	if err != nil {
		return UpstreamTokenStatus{}, err
	}
	ct, err := ut.s.sealer.Seal(token, upstreamTokenPurpose)
	if err != nil {
		return UpstreamTokenStatus{}, err
	}
	row := &db.UpstreamToken{ID: db.NewID(), UserID: userID, UpstreamID: id, Ciphertext: ct}
	if _, err := ut.s.db.NewInsert().Model(row).On("CONFLICT (user_id, upstream_id) DO UPDATE").
		Set("ciphertext = EXCLUDED.ciphertext").Set("updated_at = EXCLUDED.updated_at").Exec(ctx); err != nil {
		return UpstreamTokenStatus{}, err
	}
	ut.s.record(ctx, audit.Event{Action: audit.UpstreamTokenChanged, Actor: actor, TargetType: "upstream", TargetID: upstream,
		Data: map[string]any{"change": "saved"}})
	return ut.Status(ctx, userID, upstream)
}

// Delete removes the user's token for the Upstream.
func (ut *UpstreamTokenStore) Delete(ctx context.Context, userID uuid.UUID, upstream string, actor model.Principal) error {
	id, err := ut.upstreamID(ctx, upstream)
	if err != nil {
		return err
	}
	res, err := ut.s.db.NewDelete().Model((*db.UpstreamToken)(nil)).Where("user_id = ? AND upstream_id = ?", userID, id).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: no token saved for %s", ErrNotFound, upstream)
	}
	ut.s.record(ctx, audit.Event{Action: audit.UpstreamTokenChanged, Actor: actor, TargetType: "upstream", TargetID: upstream,
		Data: map[string]any{"change": "removed"}})
	return nil
}

// Token returns the user's decrypted token for an Upstream, by Upstream
// ID, for Pulls (internal/upstream). ok is false when none is saved.
func (ut *UpstreamTokenStore) Token(ctx context.Context, userID, upstreamID uuid.UUID) (token string, ok bool, err error) {
	var row db.UpstreamToken
	err = ut.s.db.NewSelect().Model(&row).Where("user_id = ? AND upstream_id = ?", userID, upstreamID).Scan(ctx)
	if db.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	plain, err := ut.s.sealer.Open(row.Ciphertext, upstreamTokenPurpose)
	if err != nil {
		return "", false, err
	}
	return plain, true, nil
}

// ReencryptUpstreamTokens re-encrypts every stored upstream token from
// one Instance key to another inside tx, for `hangar admin rotate-key`,
// and returns how many it changed. It fails without changing anything
// if a token cannot be decrypted with from.
func ReencryptUpstreamTokens(ctx context.Context, tx bun.IDB, from, to *secrets.Sealer) (int, error) {
	var rows []db.UpstreamToken
	if err := tx.NewSelect().Model(&rows).Scan(ctx); err != nil {
		return 0, err
	}
	for _, r := range rows {
		plain, err := from.Open(r.Ciphertext, upstreamTokenPurpose)
		if err != nil {
			return 0, fmt.Errorf("upstream token %s: %w", r.ID, err)
		}
		ct, err := to.Seal(plain, upstreamTokenPurpose)
		if err != nil {
			return 0, err
		}
		if _, err := tx.NewUpdate().Model((*db.UpstreamToken)(nil)).Where("id = ?", r.ID).Set("ciphertext = ?", ct).Exec(ctx); err != nil {
			return 0, err
		}
	}
	return len(rows), nil
}
