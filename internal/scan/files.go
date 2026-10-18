package scan

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"

	"github.com/hangar-hub/hangar/internal/scan/pickle"
)

// Kind is how a file is checked.
type Kind string

// Kinds of file.
const (
	KindNone        Kind = ""
	KindPickle      Kind = "pickle"
	KindJoblib      Kind = "joblib"
	KindNumpy       Kind = "numpy"
	KindSafetensors Kind = "safetensors"
	KindGGUF        Kind = "gguf"
)

// compressionSuffixes are the extensions joblib.dump and
// pandas.to_pickle add for a compressed pickle, such as model.pkl.gz;
// joblib.load and pandas.read_pickle decompress them. tar is pandas'
// archive and the .pth.tar name many PyTorch checkpoints use.
var compressionSuffixes = []string{".gz", ".bz2", ".xz", ".lzma", ".z", ".zst", ".lz4", ".zip", ".tar"}

// KindOf returns how a path is checked, from its name
// (07-indexing-and-scanning.md "Pickle scanner" and "Integrity checks").
// Pickle covers raw pickles and zip-based PyTorch archives; joblib, NumPy
// and pickle names with a compression suffix are checked as the name
// before the suffix. The content then decides how the file is read.
func KindOf(p string) Kind {
	ext := strings.ToLower(path.Ext(p))
	for _, c := range compressionSuffixes {
		if ext == c {
			inner := strings.TrimSuffix(p, path.Ext(p))
			if k := kindOfExt(strings.ToLower(path.Ext(inner))); k == KindPickle || k == KindJoblib || k == KindNumpy {
				return k
			}
			return KindNone
		}
	}
	return kindOfExt(ext)
}

func kindOfExt(ext string) Kind {
	switch ext {
	case ".bin", ".pt", ".pth", ".ckpt", ".pkl", ".pickle":
		return KindPickle
	case ".joblib":
		return KindJoblib
	case ".npy", ".npz":
		return KindNumpy
	case ".safetensors":
		return KindSafetensors
	case ".gguf":
		return KindGGUF
	}
	return KindNone
}

// RunsCode reports whether loading a file of this kind can run code: it
// is read with pickle by torch.load, joblib.load, numpy.load or
// pandas.read_pickle.
func (k Kind) RunsCode() bool { return k == KindPickle || k == KindJoblib || k == KindNumpy }

var (
	zipMagic      = []byte("PK\x03\x04")
	emptyZipMagic = []byte("PK\x05\x06")
	npyMagic      = []byte("\x93NUMPY")
)

func statusOf(c pickle.Classification) Status {
	switch c {
	case pickle.Unsafe:
		return StatusUnsafe
	case pickle.Suspicious:
		return StatusSuspicious
	}
	return StatusSafe
}

// fromPickle adds the imports of one or more pickles to a result, listing
// each import once.
func fromPickle(fr *FileResult, res pickle.Result, member string) {
	seen := map[string]bool{}
	for _, f := range fr.Findings {
		seen[f.Module+"\x00"+f.Name+"\x00"+f.Opcode+"\x00"+f.Detail] = true
	}
	detail := ""
	if member != "" {
		detail = "in " + member
	}
	for _, imp := range res.Imports {
		key := imp.Module + "\x00" + imp.Name + "\x00" + imp.Opcode + "\x00" + detail
		if seen[key] {
			continue
		}
		seen[key] = true
		f := Finding{Module: imp.Module, Name: imp.Name, Opcode: imp.Opcode, Status: statusOf(imp.Classification), Detail: detail}
		fr.Findings = append(fr.Findings, f)
		fr.Status = Worst(fr.Status, f.Status)
	}
}

func fail(fr *FileResult, err error) {
	if fr.Error == "" {
		fr.Error = err.Error()
	}
	fr.Status = Worst(fr.Status, StatusError)
}

// partial records that the scan could not read every part of the file a
// loader runs, so the Gate fails closed for it.
func partial(fr *FileResult, err error) {
	if !fr.Partial {
		fr.Error = ""
	}
	fr.Partial = true
	fail(fr, err)
}

