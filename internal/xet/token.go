package xet

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/hkdf"

	"github.com/jamesagarside/hangar/internal/model"
)

// tokenPrefix starts every CAS token, so a Hub Token is never mistaken for one.
const tokenPrefix = "hxet1."

// maxTokenLength bounds what VerifyToken parses.
const maxTokenLength = 4096

// tokenSigner issues and checks CAS tokens: an HMAC-SHA256 over JSON claims,
// with a key derived from the Instance key, so any replica can verify a
// token another one issued (docs/research/xet-upload-research.md §1.3).
type tokenSigner struct {
	key []byte
}

func newTokenSigner(instanceKey []byte) (*tokenSigner, error) {
	if len(instanceKey) < 32 {
		return nil, errors.New("xet: the instance key must be at least 32 bytes")
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, instanceKey, nil, []byte("hangar xet cas token v1")), key); err != nil {
		return nil, err
	}
	return &tokenSigner{key: key}, nil
}

// tokenClaims is the signed payload. Short keys keep tokens small.
type tokenClaims struct {
	UserID   uuid.UUID `json:"u"`
	Username string    `json:"n"`
	RepoType string    `json:"t"`
	Repo     string    `json:"r"`
	RepoUUID uuid.UUID `json:"ru"`
	Revision string    `json:"rv"`
	CreatePR bool      `json:"pr,omitempty"`
	Scope    string    `json:"s"`
	Expires  int64     `json:"e"`
}

func (s *tokenSigner) mac(payload string) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *tokenSigner) issue(c Claims) (string, error) {
	b, err := json.Marshal(tokenClaims{
		UserID: c.UserID, Username: c.Username, RepoType: string(c.Repo.Type), Repo: c.Repo.String(),
		RepoUUID: c.RepoUUID, Revision: c.Revision, CreatePR: c.CreatePR, Scope: c.Scope, Expires: c.Expires.Unix(),
	})
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(b)
	return tokenPrefix + payload + "." + s.mac(payload), nil
}

// verify checks the signature and expiry. A token is valid until its
// expiry plus skew, to allow for clocks that differ between replicas.
func (s *tokenSigner) verify(token string, now time.Time, skew time.Duration) (*Claims, error) {
	if len(token) > maxTokenLength {
		return nil, ErrInvalidToken
	}
	rest, ok := strings.CutPrefix(token, tokenPrefix)
	if !ok {
		return nil, ErrInvalidToken
	}
	payload, sig, ok := strings.Cut(rest, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(s.mac(payload))) {
		return nil, ErrInvalidToken
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil, ErrInvalidToken
	}
	var tc tokenClaims
	if err := json.Unmarshal(b, &tc); err != nil {
		return nil, ErrInvalidToken
	}
	exp := time.Unix(tc.Expires, 0).UTC()
	if now.After(exp.Add(skew)) {
		return nil, fmt.Errorf("%w: expired", ErrInvalidToken)
	}
	rt, err := model.ParseRepoType(tc.RepoType)
	if err != nil {
		return nil, ErrInvalidToken
	}
	id, err := model.ParseRepoID(rt, tc.Repo)
	if err != nil {
		return nil, ErrInvalidToken
	}
	return &Claims{
		UserID: tc.UserID, Username: tc.Username, Repo: id, RepoUUID: tc.RepoUUID, Revision: tc.Revision,
		CreatePR: tc.CreatePR, Scope: tc.Scope, Expires: exp,
	}, nil
}
