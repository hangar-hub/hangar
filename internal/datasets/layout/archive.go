package layout

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// Zip record signatures and sizes (APPNOTE.TXT 4.3).
const (
	zipEOCDSig        = 0x06054b50
	zipEOCDLen        = 22
	zip64LocatorSig   = 0x07064b50
	zip64LocatorLen   = 20
	zip64EOCDSig      = 0x06064b50
	zip64EOCDLen      = 56
	zipCentralSig     = 0x02014b50
	zipCentralLen     = 46
	zipMaxCommentLen  = 0xffff
	archivesInference = 10
)

// MaxArchiveEntries bounds the entries of one zip archive Hangar lists or
// converts. Each costs memory for its name, and an archive's central
// directory can claim millions in a few megabytes.
const MaxArchiveEntries = 1 << 20

// ZipEntries calls fn with the name of each entry of the zip archive r of
// size bytes, in the order of its central directory, reading only the
// directory. An archive that is not a valid zip, or that has more than
// MaxArchiveEntries entries, gives ErrInvalidArchive; an error reading r
// is returned as it is.
func ZipEntries(r io.ReaderAt, size int64, fn func(name string) error) error {
	count, dirSize, dirOffset, err := zipDirectory(r, size)
	if err != nil {
		return err
	}
	if count > MaxArchiveEntries {
		return fmt.Errorf("%w: %d entries is more than %d", ErrInvalidArchive, count, MaxArchiveEntries)
	}
	br := bufio.NewReaderSize(io.NewSectionReader(r, dirOffset, dirSize), 64<<10)
	var head [zipCentralLen]byte
	for i := uint64(0); i < count; i++ {
		if _, err := io.ReadFull(br, head[:]); err != nil {
			return zipReadError(err, "central directory entry %d", i)
		}
		if binary.LittleEndian.Uint32(head[:]) != zipCentralSig {
			return fmt.Errorf("%w: central directory entry %d has no signature", ErrInvalidArchive, i)
		}
		nameLen := int(binary.LittleEndian.Uint16(head[28:]))
		skip := int64(binary.LittleEndian.Uint16(head[30:])) + int64(binary.LittleEndian.Uint16(head[32:]))
		name := make([]byte, nameLen)
		if _, err := io.ReadFull(br, name); err != nil {
			return zipReadError(err, "central directory entry %d", i)
		}
		if _, err := br.Discard(int(skip)); err != nil {
			return zipReadError(err, "central directory entry %d", i)
		}
		if err := fn(string(name)); err != nil {
			return err
		}
	}
	return nil
}

// zipReadError is ErrInvalidArchive for a directory that ends early, and
// the read error otherwise.
func zipReadError(err error, format string, args ...any) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: "+format+" runs past the directory", append([]any{ErrInvalidArchive}, args...)...)
	}
	return err
}