func sortFindings(fr *FileResult) {
	sort.SliceStable(fr.Findings, func(i, j int) bool {
		if rank(fr.Findings[i].Status) != rank(fr.Findings[j].Status) {
			return rank(fr.Findings[i].Status) > rank(fr.Findings[j].Status)
		}
		return fr.Findings[i].Module+"."+fr.Findings[i].Name < fr.Findings[j].Module+"."+fr.Findings[j].Name
	})
}

// content is a file to scan: random access for archives, and a stream
// opened afresh for each reading of the whole file.
type content struct {
	ra   io.ReaderAt
	size int64
	open func() (io.ReadCloser, error)
}

// ioError marks a failure to read the file itself, such as a Blob store
// outage, as opposed to what the file holds: the scan Job fails and is
// retried instead of recording a result.
type ioError struct{ err error }

func (e ioError) Error() string { return e.err.Error() }
func (e ioError) Unwrap() error { return e.err }

// trackedReader remembers the first read error that is not the end of the
// data.
type trackedReader struct {
	r   io.Reader
	err error
}

func (t *trackedReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && t.err == nil {
		t.err = err
	}
	return n, err
}

// verdict is what one reading of a file found.
type verdict struct {
	res pickle.Result
	err error
}

// scanner holds the readings of one file.
type scanner struct {
	ctx context.Context
	fr  *FileResult
}

// pass reads a stream with one pickle reading. A read error of the stream
// itself is returned; what the reading found is added to the result.
func (s *scanner) pass(open func() (io.ReadCloser, error), skip int64, read func(io.Reader) (pickle.Result, error), member string) (verdict, error) {
	rc, err := open()
	if err != nil {
		var de decompressError
		if errors.As(err, &de) {
			uerr := fmt.Errorf("%w: %w", pickle.ErrUnsupported, err)
			partial(s.fr, prefixed(member, uerr))
			return verdict{err: uerr}, nil
		}
		return verdict{}, ioError{err}
	}
	defer func() { _ = rc.Close() }()
	tr := &trackedReader{r: rc}
	var in io.Reader = tr
	if skip > 0 {
		if _, err := io.CopyN(io.Discard, in, skip); err != nil {
			return verdict{}, ioError{err}
		}
	}
	res, perr := read(in)
	if tr.err != nil {
		var de decompressError
		if !errors.As(tr.err, &de) {
			return verdict{}, ioError{tr.err}
		}
		perr = fmt.Errorf("%w: %w", pickle.ErrUnsupported, tr.err)
	}
	if err := s.ctx.Err(); err != nil {
		return verdict{}, err
	}
	fromPickle(s.fr, res, member)
	if perr != nil && errors.Is(perr, pickle.ErrUnsupported) {
		partial(s.fr, prefixed(member, perr))
	}
	return verdict{res: res, err: perr}, nil
}

func prefixed(member string, err error) error {
	if member == "" {
		return err
	}
	return fmt.Errorf("%s: %w", member, err)
}

// pickleStream reads a stream that a loader unpickles from its first byte:
// as pickle.load and torch.load do, and, when the stream names joblib's
// array wrapper or the file is a joblib file, as joblib.load does. It
// records a definitive parse error only when every reading failed and the
// data is a pickle at all.
func (s *scanner) pickleStream(open func() (io.ReadCloser, error), skip int64, joblib bool, member string) error {
	plain, err := s.pass(open, skip, pickle.Scan, member)
	if err != nil {
		return err
	}
	last := plain
	if joblib || plain.res.Joblib {
		jl, err := s.pass(open, skip, pickle.ScanJoblib, member)
		if err != nil {
			return err
		}
		if jl.err == nil || plain.err == nil {
			return nil
		}
		last = jl
	}
	if last.err != nil && !errors.Is(last.err, pickle.ErrUnsupported) && !last.res.NotPickle {
		fail(s.fr, prefixed(member, last.err))
	}
	return nil
}

// decompressError is a corrupt compressed stream. The scan cannot tell
// how far a loader's decompressor gets, so it fails closed.
type decompressError struct{ err error }

func (e decompressError) Error() string { return "the compressed data is not valid: " + e.err.Error() }

type decompressReader struct {
	r io.Reader
}

func (d decompressReader) Read(p []byte) (int, error) {
	n, err := d.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		var ie ioError
		if errors.As(err, &ie) {
			return n, err
		}
		return n, decompressError{err}
	}
	return n, err
}

