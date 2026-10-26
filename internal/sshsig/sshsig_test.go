package sshsig_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/hangar-hub/hangar/internal/sshsig"
)

// fixtureKey and fixtureSig were made with OpenSSH 10:
//
//	ssh-keygen -t ed25519 -f id_ed25519
//	printf '%s' 'hangar-ollama-key fixture challenge' | ssh-keygen -Y sign -f id_ed25519 -n hangar-ollama-key
const (
	fixtureKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEfpXQuXtZJWUP5WnUVXmDXxDvbKBUSh3buBV35BuVKw fixture@hangar"
	fixtureMsg = "hangar-ollama-key fixture challenge"
	fixtureSig = `-----BEGIN SSH SIGNATURE-----
U1NIU0lHAAAAAQAAADMAAAALc3NoLWVkMjU1MTkAAAAgR+ldC5e1klZQ/ladRVeYNfEO9s
oFRKHdu4FXfkG5UrAAAAARaGFuZ2FyLW9sbGFtYS1rZXkAAAAAAAAABnNoYTUxMgAAAFMA
AAALc3NoLWVkMjU1MTkAAABAjQDUAoB6s9pas6wxvkjv9HfoaAurAqmNR/HxiiIhp4q/4G
U66JZaksOMEPnxkUQdi/RRi8KNrJjIzI+7JWcOCw==
-----END SSH SIGNATURE-----
`
)

func parseKey(t *testing.T, s string) ssh.PublicKey {
	t.Helper()
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s))
	require.NoError(t, err)
	return pub
}

// TestVerifiesSSHKeygenSignatures checks a signature from ssh-keygen -Y
// sign, the command the web UI tells users to run.
func TestVerifiesSSHKeygenSignatures(t *testing.T) {
	pub := parseKey(t, fixtureKey)
	require.NoError(t, sshsig.Verify(fixtureSig, pub, "hangar-ollama-key", []byte(fixtureMsg)))
	require.NoError(t, sshsig.Verify(strings.ReplaceAll(fixtureSig, "\n", "\r\n"), pub, "hangar-ollama-key", []byte(fixtureMsg)),
		"line endings from a Windows clipboard")

	require.ErrorIs(t, sshsig.Verify(fixtureSig, pub, "hangar-ollama-key", []byte(fixtureMsg+"x")), sshsig.ErrInvalid, "another message")
	require.ErrorIs(t, sshsig.Verify(fixtureSig, pub, "file", []byte(fixtureMsg)), sshsig.ErrInvalid, "another namespace")
	other, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	otherPub, err := ssh.NewPublicKey(other)
	require.NoError(t, err)
	require.ErrorIs(t, sshsig.Verify(fixtureSig, otherPub, "hangar-ollama-key", []byte(fixtureMsg)), sshsig.ErrInvalid, "another key")

	for name, bad := range map[string]string{
		"empty":       "",
		"no armour":   "U1NIU0lH",
		"not base64":  "-----BEGIN SSH SIGNATURE-----\n!!!\n-----END SSH SIGNATURE-----",
		"short":       "-----BEGIN SSH SIGNATURE-----\nU1NIU0lHAAAAAQ==\n-----END SSH SIGNATURE-----",
		"wrong magic": "-----BEGIN SSH SIGNATURE-----\nQUFBQUFBAAAAAQ==\n-----END SSH SIGNATURE-----",
		"too long":    strings.Repeat("A", 9000),
	} {
		require.ErrorIs(t, sshsig.Verify(bad, pub, "hangar-ollama-key", []byte(fixtureMsg)), sshsig.ErrInvalid, name)
	}
}

// TestSignRoundTrip checks that Sign writes what Verify, and so
// ssh-keygen -Y verify, accepts.
func TestSignRoundTrip(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	sig, err := sshsig.Sign(signer, "hangar-ollama-key", []byte("a challenge"))
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(sig, "-----BEGIN SSH SIGNATURE-----\n"))
	for _, line := range strings.Split(strings.TrimSpace(sig), "\n") {
		require.LessOrEqual(t, len(line), 70, "armour lines wrap at 70 characters, as ssh-keygen writes them")
	}
	require.NoError(t, sshsig.Verify(sig, signer.PublicKey(), "hangar-ollama-key", []byte("a challenge")))
	require.ErrorIs(t, sshsig.Verify(sig, signer.PublicKey(), "hangar-ollama-key", []byte("another")), sshsig.ErrInvalid)
}
