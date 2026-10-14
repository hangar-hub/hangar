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

	"github.com/jamesagarside/hangar/internal/datasets/layout"
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
	// archive is the Repo path of the zip archive that holds this file,
	// for the entries folder builders read from archives; Path is then
	// archive + "/" + entry.
	archive, entry string
}

// maxArchiveEntryInMemory caps an archive entry read into memory for
// random access (a Parquet metadata file inside an archive).
const maxArchiveEntryInMemory = 256 << 20

// openZip opens a zip archive in the Repo, refusing archives with more
// entries than layout.MaxArchiveEntries before archive/zip reads them all.
func openZip(ctx context.Context, f File) (*zip.Reader, error) {
	if f.OpenAt == nil {
		return nil, fmt.Errorf("%w: zip archive without random access", ErrUnsupported)
	}
	ra, err := f.OpenAt(ctx)
	if err != nil {
		return nil, err
	}
	if err := layout.ZipEntries(ra, f.Size, func(string) error { return nil }); err != nil {
		return nil, fmt.Errorf("%s: %w", f.Path, err)
	}
	zr, err := zip.NewReader(ra, f.Size)
	if err != nil {
		return nil, fmt.Errorf("%s: %w: %w", f.Path, layout.ErrInvalidArchive, err)
	}
	return zr, nil
}

// archiveFiles lists the files of a zip archive as FilesIterable walks the
// extracted archive: hidden files and folders skipped, a folder's files
// before its subfolders. Each opens the entry as it is stored, without
// decompressing a .gz or other compressed entry.
func archiveFiles(ctx context.Context, f File) ([]File, error) {
	zr, err := openZip(ctx, f)
	if err != nil {
		return nil, err
	}
	var out []File
	for _, zf := range zr.File {
		name := zf.Name
		if zf.FileInfo().IsDir() || hiddenMember(name) {
			continue
		}
		size := int64(zf.UncompressedSize64) //nolint:gosec // only a size hint for in-memory reads
		open := func(context.Context) (io.ReadCloser, error) { return zf.Open() }
		out = append(out, File{Path: f.Path + "/" + strings.Trim(name, "/"), Size: size, archive: f.Path, entry: strings.Trim(name, "/"),
			Open: open,
			OpenAt: func(ctx context.Context) (io.ReaderAt, error) {
				rc, err := open(ctx)
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				b, err := readAllLimit(rc, maxArchiveEntryInMemory)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", name, err)
				}
				return bytes.NewReader(b), nil
			}})
	}
	sort.SliceStable(out, func(i, j int) bool { return walkLess(out[i].entry, out[j].entry) })
	return out, nil
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
	if f.archive == "" && strings.EqualFold(path.Ext(f.Path), ".zip") {
		zr, err := openZip(ctx, f)
		if err != nil {
			return nil, err
		}
		var out []member
		for _, zf := range zr.File {
			name := zf.Name
			if zf.FileInfo().IsDir() || hiddenMember(name) {
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
		sort.Slice(out, func(i, j int) bool { return walkLess(out[i].name, out[j].name) })
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

// hiddenMember reports whether FilesIterable skips an archive member: any
// part of its path starts with "." or "__".
func hiddenMember(name string) bool {
	for _, part := range strings.Split(strings.Trim(name, "/"), "/") {
		if strings.HasPrefix(part, ".") || strings.HasPrefix(part, "__") {
			return true
		}
	}
	return false
}

// walkLess orders archive members as FilesIterable walks the extracted
// archive: top down, a directory's files (sorted) before its
// subdirectories (sorted).
func walkLess(a, b string) bool {
	pa, pb := strings.Split(a, "/"), strings.Split(b, "/")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] == pb[i] {
			continue
		}
		aFile, bFile := i == len(pa)-1, i == len(pb)-1
		if aFile != bFile {
			return aFile
		}
		return pa[i] < pb[i]
	}
	return len(pa) < len(pb)
}