// compression names the compressor whose magic starts head, as joblib's
// _detect_compressor and pandas' readers recognise them.
func compression(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		return "gzip"
	case len(head) >= 2 && head[0] == 0x78 && (uint16(head[0])<<8|uint16(head[1]))%31 == 0:
		return "zlib"
	case bytes.HasPrefix(head, []byte("BZh")):
		return "bz2"
	case bytes.HasPrefix(head, []byte{0xfd, '7', 'z', 'X', 'Z', 0}):
		return "xz"
	case bytes.HasPrefix(head, []byte{0x5d, 0x00}):
		return "lzma"
	case bytes.HasPrefix(head, []byte{0x04, 0x22, 0x4d, 0x18}):
		return "lz4"
	case bytes.HasPrefix(head, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		return "zstd"
	}
	return ""
}

// decompressed returns an opener of the decompressed stream, or an error
// for a compressor Hangar cannot read.
func decompressed(open func() (io.ReadCloser, error), kind string) (func() (io.ReadCloser, error), error) {
	switch kind {
	case "xz", "lzma":
		return nil, fmt.Errorf("%w: the file is compressed with %s, which Hangar cannot read", pickle.ErrUnsupported, kind)
	}
	return func() (io.ReadCloser, error) {
		rc, err := open()
		if err != nil {
			return nil, err
		}
		src := ioErrReader{rc}
		var r io.Reader
		switch kind {
		case "gzip":
			gz, err := gzip.NewReader(src)
			if err != nil {
				_ = rc.Close()
				return nil, decompressError{err}
			}
			r = gz
		case "zlib":
			zr, err := zlib.NewReader(src)
			if err != nil {
				_ = rc.Close()
				return nil, decompressError{err}
			}
			r = zr
		case "bz2":
			r = bzip2.NewReader(src)
		case "lz4":
			r = lz4.NewReader(src)
		case "zstd":
			zr, err := zstd.NewReader(src, zstd.WithDecoderMaxMemory(256<<20))
			if err != nil {
				_ = rc.Close()
				return nil, decompressError{err}
			}
			r = zr.IOReadCloser()
		}
		return readCloser{Reader: decompressReader{r}, close: func() error {
			if c, ok := r.(io.Closer); ok {
				_ = c.Close()
			}
			return rc.Close()
		}}, nil
	}, nil
}

type readCloser struct {
	io.Reader
	close func() error
}

func (r readCloser) Close() error { return r.close() }

// ioErrReaderAt marks read errors of the file as ioError, so a zip reader
// that passes them on is not taken for a corrupt archive.
type ioErrReaderAt struct{ ra io.ReaderAt }

func (e ioErrReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := e.ra.ReadAt(p, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, ioError{err}
	}
	return n, err
}

// ioErrReader marks read errors of the underlying file as ioError, so a
// decompressor that passes them on is not taken for corrupt data.
type ioErrReader struct{ r io.Reader }

func (e ioErrReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, ioError{err}
	}
	return n, err
}

// isTar reports whether head is a tar header: its checksum field matches,
// as Python's tarfile checks it. Old PyTorch files are tar archives whose
// members torch.load unpickles.
func isTar(head []byte) bool {
	if len(head) < 512 {
		return false
	}
	field := strings.TrimSpace(strings.Trim(string(head[148:156]), "\x00 "))
	want, err := strconv.ParseInt(field, 8, 64)
	if err != nil {
		return false
	}
	var unsigned, signed int64
	for i, b := range head[:512] {
		if i >= 148 && i < 156 {
			b = ' '
		}
		unsigned += int64(b)
		signed += int64(int8(b)) //nolint:gosec // tarfile also accepts the signed sum.
	}
	return want == unsigned || want == signed
}

// readHead reads up to 512 bytes from the start of the file.
func (c content) readHead() ([]byte, error) {
	head := make([]byte, min(c.size, 512))
	n, err := c.ra.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, ioError{err}
	}
	return head[:n], nil
}

