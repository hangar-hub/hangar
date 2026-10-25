// Package sshsig signs and verifies SSH signatures in the format of
// `ssh-keygen -Y sign` (OpenSSH PROTOCOL.sshsig): an armoured blob holding
// the public key, a namespace and a signature over a hash of the message.
// Hangar uses it for proof of possession of an Ollama key: the user signs
// a server-issued challenge with ~/.ollama/id_ed25519, either with
// ssh-keygen or with `hangar ollama key register`.
package sshsig

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

const (
	magic      = "SSHSIG"
	version    = 1
	armourHead = "-----BEGIN SSH SIGNATURE-----"
	armourTail = "-----END SSH SIGNATURE-----"
	// maxArmoured bounds the signature text a caller may send.
	maxArmoured = 8 << 10
)

// ErrInvalid means the signature is malformed, for another key or
// namespace, or does not match the message.
var ErrInvalid = errors.New("invalid SSH signature")

// Sign returns the armoured SSH signature of message by signer in
// namespace, as `ssh-keygen -Y sign -n <namespace>` writes it, hashing
// the message with SHA-512.
func Sign(signer ssh.Signer, namespace string, message []byte) (string, error) {
	sum := sha512.Sum512(message)
	sig, err := signer.Sign(rand.Reader, signedData(namespace, "sha512", sum[:]))
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	b.WriteString(magic)
	_ = binary.Write(&b, binary.BigEndian, uint32(version))
	writeString(&b, signer.PublicKey().Marshal())
	writeString(&b, []byte(namespace))
	writeString(&b, nil)
	writeString(&b, []byte("sha512"))
	writeString(&b, ssh.Marshal(sig))
	enc := base64.StdEncoding.EncodeToString(b.Bytes())
	var out strings.Builder
	out.WriteString(armourHead + "\n")
	for len(enc) > 70 {
		out.WriteString(enc[:70] + "\n")
		enc = enc[70:]
	}
	out.WriteString(enc + "\n" + armourTail + "\n")
	return out.String(), nil
}

// Verify checks that armoured is a signature of message by pub in
// namespace. It accepts the SHA-256 and SHA-512 hashes ssh-keygen uses.
func Verify(armoured string, pub ssh.PublicKey, namespace string, message []byte) error {
	if len(armoured) > maxArmoured {
		return fmt.Errorf("%w: too long", ErrInvalid)
	}
	body := strings.TrimSpace(strings.ReplaceAll(armoured, "\r\n", "\n"))
	body, ok := strings.CutPrefix(body, armourHead)
	if !ok {
		return fmt.Errorf("%w: it must start with %s", ErrInvalid, armourHead)
	}
	body, ok = strings.CutSuffix(body, armourTail)
	if !ok {
		return fmt.Errorf("%w: it must end with %s", ErrInvalid, armourTail)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(body), ""))
	if err != nil {
		return fmt.Errorf("%w: not base64", ErrInvalid)
	}
	r := reader{b: raw}
	if string(r.next(len(magic))) != magic || r.uint32() != version {
		return fmt.Errorf("%w: not an SSHSIG version 1 signature", ErrInvalid)
	}
	keyBlob, ns, _, hashAlg, sigBlob := r.string(), r.string(), r.string(), r.string(), r.string()
	if r.err || len(r.b) != 0 {
		return fmt.Errorf("%w: malformed", ErrInvalid)
	}
	if !bytes.Equal(keyBlob, pub.Marshal()) {
		return fmt.Errorf("%w: signed with another key", ErrInvalid)
	}
	if string(ns) != namespace {
		return fmt.Errorf("%w: signed for namespace %q, not %q", ErrInvalid, ns, namespace)
	}
	var sum []byte
	switch string(hashAlg) {
	case "sha512":
		s := sha512.Sum512(message)
		sum = s[:]
	case "sha256":
		s := sha256.Sum256(message)
		sum = s[:]
	default:
		return fmt.Errorf("%w: unsupported hash %q", ErrInvalid, hashAlg)
	}
	sr := reader{b: sigBlob}
	format, blob := sr.string(), sr.string()
	if sr.err {
		return fmt.Errorf("%w: malformed", ErrInvalid)
	}
	sig := &ssh.Signature{Format: string(format), Blob: blob}
	if len(sr.b) > 0 {
		sig.Rest = sr.b
	}
	if err := pub.Verify(signedData(namespace, string(hashAlg), sum), sig); err != nil {
		return fmt.Errorf("%w: it does not match", ErrInvalid)
	}
	return nil
}

// signedData is what the key signs (PROTOCOL.sshsig): the magic preamble,
// namespace, reserved field, hash algorithm and the hash of the message.
func signedData(namespace, hashAlg string, sum []byte) []byte {
	var b bytes.Buffer
	b.WriteString(magic)
	writeString(&b, []byte(namespace))
	writeString(&b, nil)
	writeString(&b, []byte(hashAlg))
	writeString(&b, sum)
	return b.Bytes()
}

func writeString(b *bytes.Buffer, s []byte) {
	_ = binary.Write(b, binary.BigEndian, uint32(len(s))) //nolint:gosec // Keys, namespaces, hashes and signatures are far below 4 GiB.
	b.Write(s)
}

// reader reads SSH wire-format fields; err is set on a short read.
type reader struct {
	b   []byte
	err bool
}

func (r *reader) next(n int) []byte {
	if r.err || n < 0 || n > len(r.b) {
		r.err = true
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *reader) uint32() uint32 {
	b := r.next(4)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

func (r *reader) string() []byte {
	n := r.uint32()
	if r.err || uint64(n) > uint64(len(r.b)) {
		r.err = true
		return nil
	}
	return r.next(int(n))
}
