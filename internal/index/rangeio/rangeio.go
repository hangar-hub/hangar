// Package rangeio turns ranged reads of a stored file into an io.ReaderAt
// that fetches fixed-size windows on demand and keeps the most recent ones,
// so header parsers read small parts of large files without downloading
// them (docs/spec/07-indexing-and-scanning.md "Reading without
// downloading").
package rangeio

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// DefaultWindow is the size of one ranged read: 1 MiB.
const DefaultWindow = 1 << 20

// maxWindows is how many windows the Reader keeps.
const maxWindows = 4

// OpenFunc opens the bytes [start, end] (inclusive) of a file.
type OpenFunc func(ctx context.Context, start, end int64) (io.ReadCloser, error)

// Reader is an io.ReaderAt over a file of known size. It is not safe for
// concurrent use.
type Reader struct {
	ctx    context.Context
	open   OpenFunc
	size   int64
	window int64
	cache  []window
	// Fetched counts the bytes read from the backend, for tests and metrics.
	Fetched int64
}

type window struct {
	start int64
	data  []byte
}

// New returns a Reader over a file of size bytes. window is the size of
// each ranged read; zero means DefaultWindow.
func New(ctx context.Context, size int64, window int64, open OpenFunc) *Reader {
	if window <= 0 {
		window = DefaultWindow
	}
	return &Reader{ctx: ctx, open: open, size: size, window: window}
}

// FromBytes returns a Reader over data in memory.
func FromBytes(data []byte) *Reader {
	return New(context.Background(), int64(len(data)), DefaultWindow, func(_ context.Context, start, end int64) (io.ReadCloser, error) {
		return io.NopCloser(&sliceReader{b: data[start : end+1]}), nil
	})
}

type sliceReader struct{ b []byte }

func (s *sliceReader) Read(p []byte) (int, error) {
	if len(s.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, s.b)
	s.b = s.b[n:]
	return n, nil
}

// Size returns the file size.
func (r *Reader) Size() int64 { return r.size }

// ReadAt implements io.ReaderAt.
func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("rangeio: negative offset")
	}
	if off >= r.size {
		if len(p) == 0 {
			return 0, nil
		}
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && off < r.size {
		w, err := r.windowAt(off)
		if err != nil {
			return n, err
		}
		c := copy(p[n:], w.data[off-w.start:])
		n += c
		off += int64(c)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (r *Reader) windowAt(off int64) (window, error) {
	for i, w := range r.cache {
		if off >= w.start && off < w.start+int64(len(w.data)) {
			if i > 0 {
				r.cache = append([]window{w}, append(r.cache[:i:i], r.cache[i+1:]...)...)
			}
			return w, nil
		}
	}
	if err := r.ctx.Err(); err != nil {
		return window{}, err
	}
	start := off - off%r.window
	end := min(start+r.window, r.size) - 1
	rc, err := r.open(r.ctx, start, end)
	if err != nil {
		return window{}, err
	}
	data := make([]byte, end-start+1)
	_, err = io.ReadFull(rc, data)
	_ = rc.Close()
	if err != nil {
		return window{}, fmt.Errorf("rangeio: read bytes %d-%d: %w", start, end, err)
	}
	r.Fetched += int64(len(data))
	w := window{start: start, data: data}
	r.cache = append([]window{w}, r.cache...)
	if len(r.cache) > maxWindows {
		r.cache = r.cache[:maxWindows]
	}
	return w, nil
}

// Cursor reads sequentially from a ReaderAt and tracks its position.
type Cursor struct {
	R   io.ReaderAt
	Pos int64
}

// Read implements io.Reader.
func (c *Cursor) Read(p []byte) (int, error) {
	n, err := c.R.ReadAt(p, c.Pos)
	c.Pos += int64(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

// Skip advances the position by n bytes without reading.
func (c *Cursor) Skip(n int64) { c.Pos += n }
