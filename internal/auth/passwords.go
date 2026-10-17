package auth

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
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

// Each argon2id computation holds argonMemoryKiB of memory until it ends,
// and an unauthenticated login costs one, so without a bound a burst of
// sign-in attempts runs a replica out of memory (40 at once took a 1 GiB
// replica down). hashSlots lets at most maxHashes run at once in this
// process: one per argonThreads CPUs, and never more than 2, so password
// hashing uses at most 128 MiB however many requests arrive. Callers queue
// for a slot and give up when their context ends.
var hashSlots = make(chan struct{}, maxHashes(runtime.GOMAXPROCS(0)))

func maxHashes(procs int) int {
	return min(max(procs/argonThreads, 1), 2)
}

// argonKey derives a key; tests replace it to watch concurrency.
var argonKey = argon2.IDKey

// withHashSlot runs fn while holding a hashing slot.
func withHashSlot(ctx context.Context, fn func()) error {
	select {
	case hashSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-hashSlots }()
	fn()
	return nil
}

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
// `$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>`. It waits for a hashing
// slot, and returns the context's error if ctx ends first.
func HashPassword(ctx context.Context, pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	var key []byte
	if err := withHashSlot(ctx, func() {
		key = argonKey([]byte(pw), salt, argonTime, argonMemoryKiB, argonThreads, argonKeyLen)
	}); err != nil {
		return "", err
	}
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemoryKiB, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// errBadHash means a stored hash cannot be parsed.
var errBadHash = errors.New("unsupported password hash")

// VerifyPassword reports whether pw matches the stored hash, using the
// parameters recorded in the hash. It compares in constant time. It waits
// for a hashing slot, and returns the context's error if ctx ends first.
func VerifyPassword(ctx context.Context, hash, pw string) (bool, error) {
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
	var got []byte
	if err := withHashSlot(ctx, func() {
		got = argonKey([]byte(pw), salt, time, memory, threads, uint32(len(want))) //nolint:gosec // len(want) is at most 128.
	}); err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is verified when a username does not exist, so a failed login
// takes the same time whether or not the user exists. It is the hash of
// dummyPassword with the current parameters, fixed here so no request
// waits to create it; TestDummyHash keeps it in step with the parameters.
const (
	dummyPassword = "hangar-dummy-password-for-timing" //nolint:gosec // No account has this password; it only sets the cost of a failed login.
	dummyHash     = "$argon2id$v=19$m=65536,t=3,p=2$ySmMdmh6UU9bUe2owK5KVA$kJuAluR/5nuNJDhq3Uy7TSYWO8+eXjmq20sE7vWWb4I"
)

// timingSafeDummyVerify returns only the context's error, when ctx ends
// while it waits for a hashing slot.
func timingSafeDummyVerify(ctx context.Context, pw string) error {
	if _, err := VerifyPassword(ctx, dummyHash, pw); ctx.Err() != nil {
		return err
	}
	return nil
}