// scanLoadable checks a file that a loader may unpickle (KindPickle,
// KindJoblib and KindNumpy) by what it holds, whatever its name: a zip
// archive (PyTorch, .npz or a pandas zip), a compressed pickle, a NumPy
// .npy file, an old PyTorch tar archive, or a raw pickle stream.
func (s *scanner) scanLoadable(k Kind, c content) error {
	head, err := c.readHead()
	if err != nil {
		return err
	}
	switch {
	case bytes.HasPrefix(head, zipMagic) || bytes.HasPrefix(head, emptyZipMagic):
		return s.scanZip(c)
	case bytes.HasPrefix(head, npyMagic):
		return s.scanNumpy(c.open, "")
	case isTar(head):
		partial(s.fr, fmt.Errorf("%w: the file is a tar archive, which older PyTorch versions load with pickle; Hangar cannot read it", pickle.ErrUnsupported))
		return nil
	}
	// A loader that does not decompress reads the bytes as they are.
	if err := s.pickleStream(c.open, 0, k == KindJoblib, ""); err != nil {
		return err
	}
	kind := compression(head)
	if kind == "" {
		return nil
	}
	// joblib.load and pandas.read_pickle decompress first.
	open, err := decompressed(c.open, kind)
	if err != nil {
		partial(s.fr, err)
		return nil
	}
	return s.scanDecompressed(open)
}

// scanDecompressed checks a decompressed stream as joblib.load and
// pandas.read_pickle read it. Once the scan has read the decompressed
// data, an error the plain reading of the compressed bytes found is not
// the file's.
func (s *scanner) scanDecompressed(open func() (io.ReadCloser, error)) error {
	if s.fr.Error != "" && !s.fr.Partial {
		s.fr.Error = ""
		s.fr.Status = StatusSafe
		for _, f := range s.fr.Findings {
			s.fr.Status = Worst(s.fr.Status, f.Status)
		}
	}
	rc, err := open()
	if err != nil {
		var de decompressError
		if errors.As(err, &de) {
			partial(s.fr, fmt.Errorf("%w: %w", pickle.ErrUnsupported, err))
			return nil
		}
		return ioError{err}
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(rc, head)
	_ = rc.Close()
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		var de decompressError
		if errors.As(err, &de) {
			partial(s.fr, fmt.Errorf("%w: %w", pickle.ErrUnsupported, err))
			return nil
		}
		return ioError{err}
	}
	head = head[:n]
	switch {
	case bytes.HasPrefix(head, zipMagic) || bytes.HasPrefix(head, emptyZipMagic) || isTar(head) || compression(head) != "":
		partial(s.fr, fmt.Errorf("%w: an archive or compressed file inside a compressed file", pickle.ErrUnsupported))
		return nil
	case bytes.HasPrefix(head, npyMagic):
		return s.scanNumpy(open, "")
	}
	return s.pickleStream(open, 0, true, "")
}

// scanZip checks a zip archive: a PyTorch archive (torch.load reads its
// data.pkl, and torch.jit.load its other .pkl members), a NumPy .npz
// (numpy.load reads its .npy members) or a pandas zip (read_pickle
// unpickles its one member). Members are read with random access, so
// tensor data is never read.
func (s *scanner) scanZip(c content) error {
	zr, err := zip.NewReader(ioErrReaderAt{c.ra}, c.size)
	if err != nil {
		var ie ioError
		if errors.As(err, &ie) {
			return err
		}
		partial(s.fr, fmt.Errorf("%w: the zip archive cannot be read: %w", pickle.ErrUnsupported, err))
		return nil
	}
	torch := false
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "data.pkl") {
			torch = true
		}
	}
	if len(zr.File) == 0 {
		fail(s.fr, errors.New("the zip archive is empty"))
		return nil
	}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") {
			continue
		}
		open := func() (io.ReadCloser, error) {
			rc, err := f.Open()
			if err != nil {
				return nil, decompressError{err}
			}
			return struct {
				io.Reader
				io.Closer
			}{decompressReader{rc}, rc}, nil
		}
		head, err := memberHead(f)
		if err != nil {
			var de decompressError
			if errors.As(err, &de) {
				partial(s.fr, fmt.Errorf("%w: %s: %w", pickle.ErrUnsupported, f.Name, err))
				continue
			}
			return err
		}
		switch {
		case bytes.HasPrefix(head, npyMagic):
			if err := s.scanNumpy(open, f.Name); err != nil {
				return err
			}
		case strings.HasSuffix(strings.ToLower(f.Name), ".pkl") || !torch:
			// In a PyTorch archive only the pickles run; other members are
			// tensor data. Any other zip may be a pandas pickle.
			if err := s.pickleStream(open, 0, false, f.Name); err != nil {
				return err
			}
		}
	}
	return nil
}

