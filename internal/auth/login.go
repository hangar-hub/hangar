package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/hangar-hub/hangar/internal/audit"
	"github.com/hangar-hub/hangar/internal/db"
	"github.com/hangar-hub/hangar/internal/model"
)

// Login throttling (docs/spec/05-auth.md "Local accounts"): after 5
// failures for a username or an IP address, each further attempt waits
// twice as long as the last, from 1 second up to 15 minutes. Counters
// reset an hour after the last failure, and a username's counter resets
// on success. They live in the database so every replica sees them. Each
// attempt counts before its password is hashed (reserve), so parallel
// guesses cannot outrun the count.
//
// The username's counter does not apply from an IP address the User has
// signed in from with a password in the last knownClientTTL: there only
// the IP address's counter does. So anyone can slow down guesses against
// an account from new addresses, but nobody can lock its owner out from
// where they usually sign in by sending wrong passwords for it.
const (
	throttleFree     = 5
	throttleBase     = time.Second
	throttleMax      = 15 * time.Minute
	throttleDecay    = time.Hour
	maxUsernameInput = 256
	knownClientTTL   = 90 * 24 * time.Hour
)

// Login errors.
var (
	// ErrLoginFailed is the one message for a wrong username or password,
	// a disabled account or an account without a password.
	ErrLoginFailed = fmt.Errorf("%w: wrong username or password", ErrInvalidCredentials)
	// ErrLocalLoginDisabled means password sign-in is off and the user is
	// not the bootstrap admin.
	ErrLocalLoginDisabled = errors.New("password sign-in is turned off; use your organisation's sign-in provider")
)

// ThrottledError means too many recent failures; try again after RetryAfter.
type ThrottledError struct{ RetryAfter time.Duration }

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("too many failed sign-in attempts; try again in %s", e.RetryAfter.Round(time.Second))
}

// Message is the error as a sentence for users, as problem details carry it.
func (e *ThrottledError) Message() string {
	return fmt.Sprintf("Too many failed sign-in attempts. Try again in %s.", e.RetryAfter.Round(time.Second))
}

// LoginService runs password sign-in.
type LoginService struct{ s *Service }

// LoginAttempt is a password sign-in attempt.
type LoginAttempt struct {
	Username  string
	Password  string
	IP        string
	UserAgent string
}

// userThrottleKey is the throttle key of a username.
func userThrottleKey(username string) string { return "user:" + strings.ToLower(username) }

// ipThrottleKey is the throttle key of an IP address.
func ipThrottleKey(ip string) string { return "ip:" + ip }

// throttleKeys returns the keys an attempt counts against: the IP
// address's, and the username's unless the IP address is one the User
// has signed in from before (known).
func throttleKeys(username, ip string, known bool) []string {
	var keys []string
	if !known || ip == "" {
		keys = append(keys, userThrottleKey(username))
	}
	if ip != "" {
		keys = append(keys, ipThrottleKey(ip))
	}
	return keys
}

// keysFor returns the throttle keys for an attempt to sign in as username
// from ip; u is the User with that username, or nil if there is none.
func (l *LoginService) keysFor(ctx context.Context, u *User, username, ip string) ([]string, error) {
	known := false
	if u != nil && ip != "" {
		n, err := l.s.db.NewSelect().Model((*db.LoginKnownClient)(nil)).
			Where("user_id = ? AND ip = ? AND last_success_at > ?", u.ID, ip, l.s.nowUTC().Add(-knownClientTTL)).Count(ctx)
		if err != nil {
			return nil, err
		}
		known = n > 0
	}
	return throttleKeys(username, ip, known), nil
}

// rememberClient records that u signed in with a password from ip.
func (l *LoginService) rememberClient(ctx context.Context, u *User, ip string) error {
	if ip == "" {
		return nil
	}
	now := l.s.nowUTC()
	row := &db.LoginKnownClient{UserID: u.ID, IP: ip, LastSuccessAt: now}
	_, err := l.s.db.NewInsert().Model(row).
		On("CONFLICT (user_id, ip) DO UPDATE").Set("last_success_at = EXCLUDED.last_success_at").Set("updated_at = EXCLUDED.updated_at").
		Exec(ctx)
	return err
}

