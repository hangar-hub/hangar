package gitstore_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/gitstore"
	"github.com/hangar-hub/hangar/internal/gitstore/gitstoretest"
)

// realPack builds a pack with git of a repository with a few Commits, so
// it has commits, trees, blobs and deltas.
func realPack(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	run := func(stdin string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com")
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		require.NoError(t, err, args)
		return string(out)
	}
	run("", "init", "-q")
	text := strings.Repeat("a line of text that repeats, so git stores later versions as deltas\n", 2000)
	for i := range 5 {
		run(text+strings.Repeat("x", i), "hash-object", "-w", "--stdin")
		cmd := exec.Command("sh", "-c", "cat > f.txt")
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(text + strings.Repeat("more\n", i))
		require.NoError(t, cmd.Run())
		run("", "add", "f.txt")
		run("", "-c", "user.name=a", "-c", "user.email=a@example.com", "commit", "-q", "-m", "c")
	}
	out := run("HEAD\n", "pack-objects", "--revs", "--stdout", "-q")
	return []byte(out)
}

func readAll(t *testing.T, r *gitstore.PackReader) ([]byte, error) {
	t.Helper()
	b, err := io.ReadAll(r)
	require.NoError(t, r.Close())
	if err != nil {
		require.Equal(t, err, r.Err(), "Err reports what the reader failed with")
	} else {
		require.NoError(t, r.Err())
	}
	return b, err
}

var roomy = gitstore.PackLimits{MaxObjects: 1000, MaxObjectSize: 1 << 20, MaxDeltaSize: 1 << 20, MaxUnpacked: 16 << 20, MaxRefUpdates: 10}

func TestLimitPackPassesValidPacksThrough(t *testing.T) {
	t.Parallel()
	pack := realPack(t)
	got, err := readAll(t, gitstore.LimitPack(bytes.NewReader(pack), roomy))
	require.NoError(t, err)
	require.Equal(t, pack, got, "the bytes are passed on unchanged")

	body := gitstoretest.PktLine(strings.Repeat("0", 40)+" "+strings.Repeat("1", 40)+" refs/heads/main\x00report-status\n") + gitstoretest.Flush
	got, err = readAll(t, gitstore.LimitReceivePack(strings.NewReader(body+string(pack)), roomy))
	require.NoError(t, err)
	require.Equal(t, body+string(pack), string(got))

	// Push options are a second pkt-line section before the pack.
	opts := gitstoretest.PktLine("ci.skip") + gitstoretest.Flush
	got, err = readAll(t, gitstore.LimitReceivePack(strings.NewReader(body+opts+string(pack)), roomy))
	require.NoError(t, err)
	require.Equal(t, body+opts+string(pack), string(got))

	// A push that only deletes refs has no pack.
	got, err = readAll(t, gitstore.LimitReceivePack(strings.NewReader(body), roomy))
	require.NoError(t, err)
	require.Equal(t, body, string(got))
}

func TestLimitPackRefusesHostilePacks(t *testing.T) {
	t.Parallel()
	small := gitstoretest.Blob([]byte("base"))
	cases := map[string]struct {
		pack []byte
		want string
	}{
		"delta bomb": {gitstoretest.Pack(gitstoretest.Blob(make([]byte, 0xffff)), gitstoretest.CopyDelta(0, 0xffff, 64)),
			"a delta that rebuilds an object of 4 MiB, over the limit of 1 MiB for one object"},
		"zip bomb": {gitstoretest.Pack(gitstoretest.Blob(make([]byte, 2<<20))), "an object of 2 MiB, over the limit of 1 MiB"},
		"delta on a large base": {gitstoretest.Pack(small, gitstoretest.Object{Type: 6, Size: -1, Base: 0,
			Data: append(gitstoretest.Varint(4<<20), gitstoretest.Varint(4)...)}), "a delta whose base object is 4 MiB"},
		"too many objects": {gitstoretest.Pack(func() []gitstoretest.Object {
			objs := make([]gitstoretest.Object, 1001)
			for i := range objs {
				objs[i] = small
			}
			return objs
		}()...), "the push has 1001 objects, over the limit of 1000"},
		"too much in all": {gitstoretest.Pack(func() []gitstoretest.Object {
			objs := []gitstoretest.Object{gitstoretest.Blob(make([]byte, 0xfffff))}
			for range 20 {
				objs = append(objs, gitstoretest.CopyDelta(0, 0xfffff, 1))
			}
			return objs
		}()...), "would unpack to more than 16 MiB"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := readAll(t, gitstore.LimitPack(bytes.NewReader(tc.pack), roomy))
			var le *gitstore.PackLimitError
			require.ErrorAs(t, err, &le)
			require.Contains(t, le.Message, tc.want)
			require.Less(t, len(got), len(tc.pack)-19, "the trailer is never passed on, so no reader can finish the pack")
		})
	}
}

