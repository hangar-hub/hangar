package auth

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Password hashing parameters from docs/spec/05-auth.md: argon2id with
// 64 MiB of memory, 3 passes and 2 threads. They are stored in every hash
// string, so they can change without breaking existing hashes.
const (
	argonMemoryKiB = 64 * 1024
	argonTime      = 3
	argonThreads   = 2
	argonKeyLen    = 32
	argonSaltLen   = 16

	// MinPasswordLength is the shortest password Hangar accepts.
	MinPasswordLength = 10
	// maxPasswordLength stops very long inputs from costing hashing time.
	maxPasswordLength = 1024
)

// ErrWeakPassword means a new password breaks the password rules.
var ErrWeakPassword = errors.New("weak password")

//go:embed common_passwords.txt
var commonPasswordsFile []byte

var (
	commonOnce      sync.Once
	commonPasswords map[string]struct{}
)

func loadCommonPasswords() {
	commonPasswords = make(map[string]struct{}, 2400)
	sc := bufio.NewScanner(bytes.NewReader(commonPasswordsFile))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		commonPasswords[strings.ToLower(line)] = struct{}{}
	}
}

// IsCommonPassword reports whether pw is in the bundled list of common
// passwords, ignoring case.
func IsCommonPassword(pw string) bool {
	commonOnce.Do(loadCommonPasswords)
	_, ok := commonPasswords[strings.ToLower(pw)]
	return ok
}

// CheckPasswordPolicy checks a new password: at least 10 characters, not
// longer than 1024 bytes, and not a common password. username, if given,
// may not be the password.
func CheckPasswordPolicy(pw, username string) error {
	switch {
	case utf8.RuneCountInString(pw) < MinPasswordLength:
		return fmt.Errorf("%w: use at least %d characters", ErrWeakPassword, MinPasswordLength)
	case len(pw) > maxPasswordLength:
		return fmt.Errorf("%w: use at most %d bytes", ErrWeakPassword, maxPasswordLength)
	case IsCommonPassword(pw):
		return fmt.Errorf("%w: this password is too common; choose another", ErrWeakPassword)
	case username != "" && strings.EqualFold(pw, username):
		return fmt.Errorf("%w: the password cannot be the username", ErrWeakPassword)
	}
	return nil
}

// HashPassword returns an argon2id hash in the PHC string format:
// `$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>`.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// errBadHash means a stored hash cannot be parsed.
var errBadHash = errors.New("unsupported password hash")

// VerifyPassword reports whether pw matches the stored hash, using the
// parameters recorded in the hash. It compares in constant time.
func VerifyPassword(hash, pw string) (bool, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errBadHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errBadHash
	}
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false, errBadHash
	}
	// Refuse parameters that would let a bad row exhaust memory or CPU.
	if memory == 0 || memory > 1024*1024 || time == 0 || time > 20 || threads == 0 {
		return false, errBadHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, errBadHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 || len(want) > 128 {
		return false, errBadHash
	}
	if len(pw) > maxPasswordLength {
		return false, nil
	}
	got := argon2.IDKey([]byte(pw), salt, time, memory, threads, uint32(len(want))) //nolint:gosec // len(want) is at most 128.
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is verified when a username does not exist, so a failed login
// takes the same time whether or not the user exists.
var (
	dummyOnce sync.Once
	dummyHash string
)

func timingSafeDummyVerify(pw string) {
	dummyOnce.Do(func() {
		h, err := HashPassword("hangar-dummy-password-for-timing")
		if err == nil {
			dummyHash = h
		}
	})
	if dummyHash != "" {
		_, _ = VerifyPassword(dummyHash, pw)
	}
}
