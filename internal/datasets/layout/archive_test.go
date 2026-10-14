package layout

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// zipOf writes a zip archive whose entries have the given names, in order.
func zipOf(t testing.TB, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, n := range names {
		_, err := w.Create(n)
		require.NoError(t, err)
	}
	require.NoError(t, w.Close())
	return buf.Bytes()
}

// zipFile is a File for a zip archive held in memory.
func zipFile(t testing.TB, p string, names ...string) File {
	b := zipOf(t, names...)
	return File{Path: p, Size: int64(len(b)), Entries: func(fn func(string) error) error {
		return ZipEntries(bytes.NewReader(b), int64(len(b)), fn)
	}}
}

func entriesOf(t *testing.T, b []byte) ([]string, error) {
	var out []string
	err := ZipEntries(bytes.NewReader(b), int64(len(b)), func(n string) error {
		out = append(out, n)
		return nil
	})
	return out, err
}

func TestZipEntries(t *testing.T) {
	names := []string{"b.csv", "a/", "a/c.jsonl", "__MACOSX/._b.csv", "é.png"}
	got, err := entriesOf(t, zipOf(t, names...))
	require.NoError(t, err)
	require.Equal(t, names, got)

	// An archive comment does not hide the directory.
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	require.NoError(t, w.SetComment("a comment"))
	_, err = w.Create("x.txt")
	require.NoError(t, err)
	require.NoError(t, w.Close())
	got, err = entriesOf(t, buf.Bytes())
	require.NoError(t, err)
	require.Equal(t, []string{"x.txt"}, got)

	got, err = entriesOf(t, zipOf(t))
	require.NoError(t, err)
	require.Empty(t, got)
}

// TestZipEntriesZip64 lists an archive of more than 65,535 entries, which
// needs the zip64 end of central directory record.
func TestZipEntriesZip64(t *testing.T) {
	names := make([]string, 70000)
	for i := range names {
		names[i] = fmt.Sprintf("%05d.txt", i)
	}
	b := zipOf(t, names...)
	require.Equal(t, uint16(0xffff), binary.LittleEndian.Uint16(b[len(b)-22+10:]), "the archive uses zip64")
	got, err := entriesOf(t, b)
	require.NoError(t, err)
	require.Equal(t, names, got)
}

// errReader fails every read, as a Blob store outage would.
type errReader struct{}

func (errReader) ReadAt([]byte, int64) (int, error) { return 0, errors.New("connection reset") }

func TestZipEntriesErrors(t *testing.T) {
	good := zipOf(t, "a.csv", "b.csv")
	eocd := len(good) - 22
	cases := map[string][]byte{
		"empty":     nil,
		"not a zip": []byte("this is not a zip archive at all"),
		"truncated": good[:len(good)-30],
	}
	bad := bytes.Clone(good)
	binary.LittleEndian.PutUint32(bad[eocd+16:], uint32(len(good))) // the directory starts past its end
	cases["directory outside"] = bad
	bad = bytes.Clone(good)
	binary.LittleEndian.PutUint16(bad[eocd+10:], 60000) // 60,000 entries in a directory of about 100 bytes
	cases["too many entries"] = bad
	bad = bytes.Clone(good)
	dir := int(binary.LittleEndian.Uint32(good[eocd+16:]))
	bad[dir] = 'X' // no central directory signature
	cases["bad signature"] = bad
	bad = bytes.Clone(good)
	binary.LittleEndian.PutUint16(bad[dir+28:], 0xffff) // a name longer than the directory
	cases["long name"] = bad
	for name, b := range cases {
		_, err := entriesOf(t, b)
		require.ErrorIs(t, err, ErrInvalidArchive, name)
	}
	// A read error is not about the archive.
	err := ZipEntries(errReader{}, 1000, func(string) error { return nil })
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrInvalidArchive)
	require.False(t, IsLayoutError(err))
	// fn's error stops the listing.
	stop := errors.New("stop")
	require.ErrorIs(t, ZipEntries(bytes.NewReader(good), int64(len(good)), func(string) error { return stop }), stop)
	require.ErrorIs(t, ZipEntries(bytes.NewReader(good), int64(len(good)), func(string) error { return io.EOF }), io.EOF)
}

func TestArchivePaths(t *testing.T) {
	require.Equal(t, []string{"a", "a/b.v1", "a/b.v1/c.csv", "d", "x.csv"},
		archivePaths([]string{"x.csv", "a/b.v1/c.csv", "d/", "a/"}))
}

