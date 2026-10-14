package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// registryAudience is the audience of Ollama registry tokens.
const registryAudience = "hangar-ollama-registry"

// registryClaims are the claims of an Ollama registry token.
type registryClaims struct {
	Issuer    string `json:"iss"`
	Audience  string `json:"aud"`
	Subject   string `json:"sub"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
	ID        string `json:"jti"`
	// Repo is the Repo the token reads, as `type:namespace/name`.
	Repo string `json:"repo"`
}

var errBadJWT = errors.New("malformed or invalid registry token")

var jwtHeader = base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))

// signJWT returns an HS256 JWT. Hangar both issues and verifies these
// tokens, so a shared key derived from the Instance key is enough.
func signJWT(key []byte, c registryClaims) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signing := jwtHeader + "." + base64.RawURLEncoding.EncodeToString(payload)
	m := hmac.New(sha256.New, key)
	m.Write([]byte(signing))
	return signing + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil)), nil
}

// looksLikeJWT reports whether s has three dot-separated parts.
func looksLikeJWT(s string) bool { return strings.Count(s, ".") == 2 && len(s) < 4096 }

// verifyJWT checks the header, signature, audience, issuer and expiry.
func verifyJWT(key []byte, token, issuer string, now time.Time) (registryClaims, error) {
	var c registryClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != jwtHeader {
		return c, errBadJWT
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return c, errBadJWT
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, m.Sum(nil)) {
		return c, errBadJWT
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(payload, &c) != nil {
		return c, errBadJWT
	}
	if c.Audience != registryAudience || c.Issuer != issuer || c.Subject == "" || c.Repo == "" {
		return c, errBadJWT
	}
	if now.Unix() >= c.ExpiresAt {
		return c, errors.New("registry token expired")
	}
	return c, nil
}
