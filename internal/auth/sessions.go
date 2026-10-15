package auth

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jamesagarside/hangar/internal/audit"
	"github.com/jamesagarside/hangar/internal/db"
	"github.com/jamesagarside/hangar/internal/httpx"
	"github.com/jamesagarside/hangar/internal/model"
)

// Cookie and header names.
const (
	// SessionCookie holds the session secret. It is HttpOnly.
	SessionCookie = "hangar_session"
	// CSRFCookie holds the CSRF token for the double-submit check. The UI
	// reads it (or the csrf_token field of GET /me) and sends it back in
	// CSRFHeader.
	CSRFCookie = "hangar_csrf"
	// CSRFHeader carries the CSRF token on state-changing calls.
	CSRFHeader = "X-CSRF-Token"
)

// sessionTouchInterval limits last_seen_at writes.
const sessionTouchInterval = time.Minute

// Session is a UI session. The cookie secret is never stored.
type Session struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	CSRFToken  string
	UserAgent  string
	IP         string
	CreatedAt  time.Time
	LastSeenAt *time.Time
	ExpiresAt  time.Time
}

// SessionStore manages UI sessions. It owns the sessions table.
type SessionStore struct{ s *Service }

func toSession(r *db.Session) Session {
	return Session{ID: r.ID, UserID: r.UserID, CSRFToken: r.CSRFSecret, UserAgent: r.UserAgent, IP: r.IP,
		CreatedAt: r.CreatedAt, LastSeenAt: r.LastSeenAt, ExpiresAt: r.ExpiresAt}
}

