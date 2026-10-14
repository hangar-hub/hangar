package auth

import (
	"context"
	"errors"
	"time"

	"github.com/jamesagarside/hangar/internal/db"
)

// CleanupResult counts the rows Cleanup removed.
type CleanupResult struct {
	Sessions, DeviceCodes, OIDCStates, OllamaNonces, LoginAttempts int64
}

// Cleanup removes expired authentication state: sessions, device codes a
// day after expiry, OIDC states, Ollama nonces, and sign-in throttle
// counters that have decayed. Expiry is enforced on read, so this only
// keeps the tables small; run it as a scheduled Job (hourly is plenty).
func (s *Service) Cleanup(ctx context.Context) (CleanupResult, error) {
	var res CleanupResult
	var errs []error
	n, err := s.Sessions.DeleteExpired(ctx)
	res.Sessions = n
	errs = append(errs, err)
	n, err = s.Device.DeleteExpired(ctx)
	res.DeviceCodes = n
	errs = append(errs, err)
	now := s.nowUTC()
	del := func(model any, where string, args ...any) int64 {
		r, err := s.db.NewDelete().Model(model).Where(where, args...).Exec(ctx)
		if err != nil {
			errs = append(errs, err)
			return 0
		}
		n, _ := r.RowsAffected()
		return n
	}
	res.OIDCStates = del((*db.OIDCState)(nil), "expires_at < ?", now)
	res.OllamaNonces = del((*db.OllamaNonce)(nil), "expires_at < ?", now)
	res.LoginAttempts = del((*db.LoginAttempt)(nil), "last_failure_at < ? AND (locked_until IS NULL OR locked_until < ?)", now.Add(-throttleDecay), now)
	return res, errors.Join(errs...)
}

// cleanupInterval is the suggested schedule for Cleanup.
const cleanupInterval = time.Hour

// CleanupInterval returns how often Cleanup should run.
func CleanupInterval() time.Duration { return cleanupInterval }
