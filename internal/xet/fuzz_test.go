package xet_test

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/jamesagarside/hangar/internal/xet"
	"github.com/jamesagarside/hangar/internal/xet/xettest"
)

// FuzzParseXorb checks the xorb parser never panics, never accepts more
// than the limits allow, and that an accepted xorb's index describes
// exactly the chunk data it copied out.
func FuzzParseXorb(f *testing.F) {
	f.Add(xettest.BuildXorb([][]byte{[]byte("hello"), textBytes(300, 1)}, false, xet.CompressionNone, xet.CompressionLZ4).Body)
	f.Add(xettest.BuildXorb([][]byte{textBytes(1001, 2), textBytes(7, 3)}, true, xet.CompressionBG4LZ4).Body)
	f.Add([]byte("XETBLOB"))
	f.Add([]byte{0, 1, 0, 0, 1, 1, 0, 0, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		var data bytes.Buffer
		idx, err := xet.ParseXorb(bytes.NewReader(b), &data)
		if err != nil {
			if !errors.Is(err, xet.ErrBadXorb) {
				t.Fatalf("unexpected error class: %v", err)
			}
			return
		}
		if len(idx.Chunks) == 0 || len(idx.Chunks) > xet.MaxXorbChunks || idx.RawSize() > xet.MaxXorbBytes {
			t.Fatalf("accepted a xorb outside the limits: %d chunks", len(idx.Chunks))
		}
		if int64(data.Len()) != idx.DataSize || !bytes.HasPrefix(b, data.Bytes()) {
			t.Fatal("the copied chunk data is not the start of the input")
		}
		back, err := xet.UnmarshalIndex(idx.MarshalIndex())
		if err != nil {
			t.Fatalf("index does not round-trip: %v", err)
		}
		if back.Hash() != idx.Hash() || back.DataSize != idx.DataSize {
			t.Fatal("index round trip changed it")
		}
		// Parsing the copied data alone gives the same index.
		again, err := xet.ParseXorb(bytes.NewReader(data.Bytes()), nil)
		if err != nil || again.Hash() != idx.Hash() {
			t.Fatalf("reparse differs: %v", err)
		}
	})
}

// FuzzParseShard checks the shard parser never panics or over-allocates,
// and that a parsed shard survives serialising and parsing again. Records
// have unused bytes the parser ignores (xet-core's own test builds fill
// them), so the bytes themselves may differ.
func FuzzParseShard(f *testing.F) {
	u := xettest.BuildUpload([][]byte{textBytes(5000, 1), {}}, xettest.UploadOptions{ChunkSize: 1000, ChunksPerXorb: 3})
	f.Add(u.ShardBytes())
	for _, name := range []string{".shard", ".shard.verification-no-footer"} {
		f.Add(readRef(f, refFile+name))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		sh, err := xet.ParseShard(b)
		if err != nil {
			if !errors.Is(err, xet.ErrBadShard) && !errors.Is(err, xet.ErrTooLarge) {
				t.Fatalf("unexpected error class: %v", err)
			}
			return
		}
		_ = sh.CheckUpload()
		_ = sh.XorbHashes()
		again, err := xet.ParseShard(xet.MarshalShard(sh))
		if err != nil {
			t.Fatalf("a serialised shard does not parse: %v", err)
		}
		again.FooterSize = sh.FooterSize
		if !reflect.DeepEqual(normalise(sh), normalise(again)) {
			t.Fatal("a shard does not survive a round trip")
		}
	})
}

// normalise makes empty and nil slices compare equal.
func normalise(s *xet.Shard) *xet.Shard {
	c := *s
	c.Files = append([]xet.ShardFile{}, s.Files...)
	for i := range c.Files {
		c.Files[i].Terms = append([]xet.Term{}, c.Files[i].Terms...)
		c.Files[i].Verification = append([]xet.Hash{}, c.Files[i].Verification...)
	}
	c.Xorbs = append([]xet.ShardXorb{}, s.Xorbs...)
	for i := range c.Xorbs {
		c.Xorbs[i].Chunks = append([]xet.ShardChunk{}, c.Xorbs[i].Chunks...)
	}
	return &c
}

// FuzzUnmarshalIndex checks the stored index parser rejects inconsistent
// input instead of panicking.
func FuzzUnmarshalIndex(f *testing.F) {
	x := xettest.BuildXorb([][]byte{[]byte("abc"), []byte("defgh")}, false)
	idx, err := xet.ParseXorb(bytes.NewReader(x.Body), nil)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(idx.MarshalIndex())
	f.Fuzz(func(t *testing.T, b []byte) {
		idx, err := xet.UnmarshalIndex(b)
		if err != nil {
			return
		}
		var end int64
		for _, c := range idx.Chunks {
			if int64(c.Offset) != end || c.CompressedSize == 0 {
				t.Fatal("accepted chunks with gaps or overlaps")
			}
			end = int64(c.End())
		}
		if end != idx.DataSize {
			t.Fatal("chunks do not cover the data")
		}
	})
}

// FuzzParseHash checks hash strings round-trip.
func FuzzParseHash(f *testing.F) {
	f.Add(refXorbHash)
	f.Add("zz")
	f.Fuzz(func(t *testing.T, s string) {
		h, err := xet.ParseHash(s)
		if err != nil {
			return
		}
		if h.String() != s {
			t.Fatalf("%q round-trips to %q", s, h.String())
		}
	})
}
