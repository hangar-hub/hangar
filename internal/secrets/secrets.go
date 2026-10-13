// Package secrets encrypts secrets stored in the database (upstream Tokens,
// webhook secrets, OIDC client secrets kept in settings) with AES-256-GCM
// under a key derived from security.instance_key (docs/spec/05-auth.md
// "Secrets at rest").
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/hkdf"
)

// version prefixes every ciphertext so the format can change.
const version = "v1:"

// ErrDecrypt means a ciphertext is malformed, was made with another key,
// or was tampered with.
var ErrDecrypt = errors.New("secrets: cannot decrypt")

// Sealer encrypts and decrypts secrets. It is safe for concurrent use.
type Sealer struct{ aead cipher.AEAD }

// New derives an encryption key from the Instance key with HKDF-SHA256.
func New(instanceKey []byte) (*Sealer, error) {
	if len(instanceKey) < 32 {
		return nil, errors.New("secrets: instance key must be at least 32 bytes")
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(hkdf.New(sha256.New, instanceKey, nil, []byte("hangar secrets v1")), key); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext. purpose is bound into the ciphertext as
// additional data, so a webhook secret cannot be decrypted as an upstream
// Token.
func (s *Sealer) Seal(plaintext, purpose string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := s.aead.Seal(nonce, nonce, []byte(plaintext), []byte(purpose))
	return version + base64.RawStdEncoding.EncodeToString(out), nil
}

// Open decrypts a ciphertext made by Seal with the same purpose.
func (s *Sealer) Open(ciphertext, purpose string) (string, error) {
	enc, ok := strings.CutPrefix(ciphertext, version)
	if !ok {
		return "", fmt.Errorf("%w: unknown format", ErrDecrypt)
	}
	raw, err := base64.RawStdEncoding.DecodeString(enc)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return "", fmt.Errorf("%w: malformed", ErrDecrypt)
	}
	n := s.aead.NonceSize()
	plain, err := s.aead.Open(nil, raw[:n], raw[n:], []byte(purpose))
	if err != nil {
		return "", ErrDecrypt
	}
	return string(plain), nil
}
