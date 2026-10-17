package gitstore

import (
	"bufio"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// MinPackObjectSize is the smallest per-object limit PackLimitsFor gives:
// trees can legitimately be larger than git.max_blob_size, because a
// folder of 100,000 files has a tree of several MiB.
const MinPackObjectSize = 64 << 20

// DefaultMaxPackObjects is the most objects one pack may hold: twice
// DefaultMaxTreePaths, so a push of a Commit at the path limit fits with
// its trees. git and go-git keep a record of every object while indexing.
const DefaultMaxPackObjects = 2_000_000

// MinUnpacked is the smallest bound PackLimitsFor puts on what a pack
// unpacks to, so a small git.max_pack_size refuses packs by their size.
const MinUnpacked = 1 << 30

// DefaultMaxRefUpdates is the most ref updates one push may send.
const DefaultMaxRefUpdates = 10_000

// PackLimits bound the work a pack can make Hangar do before any object in
// it is trusted. git and go-git rebuild every delta in memory, so a pack of
// a few KiB whose deltas copy one large base many times would otherwise
// make Hangar allocate gigabytes (a "delta bomb").
type PackLimits struct {
	// MaxObjects bounds the number of objects in the pack.
	MaxObjects int64
	// MaxObjectSize bounds the unpacked size of every object that is not a
	// delta, and of every delta's own instructions. git and go-git stream
	// these, so the bound saves work rather than memory.
	MaxObjectSize int64
	// MaxDeltaSize bounds the base size and result size every delta
	// declares. git and go-git hold both in memory while they rebuild it.
	MaxDeltaSize int64
	// MaxUnpacked bounds the sum of the unpacked sizes of every object in
	// the pack, deltas counted at their result size.
	MaxUnpacked int64
	// MaxRefUpdates bounds the ref update commands before a receive-pack
	// request's pack. LimitPack ignores it.
	MaxRefUpdates int
}

// PackLimitsFor returns the limits for pushed packs on an Instance with
// these git.max_blob_size and git.max_pack_size settings: objects and
// deltas up to the larger of maxBlob and MinPackObjectSize, at most
// DefaultMaxPackObjects objects and DefaultMaxRefUpdates ref updates, and
// four times maxPack (at least MinUnpacked) unpacked in all.
func PackLimitsFor(maxBlob, maxPack int64) PackLimits {
	size := max(maxBlob, MinPackObjectSize)
	l := PackLimits{MaxObjects: DefaultMaxPackObjects, MaxObjectSize: size, MaxDeltaSize: size, MaxRefUpdates: DefaultMaxRefUpdates}
	if maxPack > 0 && maxPack <= (1<<62)/4 {
		l.MaxUnpacked = max(4*maxPack, MinUnpacked)
	}
	return l
}

// PackLimitError is a pack that breaks a PackLimits bound. Its message is
// meant for the person pushing.
type PackLimitError struct{ Message string }

func (e *PackLimitError) Error() string { return e.Message }

// ErrMalformedPack is a pack, or receive-pack request, that LimitPack
// cannot parse. It is refused rather than passed on, so a pack the check
// misreads cannot slip past the limits.
var ErrMalformedPack = errors.New("the pack is not valid")

var errMalformed = ErrMalformedPack

// LimitPack returns a reader of the same bytes as r that checks the pack
// against l as it goes, and fails with a *PackLimitError when it breaks a
// bound. The pack's 20-byte trailer is held back until every object has
// been checked, so a reader cannot finish the pack, and so cannot start
// rebuilding its deltas, before the check passes. Close the returned
// reader when done with it.
func LimitPack(r io.Reader, l PackLimits) *PackReader {
	return newLimitReader(r, l, false)
}

// LimitReceivePack is LimitPack for the body of a receive-pack request:
// pkt-lines with the ref update commands, a flush packet and then the pack,
// which a push that only deletes refs leaves out.
func LimitReceivePack(r io.Reader, l PackLimits) *PackReader {
	return newLimitReader(r, l, true)
}

// trailerSize is the size of a SHA-1 pack's trailing checksum, and oidSize
// the size of a REF_DELTA's base object ID.
const (
	trailerSize = 20
	oidSize     = 20
)

func newLimitReader(src io.Reader, l PackLimits, commands bool) *PackReader {
	pr, pw := io.Pipe()
	lr := &PackReader{pr: pr, done: make(chan struct{})}
	fw := &holdback{w: pw}
	rec := &recordingReader{r: src}
	go func() {
		defer close(lr.done)
		tee := io.TeeReader(rec, fw)
		br := bufio.NewReaderSize(tee, 64<<10)
		err := checkStream(br, l, commands)
		// The parser reports any failed read as a malformed pack; say why
		// the read failed instead: the source failed (for example, it went
		// over git.max_pack_size) or the reader was closed.
		var le *PackLimitError
		switch {
		case errors.As(err, &le):
		case rec.err != nil:
			err = rec.err
		case fw.err != nil:
			err = errClosed
		}
		if err == nil {
			// Pass on anything after the pack, then the trailer.
			_, err = io.Copy(io.Discard, br)
		}
		if err == nil {
			err = fw.flush()
		}
		lr.err = err
		_ = pw.CloseWithError(err)
	}()
	return lr
}

// PackReader is the reader LimitPack and LimitReceivePack return.
type PackReader struct {
	pr   *io.PipeReader
	once sync.Once
	done chan struct{}
	err  error
}

func (l *PackReader) Read(p []byte) (int, error) { return l.pr.Read(p) }

// Close stops the check and unblocks it if the reader stopped early.
func (l *PackReader) Close() error {
	l.once.Do(func() { _ = l.pr.CloseWithError(errClosed) })
	return nil
}

var errClosed = errors.New("gitstore: pack reader closed")

// Err returns what the check found: nil, a *PackLimitError,
// ErrMalformedPack or an error reading the source. Call it after Close;
// it waits for the check to stop.
func (l *PackReader) Err() error {
	<-l.done
	if errors.Is(l.err, errClosed) {
		return nil
	}
	return l.err
}

// recordingReader keeps the first error, other than io.EOF, of r.
type recordingReader struct {
	r   io.Reader
	err error
}

func (r *recordingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && r.err == nil {
		r.err = err
	}
	return n, err
}

// holdback forwards everything written to it except the last trailerSize
// bytes, which flush releases.
type holdback struct {
	w   io.Writer
	buf []byte
	err error
}

func (h *holdback) Write(p []byte) (int, error) {
	h.buf = append(h.buf, p...)
	if n := len(h.buf) - trailerSize; n > 0 {
		if _, err := h.w.Write(h.buf[:n]); err != nil {
			h.err = err
			return 0, err
		}
		h.buf = append(h.buf[:0], h.buf[n:]...)
	}
	return len(p), nil
}

func (h *holdback) flush() error {
	if len(h.buf) == 0 {
		return nil
	}
	_, err := h.w.Write(h.buf)
	h.buf = h.buf[:0]
	if err != nil {
		h.err = err
	}
	return err
}

// checkStream reads one pack (after the commands, when commands is set)
// from br and checks it against l.
func checkStream(br *bufio.Reader, l PackLimits, commands bool) error {
	if commands {
		n, err := skipPktLines(br)
		if err != nil {
			return err
		}
		if l.MaxRefUpdates > 0 && n > l.MaxRefUpdates {
			return &PackLimitError{Message: fmt.Sprintf("the push updates more than %d refs. Push fewer branches and tags at a time", l.MaxRefUpdates)}
		}
		// Push options, when the client sends them, are a second section.
		for {
			next, err := br.Peek(4)
			if errors.Is(err, io.EOF) && len(next) == 0 {
				return nil // a push that only deletes refs has no pack
			}
			if string(next) == "PACK" {
				break
			}
			if n, err = skipPktLines(br); err != nil {
				return err
			}
			if l.MaxRefUpdates > 0 && n > l.MaxRefUpdates {
				return &PackLimitError{Message: fmt.Sprintf("the push sends more than %d push options", l.MaxRefUpdates)}
			}
		}
	}
	return checkPack(br, l)
}

// skipPktLines reads pkt-lines up to and including the next flush packet
// and returns how many there were.
func skipPktLines(br *bufio.Reader) (int, error) {
	var hdr [4]byte
	for n := 0; ; n++ {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			return n, errMalformed
		}
		size, err := strconv.ParseUint(string(hdr[:]), 16, 16)
		if err != nil {
			return n, errMalformed
		}
		switch {
		case size == 0:
			return n, nil
		case size < 4:
			return n, errMalformed
		}
		if _, err := br.Discard(int(size) - 4); err != nil {
			return n, errMalformed
		}
	}
}