func TestLimitPackRefusesMalformedPacks(t *testing.T) {
	t.Parallel()
	valid := gitstoretest.Pack(gitstoretest.Blob([]byte("hello")))
	lying := gitstoretest.Pack(gitstoretest.Object{Type: 3, Size: 2, Data: []byte("more than two bytes")})
	for name, pack := range map[string][]byte{
		"not a pack":      []byte("PACK not really"),
		"truncated":       valid[:len(valid)-25],
		"bad type":        gitstoretest.Pack(gitstoretest.Object{Type: 5, Size: -1, Data: []byte("x")}),
		"size lies":       lying,
		"short delta":     gitstoretest.Pack(gitstoretest.Blob([]byte("b")), gitstoretest.Object{Type: 6, Size: -1, Data: []byte{0x80}}),
		"bad pkt-line":    []byte("zzzz"),
		"short pkt-lines": []byte("0003"),
	} {
		_, err := readAll(t, gitstore.LimitPack(bytes.NewReader(pack), roomy))
		if strings.HasPrefix(name, "bad pkt") || strings.HasPrefix(name, "short pkt") {
			_, err = readAll(t, gitstore.LimitReceivePack(bytes.NewReader(pack), roomy))
		}
		require.ErrorIs(t, err, gitstore.ErrMalformedPack, name)
	}
}

func TestLimitReceivePackRefusesTooManyRefUpdates(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	for range 11 {
		b.WriteString(gitstoretest.PktLine(strings.Repeat("0", 40) + " " + strings.Repeat("1", 40) + " refs/heads/x\n"))
	}
	b.WriteString(gitstoretest.Flush)
	_, err := readAll(t, gitstore.LimitReceivePack(strings.NewReader(b.String()), roomy))
	var le *gitstore.PackLimitError
	require.ErrorAs(t, err, &le)
	require.Contains(t, le.Message, "more than 10 refs")
}

func TestLimitPackCloseStopsTheCheck(t *testing.T) {
	t.Parallel()
	pack := realPack(t)
	r := gitstore.LimitPack(bytes.NewReader(pack), roomy)
	buf := make([]byte, 10)
	_, err := io.ReadFull(r, buf)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		require.NoError(t, r.Close())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked")
	}
	_, err = r.Read(buf)
	require.Error(t, err)
	require.False(t, errors.Is(err, io.EOF))
	require.NoError(t, r.Err(), "closing early is not a failed check")
}

func TestLimitPackDeltaLimitIsSeparate(t *testing.T) {
	t.Parallel()
	// Mirrors bound only deltas: a large plain object passes.
	l := gitstore.PackLimits{MaxDeltaSize: 1 << 20}
	pack := gitstoretest.Pack(gitstoretest.Blob(make([]byte, 2<<20)))
	got, err := readAll(t, gitstore.LimitPack(bytes.NewReader(pack), l))
	require.NoError(t, err)
	require.Equal(t, pack, got)
	_, err = readAll(t, gitstore.LimitPack(bytes.NewReader(gitstoretest.Pack(gitstoretest.Blob(make([]byte, 0xffff)), gitstoretest.CopyDelta(0, 0xffff, 32))), l))
	var le *gitstore.PackLimitError
	require.ErrorAs(t, err, &le)
	require.Contains(t, le.Message, "a delta that rebuilds an object of 2 MiB, over the limit of 1 MiB")
}

func TestLimitPackPassesOnSourceErrors(t *testing.T) {
	t.Parallel()
	pack := realPack(t)
	failure := errors.New("the source failed")
	r := gitstore.LimitPack(io.MultiReader(bytes.NewReader(pack[:100]), iotestErr{failure}), roomy)
	_, err := io.ReadAll(r)
	require.ErrorIs(t, err, failure, "not reported as a malformed pack")
	require.NoError(t, r.Close())
	require.ErrorIs(t, r.Err(), failure)
}

type iotestErr struct{ err error }

func (e iotestErr) Read([]byte) (int, error) { return 0, e.err }

func TestPackLimitsFor(t *testing.T) {
	t.Parallel()
	l := gitstore.PackLimitsFor(10<<20, 2<<30)
	require.Equal(t, int64(gitstore.MinPackObjectSize), l.MaxObjectSize)
	require.Equal(t, int64(gitstore.MinPackObjectSize), l.MaxDeltaSize)
	require.Equal(t, int64(8<<30), l.MaxUnpacked)
	require.Equal(t, int64(gitstore.DefaultMaxPackObjects), l.MaxObjects)
	require.Equal(t, gitstore.DefaultMaxRefUpdates, l.MaxRefUpdates)
	require.Equal(t, int64(1<<30), gitstore.PackLimitsFor(1<<30, 0).MaxObjectSize)
	require.Zero(t, gitstore.PackLimitsFor(1, 0).MaxUnpacked)
	require.Equal(t, int64(gitstore.MinUnpacked), gitstore.PackLimitsFor(1, 64<<10).MaxUnpacked)
}

func TestBatchReadsObjectsOneAtATime(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	git := func(stdin string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.Output()
		require.NoError(t, err)
		return strings.TrimSpace(string(out))
	}
	git("", "init", "-q", "--bare")
	small := git("hello\n", "hash-object", "-w", "--stdin")
	large := git(strings.Repeat("x", 100), "hash-object", "-w", "--stdin")
	b, err := gitstore.Git{Home: t.TempDir()}.OpenBatch(context.Background(), dir)
	require.NoError(t, err)
	info, content, err := b.Read(small, 10)
	require.NoError(t, err)
	require.Equal(t, "blob", info.Type)
	require.Equal(t, "hello\n", string(content))
	info, _, err = b.Read(strings.Repeat("0", 40), 10)
	require.NoError(t, err)
	require.True(t, info.Missing)
	_, content, err = b.Read(small, 10)
	require.NoError(t, err)
	require.Equal(t, "hello\n", string(content), "the stream stays in step")
	_, _, err = b.Read(large, 10)
	require.ErrorIs(t, err, gitstore.ErrObjectTooLarge)
	_ = b.Close()
}