// zipDirectory finds the central directory from the end of central
// directory record, or its zip64 form.
func zipDirectory(r io.ReaderAt, size int64) (count uint64, dirSize, dirOffset int64, err error) {
	if size < zipEOCDLen {
		return 0, 0, 0, fmt.Errorf("%w: %d bytes is too short", ErrInvalidArchive, size)
	}
	tailLen := min(size, zipEOCDLen+zipMaxCommentLen+zip64LocatorLen)
	tail := make([]byte, tailLen)
	if _, err := r.ReadAt(tail, size-tailLen); err != nil && err != io.EOF {
		return 0, 0, 0, err
	}
	at := -1
	for i := len(tail) - zipEOCDLen; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) == zipEOCDSig && i+zipEOCDLen+int(binary.LittleEndian.Uint16(tail[i+20:])) <= len(tail) {
			at = i
			break
		}
	}
	if at < 0 {
		return 0, 0, 0, fmt.Errorf("%w: no end of central directory record", ErrInvalidArchive)
	}
	eocd := tail[at:]
	count = uint64(binary.LittleEndian.Uint16(eocd[10:]))
	dirSize = int64(binary.LittleEndian.Uint32(eocd[12:]))
	dirOffset = int64(binary.LittleEndian.Uint32(eocd[16:]))
	if count == 0xffff || dirSize == 0xffffffff || dirOffset == 0xffffffff {
		if at < zip64LocatorLen || binary.LittleEndian.Uint32(tail[at-zip64LocatorLen:]) != zip64LocatorSig {
			return 0, 0, 0, fmt.Errorf("%w: no zip64 end of central directory locator", ErrInvalidArchive)
		}
		recOffset := binary.LittleEndian.Uint64(tail[at-zip64LocatorLen+8:])
		if recOffset > uint64(size-zip64EOCDLen) { //nolint:gosec // size is at least zipEOCDLen here
			return 0, 0, 0, fmt.Errorf("%w: zip64 record outside the archive", ErrInvalidArchive)
		}
		rec := make([]byte, zip64EOCDLen)
		if _, err := r.ReadAt(rec, int64(recOffset)); err != nil && err != io.EOF { //nolint:gosec // checked against size above
			return 0, 0, 0, err
		}
		if binary.LittleEndian.Uint32(rec) != zip64EOCDSig {
			return 0, 0, 0, fmt.Errorf("%w: no zip64 end of central directory record", ErrInvalidArchive)
		}
		count = binary.LittleEndian.Uint64(rec[32:])
		dirSize = int64(binary.LittleEndian.Uint64(rec[40:]))   //nolint:gosec // checked below
		dirOffset = int64(binary.LittleEndian.Uint64(rec[48:])) //nolint:gosec // checked below
	}
	if dirSize < 0 || dirOffset < 0 || dirOffset > size || dirSize > size-dirOffset {
		return 0, 0, 0, fmt.Errorf("%w: central directory outside the archive", ErrInvalidArchive)
	}
	// Every entry takes at least a fixed-size header.
	if count > uint64(dirSize)/zipCentralLen {
		return 0, 0, 0, fmt.Errorf("%w: %d entries in a directory of %d bytes", ErrInvalidArchive, count, dirSize)
	}
	return count, dirSize, dirOffset, nil
}

// archivePaths is what `datasets` globs inside an extracted zip archive
// (xglob of "zip://**::archive"): every file and every directory, explicit
// or implied by a file's path, without a trailing slash, sorted.
func archivePaths(names []string) []string {
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.Trim(n, "/")
		for p := n; p != "" && p != "." && !seen[p]; p = path.Dir(p) {
			seen[p] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// inferArchives is infer_module_for_data_files_list_in_archives: count the
// extensions of everything in the first ten zip archives of a Split, and
// take the builder of the most common one, the first seen on a tie. When
// that extension has no builder, the Split has none.
func (r *resolver) inferArchives(files []string) (module, error) {
	counts := map[string]int{}
	var order []string
	archives := 0
	for _, f := range files {
		if !strings.HasSuffix(f, ".zip") {
			continue
		}
		if archives++; archives > archivesInference {
			break
		}
		list := r.members[f]
		if list == nil {
			return module{}, fmt.Errorf("%w: %s cannot be listed", ErrInvalidArchive, f)
		}
		var names []string
		err := list(func(name string) error {
			names = append(names, name)
			return nil
		})
		if err != nil {
			return module{}, fmt.Errorf("%s: %w", f, err)
		}
		for _, p := range archivePaths(names) {
			parts := strings.Split(path.Base(p), ".")
			for _, s := range parts[1:] {
				ext := "." + strings.ToLower(s)
				if counts[ext] == 0 {
					order = append(order, ext)
				}
				counts[ext]++
			}
		}
	}
	best := ""
	for _, ext := range order {
		if best == "" || counts[ext] > counts[best] {
			best = ext
		}
	}
	if m, ok := lookupModule(best); ok {
		return m, nil
	}
	return module{}, ErrEmptyDataset
}