// TestResolveInsideZipArchives checks the builder `datasets` 5.1.0 picks
// for Splits of zip archives by looking inside them; the compat suite
// checks the same layouts against the library itself.
func TestResolveInsideZipArchives(t *testing.T) {
	for _, c := range []struct {
		name  string
		files []File
		want  string
		err   error
	}{
		{"csv", []File{zipFile(t, "data/train.zip", "a.csv")}, "csv", nil},
		{"images and metadata", []File{zipFile(t, "train.zip", "0.jpg", "1.jpg", "metadata.csv")}, "imagefolder", nil},
		{"a tie goes to the first in path order", []File{zipFile(t, "train.zip", "z.jpg", "metadata.csv")}, "csv", nil},
		{"a tie, image first", []File{zipFile(t, "train.zip", "metadata.csv", "a.jpg")}, "imagefolder", nil},
		{"folders count", []File{zipFile(t, "train.zip", "x.v1/a.csv", "y.v1/b.jsonl", "c.jsonl")}, "json", nil},
		{"hidden entries count", []File{zipFile(t, "train.zip", "a.csv", "__MACOSX/._a.csv", "__MACOSX/._b.jsonl", "__MACOSX/._c.jsonl", ".h.jsonl", "b.jsonl")}, "json", nil},
		{"upper case", []File{zipFile(t, "train.zip", "A.JPG", "B.JPG")}, "imagefolder", nil},
		{"compressed entry", []File{zipFile(t, "train.zip", "a.csv.gz")}, "csv", nil},
		{"the most common has no builder", []File{zipFile(t, "train.zip", "a.xyz", "b.xyz", "c.csv")}, "", ErrEmptyDataset},
		{"empty archive", []File{zipFile(t, "train.zip")}, "", ErrEmptyDataset},
		{"corrupt archive", []File{{Path: "train.zip", Size: 4, Entries: func(func(string) error) error {
			return ZipEntries(bytes.NewReader([]byte("PK\x03\x04")), 4, func(string) error { return nil })
		}}}, "", ErrInvalidArchive},
		{"mixed formats", []File{zipFile(t, "train.zip", "a.csv"), zipFile(t, "test.zip", "b.jsonl")}, "", ErrMixedFormats},
	} {
		t.Run(c.name, func(t *testing.T) {
			res, err := Resolve(c.files, nil)
			if c.err != nil {
				require.ErrorIs(t, err, c.err)
				require.True(t, IsLayoutError(err))
				return
			}
			require.NoError(t, err)
			require.Equal(t, c.want, res.Configs[0].Builder)
			for _, files := range res.Configs[0].Files {
				for _, f := range files {
					require.Contains(t, f, ".zip")
				}
			}
		})
	}

	// Only the first ten archives of a Split are looked inside.
	var files []File
	for i := range 10 {
		files = append(files, zipFile(t, fmt.Sprintf("data/a%02d.zip", i), "notes.txt"))
	}
	files = append(files, zipFile(t, "data/a10.zip", "1.csv", "2.csv", "3.csv", "4.csv", "5.csv", "6.csv", "7.csv", "8.csv", "9.csv", "10.csv", "11.csv"))
	res, err := Resolve(files, nil)
	require.NoError(t, err)
	require.Equal(t, "text", res.Configs[0].Builder)

	// An archive is listed only when its extension is the Split's most
	// common: here the CSV files win and the archive is never opened.
	opened := false
	res, err = Resolve([]File{{Path: "data/train_1.csv", Size: 1}, {Path: "data/train_2.csv", Size: 1}, {Path: "data/train.zip", Size: 1,
		Entries: func(func(string) error) error { opened = true; return nil }}}, nil)
	require.NoError(t, err)
	require.Equal(t, "csv", res.Configs[0].Builder)
	require.False(t, opened)

	// A failure to read an archive is not a layout error.
	_, err = Resolve([]File{{Path: "train.zip", Size: 1000, Entries: func(fn func(string) error) error {
		return ZipEntries(errReader{}, 1000, fn)
	}}}, nil)
	require.ErrorContains(t, err, "connection reset")
	require.False(t, IsLayoutError(err))
}

// FuzzZipEntries checks that no input makes ZipEntries panic, and that it
// never lists more entries than the archive's bytes can hold.
func FuzzZipEntries(f *testing.F) {
	f.Add(zipOf(f, "a.csv", "b/", "b/c.jsonl"))
	f.Add(zipOf(f))
	f.Fuzz(func(t *testing.T, b []byte) {
		n := 0
		err := ZipEntries(bytes.NewReader(b), int64(len(b)), func(string) error {
			n++
			return nil
		})
		if err != nil {
			require.ErrorIs(t, err, ErrInvalidArchive)
			return
		}
		require.LessOrEqual(t, n, len(b)/zipCentralLen)
	})
}