// Pack object types (gitformat-pack).
const (
	objCommit   = 1
	objTree     = 2
	objBlob     = 3
	objTag      = 4
	objOfsDelta = 6
	objRefDelta = 7
)

func checkPack(br *bufio.Reader, l PackLimits) error {
	var hdr [12]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return errMalformed
	}
	if string(hdr[:4]) != "PACK" {
		return errMalformed
	}
	if v := binary.BigEndian.Uint32(hdr[4:8]); v != 2 && v != 3 {
		return errMalformed
	}
	count := int64(binary.BigEndian.Uint32(hdr[8:12]))
	if l.MaxObjects > 0 && count > l.MaxObjects {
		return &PackLimitError{Message: fmt.Sprintf("the push has %d objects, over the limit of %d. Push fewer commits at a time", count, l.MaxObjects)}
	}
	var unpacked int64
	add := func(n int64) error {
		unpacked += n
		if l.MaxUnpacked > 0 && unpacked > l.MaxUnpacked {
			return &PackLimitError{Message: fmt.Sprintf("the push would unpack to more than %s (four times git.max_pack_size, and at least 1 GiB). Push fewer commits at a time", formatBytes(l.MaxUnpacked))}
		}
		return nil
	}
	// what has a %s for the size.
	tooLarge := func(what string, n int64) error {
		limit := l.MaxObjectSize
		if strings.HasPrefix(what, "a delta ") && l.MaxDeltaSize > 0 {
			limit = l.MaxDeltaSize
		}
		return &PackLimitError{Message: fmt.Sprintf("the push has "+what+", over the limit of %s for one object (the larger of git.max_blob_size and %s). Track large files with Git LFS (git lfs track) and push again",
			formatBytes(n), formatBytes(limit), formatBytes(MinPackObjectSize))}
	}
	for i := int64(0); i < count; i++ {
		typ, size, err := readObjectHeader(br)
		if err != nil {
			return err
		}
		if l.MaxObjectSize > 0 && size > l.MaxObjectSize {
			if typ == objOfsDelta || typ == objRefDelta {
				return tooLarge("a delta of %s", size)
			}
			return tooLarge("an object of %s", size)
		}
		switch typ {
		case objCommit, objTree, objBlob, objTag:
			if err := add(size); err != nil {
				return err
			}
			if err := inflate(br, size, nil); err != nil {
				return err
			}
		case objOfsDelta, objRefDelta:
			if typ == objOfsDelta {
				if err := skipOffset(br); err != nil {
					return err
				}
			} else if _, err := br.Discard(oidSize); err != nil {
				return errMalformed
			}
			var base, result int64
			if err := inflate(br, size, func(head []byte) error {
				var ok bool
				if base, result, ok = deltaSizes(head); !ok {
					return errMalformed
				}
				return nil
			}); err != nil {
				return err
			}
			if l.MaxDeltaSize > 0 && base > l.MaxDeltaSize {
				return tooLarge("a delta whose base object is %s", base)
			}
			if l.MaxDeltaSize > 0 && result > l.MaxDeltaSize {
				return tooLarge("a delta that rebuilds an object of %s", result)
			}
			if err := add(result); err != nil {
				return err
			}
		default:
			return errMalformed
		}
	}
	return nil
}

