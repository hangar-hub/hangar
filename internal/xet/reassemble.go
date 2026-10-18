package xet

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"

	"github.com/hangar-hub/hangar/internal/blob"
)

// fileReader streams a file's bytes out of stored xorbs, term by term,
// following hub-docs file-reconstruction.md: the decoded chunk ranges are
// concatenated in term order with no gaps. It checks every chunk against
// the stored index (defence in depth against storage corruption) and hashes
// the output with SHA-256, so the caller can check the claimed digest.
type fileReader struct {
	ctx   context.Context
	blobs blob.Store
	file  *ShardFile
	index IndexFunc

	term  int        // next term to open
	cur   Term       // the open term
	x     *XorbIndex // its xorb
	chunk uint32     // next chunk of the open term
	body  io.ReadCloser
	br    *bufio.Reader

	hdr     [chunkHeaderSize]byte
	payload []byte
	raw     []byte
	out     []byte // the decoded chunk being returned
	pos     int
	dec     decoder
	hasher  chunkHasher
	sha     hash.Hash
	n       int64
	err     error
}

func (s *Service) newFileReader(ctx context.Context, f *ShardFile, index IndexFunc) *fileReader {
	return &fileReader{
		ctx: ctx, blobs: s.blobs, file: f, index: index,
		payload: make([]byte, MaxCompressedChunkSize), raw: make([]byte, MaxChunkSize),
		hasher: newChunkHasher(), sha: sha256.New(),
	}
}

// Read implements io.Reader.
func (fr *fileReader) Read(p []byte) (int, error) {
	for fr.pos >= len(fr.out) {
		if fr.err != nil {
			return 0, fr.err
		}
		if err := fr.next(); err != nil {
			fr.err = err
			fr.closeBody()
			return 0, err
		}
	}
	n := copy(p, fr.out[fr.pos:])
	fr.pos += n
	return n, nil
}

// Close releases the open xorb read, if any.
func (fr *fileReader) Close() { fr.closeBody() }

func (fr *fileReader) closeBody() {
	if fr.body != nil {
		_ = fr.body.Close()
		fr.body, fr.br = nil, nil
	}
}

// openTerm starts a ranged read of the next term's chunks.
func (fr *fileReader) openTerm() error {
	fr.closeBody()
	if fr.term >= len(fr.file.Terms) {
		return io.EOF
	}
	t := fr.file.Terms[fr.term]
	fr.term++
	x, err := fr.index(t.Xorb)
	if err != nil {
		return err
	}
	if x == nil || t.Start >= t.End || int(t.End) > len(x.Chunks) {
		return fmt.Errorf("%w: file %s refers to chunks xorb %s does not have", ErrBadShard, fr.file.Hash, t.Xorb)
	}
	first, last := x.Chunks[t.Start], x.Chunks[t.End-1]
	base := x.dataStart()
	rng := &blob.Range{Start: base + int64(first.Offset), End: base + int64(last.End()) - 1}
	body, _, err := fr.blobs.Get(fr.ctx, XorbKey(t.Xorb), rng)
	if errors.Is(err, blob.ErrNotFound) {
		return unknownXorb(t.Xorb)
	}
	if err != nil {
		return fmt.Errorf("xet: read xorb %s: %w", t.Xorb, err)
	}
	fr.body, fr.br = body, bufio.NewReaderSize(body, 256<<10)
	fr.cur, fr.x, fr.chunk = t, x, t.Start
	return nil
}

// next decodes the next chunk into fr.out.
func (fr *fileReader) next() error {
	for fr.body == nil || fr.chunk >= fr.cur.End {
		if err := fr.openTerm(); err != nil {
			return err
		}
	}
	c := fr.x.Chunks[fr.chunk]
	corrupt := func(why string) error {
		return fmt.Errorf("xet: the stored xorb %s is corrupt at chunk %d: %s", fr.cur.Xorb, fr.chunk, why)
	}
	if _, err := io.ReadFull(fr.br, fr.hdr[:]); err != nil {
		return corrupt(err.Error())
	}
	h, footer, err := parseChunkHeader(fr.hdr[:])
	if err != nil || footer || h.CompressedSize != c.CompressedSize || h.Size != c.Size {
		return corrupt("the chunk header does not match the index")
	}
	p := fr.payload[:h.CompressedSize]
	if _, err := io.ReadFull(fr.br, p); err != nil {
		return corrupt(err.Error())
	}
	out, err := fr.dec.decompress(h.Scheme, p, h.Size, fr.raw)
	if err != nil {
		return corrupt(err.Error())
	}
	if fr.hasher.sum(out) != c.Hash {
		return corrupt("the chunk hash does not match the index")
	}
	_, _ = fr.sha.Write(out)
	fr.n += int64(len(out))
	fr.out, fr.pos = out, 0
	fr.chunk++
	return nil
}

// verify reads whatever is left and checks the whole file's size and
// SHA-256. The SHA-256 in a shard comes from the client and is never
// trusted without this check (03-hf-api.md "Shard handling").
func (fr *fileReader) verify(oid string, size int64) error {
	if fr.err == nil {
		if _, err := io.Copy(io.Discard, fr); err != nil {
			return err
		}
	}
	if fr.err != nil && !errors.Is(fr.err, io.EOF) {
		return fr.err
	}
	if fr.n != size {
		return fmt.Errorf("%w: file %s rebuilds to %d bytes, not %d", ErrBadShard, fr.file.Hash, fr.n, size)
	}
	if got := hex.EncodeToString(fr.sha.Sum(nil)); got != oid {
		return fmt.Errorf("%w: file %s rebuilds to SHA-256 %s, not %s", ErrBadShard, fr.file.Hash, got, oid)
	}
	return nil
}
