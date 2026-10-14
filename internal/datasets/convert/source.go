// Package convert reads dataset source files the way the `datasets`
// packaged builders do (Parquet, CSV and TSV, JSON and JSON Lines, text,
// Arrow IPC, WebDataset tar shards, ImageFolder and AudioFolder), infers
// or applies Features, and writes the Parquet export in the Hub's layout
// (DATA-3).
package convert

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// ErrUnsupported means a source format or option Hangar cannot convert.
var ErrUnsupported = errors.New("unsupported source")

// File is a source data file in a Repo.
type File struct {
	// Path is relative to the Repo root.
	Path string
	Size int64
	// Open returns the file's content.
	Open func(ctx context.Context) (io.ReadCloser, error)
	// OpenAt returns random access to the content, for Parquet footers and
	// zip archives.
	OpenAt func(ctx context.Context) (io.ReaderAt, error)
	// OID is the file's LFS object ID, or "" for files stored in git.
	OID string
}

// member is one readable stream inside a source file: the file itself,
// decompressed, or one file of a zip archive.
type member struct {
	name string
	open func() (io.ReadCloser, error)
}

type multiCloser struct {
	io.Reader
	closers []io.Closer
}

func (m *multiCloser) Close() error {
	var err error
	for i := len(m.closers) - 1; i >= 0; i-- {
		if e := m.closers[i].Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// decompress wraps r according to the file name's last extension, as
// `datasets` extracts compressed files on the fly.
func decompress(name string, rc io.ReadCloser) (io.ReadCloser, string, error) {
	ext := strings.ToLower(path.Ext(name))
	inner := strings.TrimSuffix(name, path.Ext(name))
	switch ext {
	case ".gz", ".gzip":
		z, err := gzip.NewReader(rc)
		if err != nil {
			_ = rc.Close()
			return nil, "", fmt.Errorf("%s: %w", name, err)
		}
		return &multiCloser{Reader: z, closers: []io.Closer{rc, z}}, inner, nil
	case ".bz2":
		return &multiCloser{Reader: bzip2.NewReader(rc), closers: []io.Closer{rc}}, inner, nil
	case ".zst", ".zstd":
		z, err := zstd.NewReader(rc, zstd.WithDecoderMaxMemory(1<<31))
		if err != nil {
			_ = rc.Close()
			return nil, "", err
		}
		return &multiCloser{Reader: z, closers: []io.Closer{rc, closerFunc(func() error { z.Close(); return nil })}}, inner, nil
	case ".xz", ".lz4", ".7z", ".rar":
		_ = rc.Close()
		return nil, "", fmt.Errorf("%w: %s compression", ErrUnsupported, ext)
	}
	return rc, name, nil
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// members lists the readable streams of a file.
func members(ctx context.Context, f File) ([]member, error) {
	if strings.EqualFold(path.Ext(f.Path), ".zip") {
		if f.OpenAt == nil {
			return nil, fmt.Errorf("%w: zip archive without random access", ErrUnsupported)
		}
		ra, err := f.OpenAt(ctx)
		if err != nil {
			return nil, err
		}
		zr, err := zip.NewReader(ra, f.Size)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.Path, err)
		}
		var out []member
		for _, zf := range zr.File {
			name := zf.Name
			base := path.Base(name)
			if zf.FileInfo().IsDir() || strings.HasPrefix(base, ".") || strings.HasPrefix(base, "__") ||
				strings.Contains(name, "__MACOSX/") || strings.Contains(name, "/.") {
				continue
			}
			out = append(out, member{name: name, open: func() (io.ReadCloser, error) {
				rc, err := zf.Open()
				if err != nil {
					return nil, err
				}
				r, _, err := decompress(name, rc)
				return r, err
			}})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
		return out, nil
	}
	return []member{{name: f.Path, open: func() (io.ReadCloser, error) {
		rc, err := f.Open(ctx)
		if err != nil {
			return nil, err
		}
		r, _, err := decompress(f.Path, rc)
		return r, err
	}}}, nil
}

// innerName is the file name without a compression extension.
func innerName(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".gz", ".gzip", ".bz2", ".zst", ".zstd", ".xz":
		return strings.TrimSuffix(name, path.Ext(name))
	}
	return name
}

// chunkReader yields chunks of about size bytes that end at a line break,
// as the `datasets` JSON and text builders read files (f.read(chunksize)
// followed by f.readline()).
type chunkReader struct {
	r    *bufio.Reader
	size int
}

func newChunkReader(r io.Reader, size int) *chunkReader {
	return &chunkReader{r: bufio.NewReaderSize(r, 1<<16), size: size}
}

// next returns the next chunk, or io.EOF.
func (c *chunkReader) next() ([]byte, error) {
	buf := make([]byte, c.size)
	n, err := io.ReadFull(c.r, buf)
	if n == 0 {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, err
	}
	buf = buf[:n]
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if n == c.size {
		line, rerr := c.r.ReadBytes('\n')
		buf = append(buf, line...)
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return nil, rerr
		}
	}
	return buf, nil
}

// readAllLimit reads at most limit bytes and fails beyond that.
func readAllLimit(r io.Reader, limit int64) ([]byte, error) {
	var b bytes.Buffer
	n, err := io.CopyN(&b, r, limit+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if n > limit {
		return nil, fmt.Errorf("%w: a file larger than %d bytes", ErrUnsupported, limit)
	}
	return b.Bytes(), nil
}