// ThrottleTarget names the throttle counters ClearThrottle removes: a
// username's, an IP address's, or with All every counter.
type ThrottleTarget struct {
	Username string
	IP       string
	All      bool
}

// ClearThrottle removes sign-in throttle counters, so a locked-out user or
// address can sign in again at once (`hangar admin unlock-sign-in`), and
// records a login.throttle_cleared event. It returns how many counters
// it removed.
func (l *LoginService) ClearThrottle(ctx context.Context, t ThrottleTarget, actor model.Principal) (int64, error) {
	q := l.s.db.NewDelete().Model((*db.LoginAttempt)(nil))
	var keys []string
	switch {
	case t.All:
		q = q.Where("1 = 1")
	case t.Username == "" && t.IP == "":
		return 0, fmt.Errorf("%w: name a username, an IP address, or all", ErrInvalid)
	default:
		if t.Username != "" {
			keys = append(keys, userThrottleKey(t.Username))
		}
		if t.IP != "" {
			keys = append(keys, ipThrottleKey(t.IP))
		}
		q = q.Where("key IN (?)", bun.List(keys))
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	data := map[string]any{"removed": n}
	switch {
	case t.All:
		data["all"] = true
	default:
		if t.Username != "" {
			data["username"] = t.Username
		}
		if t.IP != "" {
			data["ip"] = t.IP
		}
	}
	l.s.record(ctx, audit.Event{Action: audit.LoginThrottleCleared, Actor: actor, TargetType: "login_throttle", TargetID: strings.Join(keys, ","), Data: data})
	return n, nil
}

// backoff returns the lock time after n failures.
func backoff(n int) time.Duration {
	if n < throttleFree {
		return 0
	}
	d := throttleBase
	for i := throttleFree; i < n && d < throttleMax; i++ {
		d *= 2
	}
	return min(d, throttleMax)
}

// throttled returns how long the caller must wait, or zero. It only reads,
// so it refuses a locked caller cheaply; reserve checks again atomically.
func (l *LoginService) throttled(ctx context.Context, keys []string) (time.Duration, error) {
	var rows []db.LoginAttempt
	if err := l.s.db.NewSelect().Model(&rows).Where("key IN (?)", bun.List(keys)).Scan(ctx); err != nil {
		return 0, err
	}
	now := l.s.nowUTC()
	var wait time.Duration
	for _, r := range rows {
		if r.LockedUntil != nil && now.Before(*r.LockedUntil) {
			wait = max(wait, r.LockedUntil.Sub(now))
		}
	}
	return wait, nil
}

// attempt is a password check that reserve counted as a failure before
// it ran. release takes the count back when the password was right.
type attempt struct {
	keys []string
	// at is when the attempt was counted.
	at time.Time
	// prev holds each key's row as it was before the attempt counted, so
	// release can restore the lock and decay times this attempt set.
	prev map[string]db.LoginAttempt
	// set holds the lock time the attempt set on each key, if any.
	set map[string]*time.Time
}

// lockAttemptRows returns the throttle rows for keys, creating missing
// ones, locked for the rest of tx. SQLite transactions already hold the
// write lock (BEGIN IMMEDIATE); on Postgres the rows are locked with FOR
// UPDATE, in key order so two transactions cannot deadlock.
func (l *LoginService) lockAttemptRows(ctx context.Context, tx bun.Tx, keys []string) (map[string]db.LoginAttempt, error) {
	sorted := slices.Clone(keys)
	slices.Sort(sorted)
	for _, k := range sorted {
		if _, err := tx.NewInsert().Model(&db.LoginAttempt{Key: k}).On("CONFLICT (key) DO NOTHING").Exec(ctx); err != nil {
			return nil, err
		}
	}
	var rows []db.LoginAttempt
	q := tx.NewSelect().Model(&rows).Where("key IN (?)", bun.List(sorted)).Order("key")
	if !l.s.db.IsSQLite() {
		q = q.For("UPDATE")
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	out := make(map[string]db.LoginAttempt, len(rows))
	for _, r := range rows {
		out[r.Key] = r
	}
	return out, nil
}

func saveAttemptRow(ctx context.Context, tx bun.Tx, row *db.LoginAttempt) error {
	_, err := tx.NewUpdate().Model(row).Column("failures", "last_failure_at", "locked_until", "updated_at").WherePK().Exec(ctx)
	return err
}

// reserve checks the throttle and, in the same transaction, counts the
// attempt as a failure against every key before the password is checked,
// so a burst of parallel guesses cannot all pass the check while the
// first ones are still hashing: once five are counted, the next is
// refused until the lock ends. It returns a ThrottledError while any key
// is locked. A right password gives the count back with release.
func (l *LoginService) reserve(ctx context.Context, keys []string) (*attempt, error) {
	now := l.s.nowUTC()
	a := &attempt{keys: keys, at: now, prev: map[string]db.LoginAttempt{}, set: map[string]*time.Time{}}
	err := l.s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		rows, err := l.lockAttemptRows(ctx, tx, keys)
		if err != nil {
			return err
		}
		var wait time.Duration
		for _, r := range rows {
			if r.LockedUntil != nil && now.Before(*r.LockedUntil) {
				wait = max(wait, r.LockedUntil.Sub(now))
			}
		}
		if wait > 0 {
			return &ThrottledError{RetryAfter: wait}
		}
		for _, k := range keys {
			row := rows[k]
			a.prev[k] = row
			if row.LastFailureAt != nil && now.Sub(*row.LastFailureAt) > throttleDecay {
				row.Failures = 0
			}
			row.Failures++
			row.LastFailureAt = &now
			if d := backoff(row.Failures); d > 0 {
				until := now.Add(d)
				row.LockedUntil = &until
				a.set[k] = &until
			}
			if err := saveAttemptRow(ctx, tx, &row); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return a, nil
}

// confirm records that a reserved attempt failed, after its password was
// checked. reserve set any lock from when the attempt was counted, before
// the hash, which takes long enough on a slow machine to use up the first
// one-second lock, so the lock now runs from the failure instead, as long
// as each key's count calls for. It never shortens a lock.
func (l *LoginService) confirm(ctx context.Context, a *attempt) error {
	now := l.s.nowUTC()
	return l.s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		rows, err := l.lockAttemptRows(ctx, tx, a.keys)
		if err != nil {
			return err
		}
		for _, k := range a.keys {
			row := rows[k]
			d := backoff(row.Failures)
			if d == 0 {
				continue
			}
			until := now.Add(d)
			if row.LockedUntil != nil && !row.LockedUntil.Before(until) {
				continue
			}
			row.LockedUntil = &until
			row.UpdatedAt = now
			if err := saveAttemptRow(ctx, tx, &row); err != nil {
				return err
			}
		}
		return nil
	})
}

// release takes back the failure reserve counted, after a right password.
// The lock and decay times go back to what they were, unless another
// attempt has changed them since.
func (l *LoginService) release(ctx context.Context, a *attempt) error {
	return l.s.db.InTx(ctx, func(ctx context.Context, tx bun.Tx) error {
		rows, err := l.lockAttemptRows(ctx, tx, a.keys)
		if err != nil {
			return err
		}
		for _, k := range a.keys {
			row := rows[k]
			row.Failures = max(row.Failures-1, 0)
			prev := a.prev[k]
			if row.LastFailureAt != nil && row.LastFailureAt.Equal(a.at) {
				row.LastFailureAt = prev.LastFailureAt
			}
			if set := a.set[k]; set != nil && row.LockedUntil != nil && row.LockedUntil.Equal(*set) {
				row.LockedUntil = prev.LockedUntil
			}
			if row.Failures == 0 && row.LastFailureAt == nil && row.LockedUntil == nil {
				// The row only held this attempt: remove it, as Cleanup
				// removes rows by their last failure and would miss it.
				if _, err := tx.NewDelete().Model((*db.LoginAttempt)(nil)).Where("key = ?", k).Exec(ctx); err != nil {
					return err
				}
				continue
			}
			if err := saveAttemptRow(ctx, tx, &row); err != nil {
				return err
			}
		}
		return nil
	})
}

// Password checks a username and password and returns the User. It never
// says which part was wrong. It records login.succeeded or login.failed.
func (l *LoginService) Password(ctx context.Context, a LoginAttempt) (*User, error) {
	u, err := l.password(ctx, a)
	ev := audit.Event{IP: a.IP, UserAgent: a.UserAgent, TargetType: "user", TargetID: truncate(a.Username, maxUsernameInput)}
	if err != nil {
		var te *ThrottledError
		reason := "wrong_credentials"
		switch {
		case errors.As(err, &te):
			reason = "throttled"
		case errors.Is(err, ErrLocalLoginDisabled):
			reason = "local_login_disabled"
		case errors.Is(err, ErrDisabled):
			reason = "disabled"
			err = ErrLoginFailed
		case !errors.Is(err, ErrInvalidCredentials):
			return nil, err
		}
		ev.Action, ev.Actor = audit.LoginFailed, model.Anonymous()
		ev.Data = map[string]any{"method": string(model.AuthPassword), "reason": reason}
		l.s.record(ctx, ev)
		l.s.countAttempt(ctx, model.AuthPassword, "failure")
		return nil, err
	}
	ev.Action, ev.Actor = audit.LoginSucceeded, u.Principal(model.AuthPassword)
	ev.TargetID = u.Username
	ev.Data = map[string]any{"method": string(model.AuthPassword)}
	l.s.record(ctx, ev)
	l.s.countAttempt(ctx, model.AuthPassword, "success")
	return u, nil
}

func (l *LoginService) password(ctx context.Context, a LoginAttempt) (*User, error) {
	if a.Username == "" || a.Password == "" || len(a.Username) > maxUsernameInput {
		return nil, ErrLoginFailed
	}
	u, err := l.s.Users.GetByUsername(ctx, a.Username)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	keys, err := l.keysFor(ctx, u, a.Username, a.IP)
	if err != nil {
		return nil, err
	}
	wait, err := l.throttled(ctx, keys)
	if err != nil {
		return nil, err
	}
	if wait > 0 {
		return nil, &ThrottledError{RetryAfter: wait}
	}
	st, err := l.s.Settings.Get(ctx)
	if err != nil {
		return nil, err
	}
	if !st.LocalLogin && !strings.EqualFold(a.Username, l.s.cfg.Security.BootstrapAdmin.Username) {
		return nil, ErrLocalLoginDisabled
	}
	// The attempt counts as a failure before the hash, and only a right
	// password takes it back: see reserve.
	att, err := l.reserve(ctx, keys)
	if err != nil {
		return nil, err
	}
	ok := false
	if u != nil && u.HasPassword && !u.IsSystem {
		ok, err = VerifyPassword(ctx, u.passwordHash, a.Password)
		if err != nil && ctx.Err() != nil {
			return nil, err
		}
		if err != nil {
			l.s.log.ErrorContext(ctx, "stored password hash is unreadable", "user_id", u.ID.String())
			ok = false
		}
	} else if err := timingSafeDummyVerify(ctx, a.Password); err != nil {
		return nil, err
	}
	if !ok {
		if err := l.confirm(ctx, att); err != nil {
			return nil, err
		}
		return nil, ErrLoginFailed
	}
	if err := l.release(ctx, att); err != nil {
		return nil, err
	}
	if u.Disabled() {
		return nil, ErrDisabled
	}
	if _, err := l.s.db.NewDelete().Model((*db.LoginAttempt)(nil)).Where("key = ?", userThrottleKey(a.Username)).Exec(ctx); err != nil {
		return nil, err
	}
	if err := l.rememberClient(ctx, u, a.IP); err != nil {
		return nil, err
	}
	return u, nil
}