// readObjectHeader reads an object's type and unpacked size.
func readObjectHeader(br *bufio.Reader) (typ byte, size int64, err error) {
	c, err := br.ReadByte()
	if err != nil {
		return 0, 0, errMalformed
	}
	typ = (c >> 4) & 7
	u := uint64(c & 0x0f)
	for shift := uint(4); c&0x80 != 0; shift += 7 {
		if shift > 56 {
			return 0, 0, errMalformed
		}
		if c, err = br.ReadByte(); err != nil {
			return 0, 0, errMalformed
		}
		u |= uint64(c&0x7f) << shift
	}
	if u > 1<<62 {
		return 0, 0, errMalformed
	}
	return typ, int64(u), nil
}

// skipOffset reads an OFS_DELTA's base offset.
func skipOffset(br *bufio.Reader) error {
	for i := 0; ; i++ {
		c, err := br.ReadByte()
		if err != nil || i > 9 {
			return errMalformed
		}
		if c&0x80 == 0 {
			return nil
		}
	}
}

// deltaHead is enough of a delta to read its two size headers.
const deltaHead = 20

// inflate reads one zlib stream from br and checks that it unpacks to
// exactly size bytes. head, when set, gets the first deltaHead bytes (or
// all of them, if fewer).
func inflate(br *bufio.Reader, size int64, head func([]byte) error) error {
	zr, err := zlib.NewReader(br)
	if err != nil {
		return errMalformed
	}
	defer func() { _ = zr.Close() }()
	var n int64
	if head != nil {
		buf := make([]byte, min(size, deltaHead))
		if _, err := io.ReadFull(zr, buf); err != nil {
			return errMalformed
		}
		if err := head(buf); err != nil {
			return err
		}
		n = int64(len(buf))
	}
	// Read one byte past the declared size, so a stream that unpacks to
	// more than its header says is caught without unpacking all of it.
	m, err := io.Copy(io.Discard, io.LimitReader(zr, size-n+1))
	if err != nil || n+m != size {
		return errMalformed
	}
	return nil
}

// deltaSizes reads a delta's base and result sizes.
func deltaSizes(b []byte) (base, result int64, ok bool) {
	var vals [2]int64
	for i := range vals {
		var u uint64
		var shift uint
		for {
			if len(b) == 0 || shift > 56 {
				return 0, 0, false
			}
			c := b[0]
			b = b[1:]
			u |= uint64(c&0x7f) << shift
			shift += 7
			if c&0x80 == 0 {
				break
			}
		}
		if u > 1<<62 {
			return 0, 0, false
		}
		vals[i] = int64(u)
	}
	return vals[0], vals[1], true
}

// formatBytes gives a size in the largest binary unit that keeps it at
// least 1, such as "64 MiB".
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	f := float64(n) / unit
	i := 0
	for f >= unit && i < len(units)-1 {
		f /= unit
		i++
	}
	s := strconv.FormatFloat(f, 'f', 1, 64)
	if len(s) > 2 && s[len(s)-2:] == ".0" {
		s = s[:len(s)-2]
	}
	return s + " " + units[i]
}