func memberHead(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, decompressError{err}
	}
	defer func() { _ = rc.Close() }()
	head := make([]byte, len(npyMagic))
	n, err := io.ReadFull(rc, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		var ie ioError
		if errors.As(err, &ie) {
			return nil, err
		}
		return nil, decompressError{err}
	}
	return head[:n], nil
}

// scanNumpy checks a .npy stream: an array whose dtype holds objects
// stores a pickle after the header, which numpy.load reads with
// allow_pickle=True. Other arrays hold raw numbers.
func (s *scanner) scanNumpy(open func() (io.ReadCloser, error), member string) error {
	rc, err := open()
	if err != nil {
		return ioError{err}
	}
	tr := &trackedReader{r: rc}
	br := bufio.NewReader(tr)
	hlen, header, herr := npyHeader(br)
	_ = rc.Close()
	if tr.err != nil {
		var de decompressError
		if errors.As(tr.err, &de) {
			partial(s.fr, prefixed(member, fmt.Errorf("%w: %w", pickle.ErrUnsupported, tr.err)))
			return nil
		}
		return ioError{tr.err}
	}
	if herr != nil {
		fail(s.fr, prefixed(member, herr))
		return nil
	}
	if !maybeObjectDType(header) {
		return nil
	}
	return s.pickleStream(open, hlen, false, member)
}

// npyHeader reads a .npy header and returns the offset of the data and
// the header text.
func npyHeader(br *bufio.Reader) (int64, string, error) {
	var pre [8]byte
	if _, err := io.ReadFull(br, pre[:]); err != nil || !bytes.Equal(pre[:6], npyMagic) {
		return 0, "", errors.New("not a NumPy .npy file")
	}
	var hlen, width int64
	switch pre[6] {
	case 1:
		var b [2]byte
		if _, err := io.ReadFull(br, b[:]); err != nil {
			return 0, "", errors.New("truncated .npy header")
		}
		hlen, width = int64(binary.LittleEndian.Uint16(b[:])), 2
	case 2, 3:
		var b [4]byte
		if _, err := io.ReadFull(br, b[:]); err != nil {
			return 0, "", errors.New("truncated .npy header")
		}
		hlen, width = int64(binary.LittleEndian.Uint32(b[:])), 4
	default:
		return 0, "", fmt.Errorf("unsupported .npy version %d", pre[6])
	}
	if hlen > 1<<20 {
		return 0, "", errors.New(".npy header is too large")
	}
	header := make([]byte, hlen)
	if _, err := io.ReadFull(br, header); err != nil {
		return 0, "", errors.New("truncated .npy header")
	}
	return 8 + width + hlen, string(header), nil
}

// plainDescr matches a .npy descr that is one simple, fixed-size type,
// such as '<f4', '|b1' or '<U8': arrays of it hold raw numbers or text.
var plainDescr = regexp.MustCompile(`^['"][<>|=]?[biufcmMSUV?]\d*(\[[A-Za-z0-9]+\])?['"]$`)

// maybeObjectDType reports whether a .npy header's descr may hold objects,
// which NumPy stores with pickle. Only a descr that is one simple numeric
// or string type is known not to; structured, object and unreadable
// descrs are scanned.
func maybeObjectDType(header string) bool {
	i := strings.Index(header, "'descr'")
	if i < 0 {
		i = strings.Index(header, `"descr"`)
	}
	if i < 0 {
		return true
	}
	rest := strings.TrimSpace(header[i+len("'descr'"):])
	rest, ok := strings.CutPrefix(rest, ":")
	if !ok {
		return true
	}
	rest = strings.TrimSpace(rest)
	if rest == "" || (rest[0] != '\'' && rest[0] != '"') {
		return true
	}
	end := strings.IndexByte(rest[1:], rest[0])
	if end < 0 {
		return true
	}
	return !plainDescr.MatchString(rest[:end+2])
}
