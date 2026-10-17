package gitstoretest_test

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // git object IDs and pack trailers are SHA-1.
	"fmt"
	"io"
	"math/rand/v2"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/format/packfile"
	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/gitstore/gitstoretest"
)

// scanned is one object as go-git's pack scanner reads it back.
type scanned struct {
	header  packfile.ObjectHeader
	content []byte
}

// scan parses pack with go-git's scanner, which checks the header, every
// object entry and the SHA-1 trailer, and returns the objects it read.
//
// The reader hides bytes.Reader's Seek: on a seekable reader the scanner
// inflates content by seeking back, which mid-scan breaks its running
// checksum. A plain reader makes it keep each object's content instead.
func scan(t *testing.T, pack []byte) []scanned {
	t.Helper()
	s := packfile.NewScanner(io.MultiReader(bytes.NewReader(pack)))
	var objs []scanned
	var sawHeader, sawFooter bool
	for s.Scan() {
		d := s.Data()
		switch d.Section {
		case packfile.HeaderSection:
			h, ok := d.Value().(packfile.Header)
			require.True(t, ok)
			require.Equal(t, packfile.V2, h.Version)
			sawHeader = true
		case packfile.ObjectSection:
			oh, ok := d.Value().(packfile.ObjectHeader)
			require.True(t, ok)
			var b bytes.Buffer
			require.NoError(t, s.WriteObject(&oh, &b))
			objs = append(objs, scanned{header: oh, content: b.Bytes()})
		case packfile.FooterSection:
			sum := sha1.Sum(pack[:len(pack)-20]) //nolint:gosec // the pack trailer is SHA-1.
			require.Equal(t, sum[:], pack[len(pack)-20:])
			sawFooter = true
		}
	}
	require.NoError(t, s.Error())
	// The scanner reports no header section for an empty pack.
	require.Equal(t, len(objs) > 0, sawHeader, "pack header section")
	require.True(t, sawFooter, "pack trailer not scanned")
	return objs
}

// blobID is the git object ID of a blob of content.
func blobID(content []byte) [20]byte {
	return sha1.Sum(append(fmt.Appendf(nil, "blob %d\x00", len(content)), content...)) //nolint:gosec // git object IDs are SHA-1.
}

func TestPackEmpty(t *testing.T) {
	require.Empty(t, scan(t, gitstoretest.Pack()))
}

func TestPackBlobsRoundTrip(t *testing.T) {
	// Sizes that need one, two and three bytes of object header.
	small := []byte("hello")
	medium := bytes.Repeat([]byte("m"), 100)
	large := bytes.Repeat([]byte("L"), 5000)

	objs := scan(t, gitstoretest.Pack(gitstoretest.Blob(small), gitstoretest.Blob(medium), gitstoretest.Blob(large)))
	require.Len(t, objs, 3)
	for i, want := range [][]byte{small, medium, large} {
		require.Equal(t, plumbing.BlobObject, objs[i].header.Type)
		require.Equal(t, int64(len(want)), objs[i].header.Size)
		require.Equal(t, want, objs[i].content)
		id := blobID(want)
		require.Equal(t, id[:], objs[i].header.Hash.Bytes())
	}
}

func TestPackDeclaredSizeOverridesData(t *testing.T) {
	// A header that claims far more than the data holds, as a hostile
	// pack would, written with a multi-byte size.
	const claimed = int64(1) << 35
	objs := scan(t, gitstoretest.Pack(gitstoretest.Object{Type: 3, Size: claimed, Data: []byte("x")}))
	require.Len(t, objs, 1)
	require.Equal(t, claimed, objs[0].header.Size)
}

func TestPackCopyDeltaExpands(t *testing.T) {
	base := []byte("abcdefgh")
	const copies = 5
	pack := gitstoretest.Pack(gitstoretest.Blob(base), gitstoretest.CopyDelta(0, int64(len(base)), copies))

	objs := scan(t, pack)
	require.Len(t, objs, 2)
	d := objs[1]
	require.Equal(t, plumbing.OFSDeltaObject, d.header.Type)
	require.Equal(t, objs[0].header.Offset, d.header.OffsetReference)
	// Two size varints, then four bytes per copy.
	require.Equal(t, int64(2+4*copies), d.header.Size)

	got, err := packfile.PatchDelta(base, d.content)
	require.NoError(t, err)
	require.Equal(t, bytes.Repeat(base, copies), got)
}

func TestPackOFSDeltaFarBase(t *testing.T) {
	// Incompressible filler puts the delta far enough from its base that
	// the offset needs two and then three bytes of its encoding.
	rng := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // deterministic filler, not a secret.
	filler := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rng.Uint32())
		}
		return b
	}
	base := []byte("base!")
	for _, fill := range []int{200, 20000} {
		t.Run(fmt.Sprint(fill), func(t *testing.T) {
			pack := gitstoretest.Pack(
				gitstoretest.Blob(base),
				gitstoretest.Blob(filler(fill)),
				gitstoretest.CopyDelta(0, int64(len(base)), 2),
			)
			objs := scan(t, pack)
			require.Len(t, objs, 3)
			require.Equal(t, objs[0].header.Offset, objs[2].header.OffsetReference)
			require.Greater(t, objs[2].header.Offset-objs[0].header.Offset, int64(fill))
			got, err := packfile.PatchDelta(base, objs[2].content)
			require.NoError(t, err)
			require.Equal(t, bytes.Repeat(base, 2), got)
		})
	}
}

func TestPackREFDelta(t *testing.T) {
	base := []byte("ref base")
	oid := blobID(base)
	delta := gitstoretest.CopyDelta(0, int64(len(base)), 3)
	delta.Type = 7
	delta.BaseOID = oid

	objs := scan(t, gitstoretest.Pack(gitstoretest.Blob(base), delta))
	require.Len(t, objs, 2)
	require.Equal(t, plumbing.REFDeltaObject, objs[1].header.Type)
	require.Equal(t, oid[:], objs[1].header.Reference.Bytes())
	got, err := packfile.PatchDelta(base, objs[1].content)
	require.NoError(t, err)
	require.Equal(t, bytes.Repeat(base, 3), got)
}

func TestCopyDeltaBaseSizeRange(t *testing.T) {
	for _, size := range []int64{0, -1, 0x1000000} {
		require.PanicsWithValue(t,
			fmt.Sprintf("gitstoretest: base size %d out of range", size),
			func() { gitstoretest.CopyDelta(0, size, 1) },
			"base size %d", size)
	}
	// The largest base size a three-byte copy can address is accepted.
	d := gitstoretest.CopyDelta(0, 0xffffff, 1)
	require.Equal(t, append(gitstoretest.Varint(0xffffff), gitstoretest.Varint(0xffffff)...), d.Data[:8])
	require.Equal(t, []byte{0xf0, 0xff, 0xff, 0xff}, d.Data[8:])
}

func TestVarint(t *testing.T) {
	for _, tc := range []struct {
		n    uint64
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{0x7f, []byte{0x7f}},
		{0x80, []byte{0x80, 0x01}},
		{300, []byte{0xac, 0x02}},
		{1 << 21, []byte{0x80, 0x80, 0x80, 0x01}},
	} {
		require.Equal(t, tc.want, gitstoretest.Varint(tc.n), "Varint(%d)", tc.n)
	}
}

func TestPktLine(t *testing.T) {
	require.Equal(t, "0009hello", gitstoretest.PktLine("hello"))
	require.Equal(t, "0004", gitstoretest.PktLine(""))
	require.Equal(t, "0000", gitstoretest.Flush)
}
