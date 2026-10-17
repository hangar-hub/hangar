package auth

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/argon2"
)

// countArgon replaces argonKey with a slow fake that records the most
// computations running at once, and restores it when the test ends.
func countArgon(t *testing.T, hold time.Duration) *atomic.Int32 {
	t.Helper()
	var running, most atomic.Int32
	argonKey = func(pw, salt []byte, _, _ uint32, _ uint8, keyLen uint32) []byte {
		n := running.Add(1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(hold)
		running.Add(-1)
		return argon2.IDKey(pw, salt, 1, 8, 1, keyLen)
	}
	t.Cleanup(func() { argonKey = argon2.IDKey })
	return &most
}

// A burst of sign-in attempts must not run more argon2id computations at
// once than hashSlots allows: each holds 64 MiB, and 40 at once took a
// 1 GiB replica down in the load tests.
func TestPasswordHashingIsBounded(t *testing.T) {
	hash, err := HashPassword(t.Context(), "correct horse battery staple 42")
	require.NoError(t, err)
	most := countArgon(t, 20*time.Millisecond)

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			_, err := VerifyPassword(t.Context(), hash, "wrong password 1234")
			require.NoError(t, err)
		})
		wg.Go(func() {
			_, err := HashPassword(t.Context(), "another good password 99")
			require.NoError(t, err)
		})
	}
	wg.Wait()
	require.LessOrEqual(t, int(most.Load()), cap(hashSlots))
	require.Positive(t, int(most.Load()))
}

func TestMaxHashes(t *testing.T) {
	for procs, want := range map[int]int{1: 1, 2: 1, 3: 1, 4: 2, 12: 2, 64: 2} {
		require.Equal(t, want, maxHashes(procs), "GOMAXPROCS %d", procs)
	}
}

// A caller whose request ends while it waits for a slot gets the
// context's error and never hashes.
func TestPasswordHashingWaitEndsWithContext(t *testing.T) {
	hash, err := HashPassword(t.Context(), "correct horse battery staple 42")
	require.NoError(t, err)
	countArgon(t, 0)
	for range cap(hashSlots) {
		hashSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for range cap(hashSlots) {
			<-hashSlots
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = VerifyPassword(ctx, hash, "correct horse battery staple 42")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, err = HashPassword(ctx, "another good password 99")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, timingSafeDummyVerify(ctx, "anything at all"), context.DeadlineExceeded)
}

// The fixed dummy hash must cost what a real one costs, so it must carry
// the current parameters and verify its password.
func TestDummyHash(t *testing.T) {
	fresh, err := HashPassword(t.Context(), dummyPassword)
	require.NoError(t, err)
	prefix := func(h string) string { return h[:strings.LastIndex(h[:strings.LastIndex(h, "$")], "$")] }
	require.Equal(t, prefix(fresh), prefix(dummyHash), "regenerate dummyHash with HashPassword(dummyPassword)")
	ok, err := VerifyPassword(t.Context(), dummyHash, dummyPassword)
	require.NoError(t, err)
	require.True(t, ok)
}
