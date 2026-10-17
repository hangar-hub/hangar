package httpx

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/hkdf"
)

// Query parameters a signed URL carries.
const (
	SigParam    = "sig"
	ExpiryParam = "exp"
	ScopeParam  = "scope"
)

// Signer signs and verifies URLs that must authorise themselves, such as
// LFS upload and part URLs, which Hugging Face clients call without an
// Authorization header (docs/spec/04-git.md). A signature covers the path,
// the scope, the expiry and every other query parameter.
type Signer struct {
	key []byte
	now func() time.Time
}

// ErrBadSignature is returned for a missing, malformed, wrong or expired signature.
var ErrBadSignature = errors.New("invalid or expired signed URL")

// NewSigner derives a URL-signing key from the Instance key with HKDF, so
// the Instance key itself never signs anything directly.
func NewSigner(instanceKey []byte) (*Signer, error) {
	if len(instanceKey) < 32 {
		return nil, errors.New("instance key must be at least 32 bytes")
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, instanceKey, nil, []byte("hangar url signer v1")), key); err != nil {
		return nil, err
	}
	return &Signer{key: key, now: time.Now}, nil
}

// WithClock returns a copy of the Signer that uses now for expiry checks.
func (s *Signer) WithClock(now func() time.Time) *Signer {
	return &Signer{key: s.key, now: now}
}

// Sign adds scope, expiry and signature parameters to rawURL. scope names
// what the URL may do, for example `lfs-upload:<oid>`.
func (s *Signer) Sign(rawURL, scope string, ttl time.Duration) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Del(SigParam)
	q.Set(ScopeParam, scope)
	q.Set(ExpiryParam, strconv.FormatInt(s.now().Add(ttl).Unix(), 10))
	q.Set(SigParam, s.mac(u.EscapedPath(), q))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Verify checks the signature of a request URL and that it has not expired,
// and returns its scope.
func (s *Signer) Verify(u *url.URL) (string, error) {
	q := u.Query()
	sig := q.Get(SigParam)
	if sig == "" {
		return "", ErrBadSignature
	}
	if !hmac.Equal([]byte(sig), []byte(s.mac(u.EscapedPath(), q))) {
		return "", ErrBadSignature
	}
	exp, err := strconv.ParseInt(q.Get(ExpiryParam), 10, 64)
	if err != nil || s.now().Unix() > exp {
		return "", fmt.Errorf("%w: expired", ErrBadSignature)
	}
	return q.Get(ScopeParam), nil
}

func (s *Signer) mac(escapedPath string, q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		if k != SigParam {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	// Keys and values are query-escaped, so a value that contains a
	// newline or `=` cannot pass for another parameter: otherwise a signed
	// URL with `a=x%0Aexp%3D<later>` would also sign `a=x&exp=<later>`.
	var b strings.Builder
	b.WriteString(escapedPath)
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			b.WriteString("\n" + url.QueryEscape(k) + "=" + url.QueryEscape(v))
		}
	}
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(b.String()))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
