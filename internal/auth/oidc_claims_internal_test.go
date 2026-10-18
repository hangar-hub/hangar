package auth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/config"
)

func TestMapClaims(t *testing.T) {
	cfg := config.OIDCProvider{Claims: map[string]string{"username": "login", "groups": "realm_access.roles", "name": "display"}}
	c := mapClaims(cfg, map[string]any{
		"sub": "123", "login": "jdoe", "email": "j@example.com", "email_verified": "true", "display": "J Doe",
		"realm_access": map[string]any{"roles": []any{"/ml-team", "admins", 7}},
	})
	require.Equal(t, "123", c.Subject)
	require.Equal(t, "jdoe", c.Username)
	require.Equal(t, "J Doe", c.Name)
	require.True(t, c.EmailVerified)
	require.True(t, c.HasGroups)
	require.Equal(t, []string{"ml-team", "admins"}, c.Groups, "nested claims, leading slashes stripped, non-strings skipped")

	c = mapClaims(config.OIDCProvider{}, map[string]any{"sub": 42.0, "preferred_username": "x", "groups": "a, b c", "email_verified": true})
	require.Equal(t, "42", c.Subject, "numeric subjects")
	require.Equal(t, []string{"a", "b", "c"}, c.Groups, "space or comma separated groups")
	require.True(t, c.EmailVerified)

	c = mapClaims(config.OIDCProvider{}, map[string]any{"sub": "s", "groups": map[string]any{"x": 1}})
	require.False(t, c.HasGroups, "an unusable groups claim counts as none")
	c = mapClaims(config.OIDCProvider{}, map[string]any{"sub": "s"})
	require.False(t, c.HasGroups)
	_, ok := lookupClaim(map[string]any{"a": "x"}, "a.b")
	require.False(t, ok)
}

func TestUsernameBase(t *testing.T) {
	for want, c := range map[string]identityClaims{
		"jdoe":            {Username: "jdoe"},
		"Jane-Doe":        {Username: "Jane Doe"},
		"jane.doe":        {Email: "jane.doe@example.com"},
		"Full-Name":       {Name: "Full Name!"},
		"user":            {Username: "!", Name: "?"},
		"abc":             {Username: "--abc"},
		"0123456789abcde": {Username: "0123456789abcde"},
	} {
		require.Equal(t, want, usernameBase(c))
	}
	require.Len(t, usernameBase(identityClaims{Username: "a123456789b123456789c123456789d123456789e1234"}), 36)
}

func TestSafeRedirect(t *testing.T) {
	for in, want := range map[string]string{
		"":                     "/",
		"/ok/path?x=1":         "/ok/path?x=1",
		"//evil.example":       "/",
		"/\\evil.example":      "/",
		"https://evil.example": "/",
		"/a\r\nSet-Cookie: x":  "/",
		"relative":             "/",
	} {
		require.Equal(t, want, SafeRedirect(in), in)
	}
}

func TestBackoff(t *testing.T) {
	require.Zero(t, backoff(4))
	require.Equal(t, throttleBase, backoff(5))
	require.Equal(t, 8*throttleBase, backoff(8))
	require.Equal(t, throttleMax, backoff(100))
	require.Contains(t, (&ThrottledError{RetryAfter: throttleMax}).Error(), "15m0s")
}

func TestJWTRejects(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	tok, err := signJWT(key, registryClaims{Issuer: "i", Audience: registryAudience, Subject: "s", Repo: "model:a/b", ExpiresAt: 2_000_000_000})
	require.NoError(t, err)
	_, err = verifyJWT(key, tok, "i", timeUnix(1_900_000_000))
	require.NoError(t, err)
	for name, c := range map[string]struct {
		token, issuer string
		now           int64
	}{
		"wrong issuer": {tok, "other", 1_900_000_000},
		"expired":      {tok, "i", 2_000_000_000},
		"two parts":    {"a.b", "i", 0},
		"bad header":   {"x" + tok, "i", 0},
		"bad sig":      {tok[:len(tok)-2] + "AA", "i", 0},
		"bad base64":   {jwtHeader + ".!!!.!!!", "i", 0},
	} {
		_, err := verifyJWT(key, c.token, c.issuer, timeUnix(c.now))
		require.Error(t, err, name)
	}
	other, _ := signJWT(key, registryClaims{Issuer: "i", Audience: "else", Subject: "s", Repo: "r", ExpiresAt: 2_000_000_000})
	_, err = verifyJWT(key, other, "i", timeUnix(0))
	require.Error(t, err, "wrong audience")
}

func timeUnix(s int64) time.Time { return time.Unix(s, 0) }
