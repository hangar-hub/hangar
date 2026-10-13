package scan

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/jamesagarside/hangar/internal/scan/pickle"
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

// KindOf returns how a path is checked, from its extension
// (07-indexing-and-scanning.md "Pickle scanner" and "Integrity checks").
// Pickle covers raw pickles and zip-based PyTorch archives.
func KindOf(p string) Kind {
	switch strings.ToLower(path.Ext(p)) {
	case ".bin", ".pt", ".pth", ".ckpt", ".pkl", ".pickle":
		return KindPickle
	case ".joblib":
		return KindJoblib
	case ".npy":
		return KindNumpy
	case ".safetensors":
		return KindSafetensors
	case ".gguf":
		return KindGGUF
	}
	return KindNone
}

var zipMagic = []byte("PK\x03\x04")

// emptyZipMagic starts a zip archive with no members.
var emptyZipMagic = []byte("PK\x05\x06")

func statusOf(c pickle.Classification) Status {
	switch c {
	case pickle.Unsafe:
		return StatusUnsafe
	case pickle.Suspicious:
		return StatusSuspicious
	}
	return StatusSafe
}

// fromPickle turns the imports of one or more pickles into a result,
// listing each import once.
func fromPickle(fr *FileResult, res pickle.Result, member string) {
	seen := map[string]bool{}
	for _, f := range fr.Findings {
		seen[f.Module+"\x00"+f.Name+"\x00"+f.Opcode] = true
	}
	for _, imp := range res.Imports {
		key := imp.Module + "\x00" + imp.Name + "\x00" + imp.Opcode
		if seen[key] {
			continue
		}
		seen[key] = true
		f := Finding{Module: imp.Module, Name: imp.Name, Opcode: imp.Opcode, Status: statusOf(imp.Classification)}
		if member != "" {
			f.Detail = "in " + member
		}
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

func sortFindings(fr *FileResult) {
	sort.SliceStable(fr.Findings, func(i, j int) bool {
		if rank(fr.Findings[i].Status) != rank(fr.Findings[j].Status) {
			return rank(fr.Findings[i].Status) > rank(fr.Findings[j].Status)
		}
		return fr.Findings[i].Module+"."+fr.Findings[i].Name < fr.Findings[j].Module+"."+fr.Findings[j].Name
	})
}

// scanPickleStream scans a raw pickle, or a zip archive read as a stream.
func scanPickleStream(fr *FileResult, r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	head, _ := br.Peek(4)
	if bytes.Equal(head, zipMagic) || bytes.Equal(head, emptyZipMagic) {
		scanZipStream(fr, br)
		return
	}
	res, err := pickle.Scan(br)
	fromPickle(fr, res, "")
	if err != nil {
		fail(fr, err)
	}
}

// isPickleMember reports whether a zip member is a pickle to scan: data.pkl
// in PyTorch archives, and any other .pkl member.
func isPickleMember(name string) bool { return strings.HasSuffix(strings.ToLower(name), ".pkl") }

// scanZipAt scans the pickle members of a zip archive with random access,
// reading only the central directory and those members.
func scanZipAt(fr *FileResult, ra io.ReaderAt, size int64) {
	zr, err := zip.NewReader(ra, size)
	if err != nil {
		fail(fr, fmt.Errorf("not a valid zip archive: %w", err))
		return
	}
	found := false
	for _, f := range zr.File {
		if !isPickleMember(f.Name) {
			continue
		}
		found = true
		rc, err := f.Open()
		if err != nil {
			fail(fr, fmt.Errorf("%s: %w", f.Name, err))
			continue
		}
		res, err := pickle.Scan(rc)
		_ = rc.Close()
		fromPickle(fr, res, f.Name)
		if err != nil {
			fail(fr, fmt.Errorf("%s: %w", f.Name, err))
		}
	}
	if !found {
		fail(fr, errors.New("the zip archive has no pickle member"))
	}
}

// scanZipStream walks a zip archive's local file headers in order, scanning
// pickle members and discarding the rest as they stream past.
func scanZipStream(fr *FileResult, r *bufio.Reader) {
	found := false
	var hdr [26]byte
	for {
		var sig [4]byte
		if _, err := io.ReadFull(r, sig[:]); err != nil {
			fail(fr, errors.New("the zip archive ends unexpectedly"))
			return
		}
		switch binary.LittleEndian.Uint32(sig[:]) {
		case 0x04034b50:
		case 0x02014b50, 0x06054b50, 0x06064b50:
			if !found {
				fail(fr, errors.New("the zip archive has no pickle member"))
			}
			return
		default:
			fail(fr, errors.New("not a valid zip archive"))
			return
		}
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			fail(fr, errors.New("the zip archive ends unexpectedly"))
			return
		}
		flags := binary.LittleEndian.Uint16(hdr[2:])
		method := binary.LittleEndian.Uint16(hdr[4:])
		csize := uint64(binary.LittleEndian.Uint32(hdr[14:]))
		nameLen := int(binary.LittleEndian.Uint16(hdr[22:]))
		extraLen := int(binary.LittleEndian.Uint16(hdr[24:]))
		nameExtra := make([]byte, nameLen+extraLen)
		if _, err := io.ReadFull(r, nameExtra); err != nil {
			fail(fr, errors.New("the zip archive ends unexpectedly"))
			return
		}
		name := string(nameExtra[:nameLen])
		if csize == 0xffffffff {
			csize = zip64Size(nameExtra[nameLen:])
		}
		descriptor := flags&0x8 != 0 && csize == 0
		if descriptor && method != zip.Deflate {
			fail(fr, fmt.Errorf("%s: stored members with data descriptors cannot be scanned as a stream", name))
			return
		}
		// A deflated member ends where its compressed stream ends, so it
		// can be read without knowing its size.
		var body io.Reader = io.LimitReader(r, int64(csize))
		if descriptor {
			body = r
		}
		var member io.Reader
		switch method {
		case zip.Store:
			member = body
		case zip.Deflate:
			member = flate.NewReader(body)
		default:
			if isPickleMember(name) || descriptor {
				fail(fr, fmt.Errorf("%s: unsupported compression method %d", name, method))
				return
			}
		}
		if isPickleMember(name) {
			found = true
			res, err := pickle.Scan(member)
			fromPickle(fr, res, name)
			if err != nil {
				fail(fr, fmt.Errorf("%s: %w", name, err))
			}
		}
		drain := body
		if descriptor {
			drain = member
		}
		if _, err := io.Copy(io.Discard, drain); err != nil {
			fail(fr, fmt.Errorf("%s: %w", name, err))
			return
		}
		if flags&0x8 != 0 {
			// Skip the data descriptor: an optional signature, the CRC and
			// both sizes.
			n := 12
			if sig, err := r.Peek(4); err == nil && binary.LittleEndian.Uint32(sig) == 0x08074b50 {
				n = 16
			}
			if _, err := r.Discard(n); err != nil {
				fail(fr, errors.New("the zip archive ends unexpectedly"))
				return
			}
		}
	}
}

// zip64Size reads the compressed size from a zip64 extended information
// extra field in a local header.
func zip64Size(extra []byte) uint64 {
	for len(extra) >= 4 {
		id := binary.LittleEndian.Uint16(extra)
		n := int(binary.LittleEndian.Uint16(extra[2:]))
		if 4+n > len(extra) {
			return 0
		}
		if id == 0x0001 && n >= 16 {
			return binary.LittleEndian.Uint64(extra[12:])
		}
		extra = extra[4+n:]
	}
	return 0
}

// scanNumpy checks a .npy file: object arrays hold a pickle after the
// header; other arrays hold raw numbers and are safe.
func scanNumpy(fr *FileResult, r io.Reader) {
	br := bufio.NewReader(r)
	var pre [8]byte
	if _, err := io.ReadFull(br, pre[:]); err != nil || string(pre[:6]) != "\x93NUMPY" {
		fail(fr, errors.New("not a NumPy .npy file"))
		return
	}
	var hlen int
	switch pre[6] {
	case 1:
		var b [2]byte
		if _, err := io.ReadFull(br, b[:]); err != nil {
			fail(fr, errors.New("truncated .npy header"))
			return
		}
		hlen = int(binary.LittleEndian.Uint16(b[:]))
	case 2, 3:
		var b [4]byte
		if _, err := io.ReadFull(br, b[:]); err != nil {
			fail(fr, errors.New("truncated .npy header"))
			return
		}
		hlen = int(binary.LittleEndian.Uint32(b[:]))
	default:
		fail(fr, fmt.Errorf("unsupported .npy version %d", pre[6]))
		return
	}
	if hlen > 1<<20 {
		fail(fr, errors.New(".npy header is too large"))
		return
	}
	header := make([]byte, hlen)
	if _, err := io.ReadFull(br, header); err != nil {
		fail(fr, errors.New("truncated .npy header"))
		return
	}
	if !objectDType(string(header)) {
		fr.Status = Worst(fr.Status, StatusSafe)
		return
	}
	res, err := pickle.Scan(br)
	fromPickle(fr, res, "")
	if err != nil {
		fail(fr, err)
	}
}

var objectCodeRe = regexp.MustCompile(`['"][|<>=]?O\d*['"]`)

// objectDType reports whether a .npy header's descr is, or contains, an
// object dtype, which NumPy stores with pickle (allow_pickle). A header
// without a descr is treated as one, so the data is scanned.
func objectDType(header string) bool {
	i := strings.Index(header, "'descr'")
	if i < 0 {
		i = strings.Index(header, `"descr"`)
	}
	if i < 0 {
		return true
	}
	rest := header[i+7:]
	for _, key := range []string{"'fortran_order'", "'shape'", `"fortran_order"`, `"shape"`} {
		if j := strings.Index(rest, key); j >= 0 {
			rest = rest[:j]
		}
	}
	return objectCodeRe.MatchString(rest)
}

// scanJoblib checks a joblib file: a pickle, optionally compressed.
func scanJoblib(fr *FileResult, r io.Reader) {
	br := bufio.NewReader(r)
	head, _ := br.Peek(6)
	var in io.Reader = br
	switch {
	case len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b:
		gz, err := gzip.NewReader(br)
		if err != nil {
			fail(fr, err)
			return
		}
		in = gz
	case len(head) >= 2 && head[0] == 0x78 && (uint16(head[0])<<8|uint16(head[1]))%31 == 0:
		zr, err := zlib.NewReader(br)
		if err != nil {
			fail(fr, err)
			return
		}
		in = zr
	case len(head) >= 3 && string(head[:3]) == "BZh":
		in = bzip2.NewReader(br)
	case len(head) >= 6 && bytes.Equal(head, []byte{0xfd, '7', 'z', 'X', 'Z', 0}):
		fail(fr, errors.New("the file is compressed with xz, which Hangar cannot scan"))
		return
	case len(head) >= 4 && bytes.Equal(head[:4], []byte{0x04, 0x22, 0x4d, 0x18}):
		fail(fr, errors.New("the file is compressed with lz4, which Hangar cannot scan"))
		return
	}
	res, err := pickle.Scan(in)
	fromPickle(fr, res, "")
	if err != nil {
		fail(fr, err)
	}
}