// Create starts a session for a User and returns it with the cookie secret.
func (ss *SessionStore) Create(ctx context.Context, userID uuid.UUID, userAgent, ip string) (*Session, string, error) {
	secret, err := randomString(32)
	if err != nil {
		return nil, "", err
	}
	csrf, err := randomString(32)
	if err != nil {
		return nil, "", err
	}
	now := ss.s.nowUTC()
	row := &db.Session{ID: db.NewID(), IDHash: hashSecret(secret), UserID: userID, CSRFSecret: csrf,
		ExpiresAt: now.Add(ss.s.cfg.Security.SessionTTL), LastSeenAt: &now, UserAgent: truncate(userAgent, 512), IP: ip}
	if _, err := ss.s.db.NewInsert().Model(row).Exec(ctx); err != nil {
		return nil, "", err
	}
	sess := toSession(row)
	return &sess, secret, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// lookup returns the live session for a cookie secret.
func (ss *SessionStore) lookup(ctx context.Context, secret string) (*Session, error) {
	var row db.Session
	err := ss.s.db.NewSelect().Model(&row).Where("id_hash = ?", hashSecret(secret)).Scan(ctx)
	if db.IsNotFound(err) {
		return nil, fmt.Errorf("%w: unknown session", ErrInvalidCredentials)
	}
	if err != nil {
		return nil, err
	}
	now := ss.s.nowUTC()
	if !now.Before(row.ExpiresAt) {
		return nil, fmt.Errorf("%w: expired session", ErrInvalidCredentials)
	}
	if row.LastSeenAt == nil || now.Sub(*row.LastSeenAt) >= sessionTouchInterval {
		if _, err := ss.s.db.NewUpdate().Model((*db.Session)(nil)).Where("id = ?", row.ID).Set("last_seen_at = ?", now).Exec(ctx); err != nil {
			ss.s.log.WarnContext(ctx, "record session use", "error", err)
		}
		row.LastSeenAt = &now
	}
	sess := toSession(&row)
	return &sess, nil
}

// Get returns a session by ID.
func (ss *SessionStore) Get(ctx context.Context, id uuid.UUID) (*Session, error) {
	var row db.Session
	err := ss.s.db.NewSelect().Model(&row).Where("id = ?", id).Scan(ctx)
	if db.IsNotFound(err) {
		return nil, fmt.Errorf("%w: session", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	sess := toSession(&row)
	return &sess, nil
}

// List returns a User's live sessions, newest first.
func (ss *SessionStore) List(ctx context.Context, userID uuid.UUID) ([]Session, error) {
	var rows []db.Session
	err := ss.s.db.NewSelect().Model(&rows).Where("user_id = ? AND expires_at > ?", userID, ss.s.nowUTC()).
		OrderExpr("created_at DESC, id DESC").Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Session, 0, len(rows))
	for i := range rows {
		out = append(out, toSession(&rows[i]))
	}
	return out, nil
}

// Revoke ends one of a User's sessions. Another user's session ID gives
// ErrNotFound.
func (ss *SessionStore) Revoke(ctx context.Context, userID, id uuid.UUID) error {
	res, err := ss.s.db.NewDelete().Model((*db.Session)(nil)).Where("id = ? AND user_id = ?", id, userID).Exec(ctx)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: session", ErrNotFound)
	}
	return nil
}

// RevokeAll ends every session of a User.
func (ss *SessionStore) RevokeAll(ctx context.Context, userID uuid.UUID) error {
	_, err := ss.s.db.NewDelete().Model((*db.Session)(nil)).Where("user_id = ?", userID).Exec(ctx)
	return err
}

// DeleteExpired removes expired sessions and returns how many it removed.
func (ss *SessionStore) DeleteExpired(ctx context.Context) (int64, error) {
	res, err := ss.s.db.NewDelete().Model((*db.Session)(nil)).Where("expires_at <= ?", ss.s.nowUTC()).Exec(ctx)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetCookies writes the session and CSRF cookies: HttpOnly for the
// session, Secure when public_url is HTTPS, SameSite=Lax. Secure follows
// public_url (05-auth.md) because a plain-HTTP LAN Instance cannot set
// Secure cookies, and the CSRF cookie is readable by the UI on purpose.
func (s *Service) SetCookies(w http.ResponseWriter, sess *Session, secret string) {
	maxAge := int(sess.ExpiresAt.Sub(s.now()).Seconds())
	s.setCookie(w, SessionCookie, secret, maxAge, sess.ExpiresAt, true)
	s.setCookie(w, CSRFCookie, sess.CSRFToken, maxAge, sess.ExpiresAt, false)
}

// ClearCookies removes the session and CSRF cookies.
func (s *Service) ClearCookies(w http.ResponseWriter) {
	s.setCookie(w, SessionCookie, "", -1, time.Unix(0, 0), true)
	s.setCookie(w, CSRFCookie, "", -1, time.Unix(0, 0), false)
}

func (s *Service) setCookie(w http.ResponseWriter, name, value string, maxAge int, expires time.Time, httpOnly bool) {
	// A response sets each cookie once (RFC 6265, section 4.1.1): signing
	// in on a request whose stale cookie Middleware cleared replaces the
	// clearing header.
	h := w.Header()
	var kept []string
	for _, v := range h.Values("Set-Cookie") {
		if !strings.HasPrefix(v, name+"=") {
			kept = append(kept, v)
		}
	}
	h.Del("Set-Cookie")
	for _, v := range kept {
		h.Add("Set-Cookie", v)
	}
	c := &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: maxAge, Expires: expires, SameSite: http.SameSiteLaxMode} //nolint:gosec // Secure and HttpOnly are set below.
	c.Secure = s.secureCookies()
	c.HttpOnly = httpOnly
	http.SetCookie(w, c)
}

// StartSession creates a session for u, sets the cookies and returns the
// session. Handlers call it after a successful sign-in. The session the
// request's cookie names, if any, ends: the browser's cookie is replaced,
// so nothing could use it again.
func (s *Service) StartSession(ctx context.Context, w http.ResponseWriter, r *http.Request, u *User) (*Session, error) {
	sess, secret, err := s.Sessions.Create(ctx, u.ID, r.UserAgent(), httpx.ClientIP(r))
	if err != nil {
		return nil, err
	}
	s.deleteRequestSession(ctx, r)
	s.SetCookies(w, sess, secret)
	s.Users.touchLogin(ctx, u.ID)
	return sess, nil
}

// EndSession deletes the request's session, if any, and clears the cookies.
func (s *Service) EndSession(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	s.deleteRequestSession(ctx, r)
	s.ClearCookies(w)
}

// deleteRequestSession deletes the session the request's cookie names.
func (s *Service) deleteRequestSession(ctx context.Context, r *http.Request) {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return
	}
	if _, err := s.db.NewDelete().Model((*db.Session)(nil)).Where("id_hash = ?", hashSecret(c.Value)).Exec(ctx); err != nil {
		s.log.WarnContext(ctx, "delete session", "error", err)
	}
}

// CurrentSession returns the session behind a session-authenticated
// Principal.
func (s *Service) CurrentSession(ctx context.Context, p model.Principal) (*Session, error) {
	if p.Method != model.AuthSession || p.SessionID == uuid.Nil {
		return nil, fmt.Errorf("%w: not a session", ErrNotFound)
	}
	return s.Sessions.Get(ctx, p.SessionID)
}

// checkCSRF compares the CSRF header with the session's token in constant time.
func checkCSRF(r *http.Request, sess *Session) bool {
	got := r.Header.Get(CSRFHeader)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(sess.CSRFToken)) == 1
}

// RecordLogout records a logout audit event.
func (s *Service) RecordLogout(ctx context.Context, p model.Principal, ip, userAgent string) {
	s.record(ctx, audit.Event{Action: audit.Logout, Actor: p, TargetType: "user", TargetID: p.Username, IP: ip, UserAgent: userAgent})
}
