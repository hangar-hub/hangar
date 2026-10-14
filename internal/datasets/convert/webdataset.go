package convert

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jamesagarside/hangar/internal/datasets/features"
)

// webdatasetBatch is WebDataset.DEFAULT_WRITER_BATCH_SIZE.
const webdatasetBatch = 100

// maxTarMember caps one file inside a WebDataset shard.
const maxTarMember = 1 << 31

var (
	baseExtRe = regexp.MustCompile(`^((?:.*/|)[^.]+)[.]([^/]*)$`)

	wdsImageExt = []string{"blp", "bmp", "dib", "bufr", "cur", "pcx", "dcx", "dds", "ps", "eps", "fit", "fits", "fli", "flc", "ftc",
		"ftu", "gbr", "gif", "grib", "h5", "hdf", "png", "apng", "jp2", "j2k", "jpc", "jpf", "jpx", "j2c", "icns", "ico", "im", "iim",
		"tif", "tiff", "jfif", "jpe", "jpg", "jpeg", "mpg", "mpeg", "msp", "pcd", "pxr", "pbm", "pgm", "ppm", "pnm", "psd", "bw",
		"rgb", "rgba", "sgi", "ras", "tga", "icb", "vda", "vst", "webp", "wmf", "emf", "xbm", "xpm"}
	wdsAudioExt = []string{"aiff", "au", "avr", "caf", "flac", "htk", "svx", "mat4", "mat5", "mpc2k", "ogg", "paf", "pvf", "raw",
		"rf64", "sd2", "sds", "ircam", "voc", "w64", "wav", "nist", "wavex", "wve", "xi", "mp3", "opus"}
	wdsVideoExt  = []string{"mkv", "mp4", "avi", "mpeg", "mov"}
	wdsMeshExt   = []string{"glb", "ply", "stl"}
	wdsUndecoded = []string{"ten", "tb", "mp", "msg", "npy", "npz", "cbor", "pth"}
	wdsCompress  = []string{".gz", ".bz2", ".xz", ".zst", ".zstd"}
)

// wdsExample is one WebDataset sample: its fields in tar order.
type wdsExample struct {
	key    string
	url    string
	fields []string
	values []any
	// keysFirst is true for the last sample of a shard, which `datasets`
	// yields without moving __key__ and __url__ after the fields.
	keysFirst bool
}

func (x *wdsExample) get(name string) (any, bool) {
	for i, f := range x.fields {
		if f == name {
			return x.values[i], true
		}
	}
	return nil, false
}

// decodeWDS applies WebDataset.DECODERS to a field's bytes.
func decodeWDS(ext string, b []byte) (any, error) {
	switch ext {
	case "txt", "text", "transcript":
		return string(b), nil
	case "cls", "cls2", "index", "inx", "id":
		i, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("the %s field is not an integer", ext)
		}
		return i, nil
	case "json", "jsn":
		return decodeJSON(b)
	}
	if slices.Contains(wdsUndecoded, ext) {
		return nil, fmt.Errorf("%w: WebDataset %q fields", ErrUnsupported, ext)
	}
	return b, nil
}

// wdsSamples streams the samples of one tar shard.
func wdsSamples(r io.Reader, url string, fn func(*wdsExample) error) error {
	tr := tar.NewReader(r)
	var cur *wdsExample
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		m := baseExtRe.FindStringSubmatch(h.Name)
		if m == nil {
			continue
		}
		key, field := m[1], m[2]
		if cur != nil && cur.key != key {
			if err := fn(cur); err != nil {
				return err
			}
			cur = nil
		}
		if cur == nil {
			cur = &wdsExample{key: key, url: url}
		}
		raw, err := readAllLimit(tr, maxTarMember)
		if err != nil {
			return err
		}
		dataExt := strings.ToLower(field[strings.LastIndex(field, ".")+1:])
		if last := strings.ToLower(path.Ext(h.Name)); slices.Contains(wdsCompress, last) {
			rc, inner, err := decompress(h.Name, io.NopCloser(bytes.NewReader(raw)))
			if err != nil {
				return err
			}
			raw, err = readAllLimit(rc, maxTarMember)
			_ = rc.Close()
			if err != nil {
				return err
			}
			dataExt = strings.ToLower(strings.TrimPrefix(path.Ext(inner), "."))
		}
		v, err := decodeWDS(dataExt, raw)
		if err != nil {
			return fmt.Errorf("%s: %w", h.Name, err)
		}
		cur.fields = append(cur.fields, field)
		cur.values = append(cur.values, v)
	}
	if cur != nil {
		cur.keysFirst = true
		return fn(cur)
	}
	return nil
}

type webdatasetBuilder struct{}

func (webdatasetBuilder) name() string { return "webdataset" }

func mediaKind(field string) features.Kind {
	ext := strings.ToLower(field[strings.LastIndex(field, ".")+1:])
	switch {
	case slices.Contains(wdsImageExt, ext):
		return features.KindImage
	case slices.Contains(wdsAudioExt, ext):
		return features.KindAudio
	case slices.Contains(wdsVideoExt, ext):
		return features.KindVideo
	case slices.Contains(wdsMeshExt, ext):
		return "Mesh"
	}
	return ""
}

func (webdatasetBuilder) infer(e *env, splits []splitFiles) (features.Features, error) {
	if len(splits) == 0 || len(splits[len(splits)-1].files) == 0 {
		return nil, errors.New("no WebDataset shards")
	}
	// `datasets` reads the first shard of the last Split it listed.
	f := splits[len(splits)-1].files[0]
	rc, err := f.Open(e.ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var first []*wdsExample
	stop := errors.New("stop")
	err = wdsSamples(rc, e.repoURL+"/"+f.Path, func(x *wdsExample) error {
		first = append(first, x)
		if len(first) == 5 {
			return stop
		}
		return nil
	})
	if err != nil && !errors.Is(err, stop) {
		return nil, fmt.Errorf("%s: %w", f.Path, err)
	}
	if len(first) == 0 {
		return nil, fmt.Errorf("%s has no WebDataset samples", f.Path)
	}
	for _, x := range first[1:] {
		if !sameFields(x.fields, first[0].fields) {
			return nil, errors.New("the TAR archives of the dataset should be in WebDataset format, but the files in the archive don't share the same prefix or the same types")
		}
	}
	var fs features.Features
	for _, name := range first[0].fields {
		if k := mediaKind(name); k != "" {
			if k == "Mesh" {
				return nil, fmt.Errorf("%w: WebDataset mesh fields", ErrUnsupported)
			}
			fs = append(fs, features.Field{Name: name, Feature: &features.Feature{Kind: k, Length: -1}})
			continue
		}
		t := &itype{id: features.Null}
		for _, x := range first {
			v, _ := x.get(name)
			if b, ok := v.([]byte); ok {
				_ = b
				t = merge(t, &itype{id: features.Binary})
				continue
			}
			t = merge(t, inferJSON(v, false, 0))
		}
		if t.json {
			return nil, fmt.Errorf("the WebDataset field %q has values of different types", name)
		}
		fs = append(fs, features.Field{Name: name, Feature: t.toFeature()})
	}
	keyFields := features.Features{{Name: "__key__", Feature: features.Value("string")}, {Name: "__url__", Feature: features.Value("string")}}
	if first[0].keysFirst {
		return append(keyFields, fs...), nil
	}
	return append(fs, keyFields...), nil
}

func sameFields(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

func (webdatasetBuilder) read(e *env, split splitFiles, fs features.Features, emit emitFunc) error {
	st, err := fs.StorageSchema()
	if err != nil {
		return err
	}
	var batch [][]any
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := emit(batch)
		batch = nil
		return err
	}
	for _, f := range split.files {
		rc, err := f.Open(e.ctx)
		if err != nil {
			return err
		}
		err = wdsSamples(rc, e.repoURL+"/"+f.Path, func(x *wdsExample) error {
			row := make([]any, len(fs))
			for i, fl := range fs {
				var v any
				switch fl.Name {
				case "__key__":
					v = x.key
				case "__url__":
					v = x.url
				default:
					v, _ = x.get(fl.Name)
					if v != nil && fl.Feature.IsMedia() {
						b, ok := v.([]byte)
						if !ok {
							return fmt.Errorf("the %s field is not a file", fl.Name)
						}
						v = []any{b, path.Base(x.key + "." + fl.Name)}
					}
				}
				c, err := CastFeature(v, fl.Feature, st.Fields[i].Type)
				if err != nil {
					return fmt.Errorf("%s: column %q: %w", x.key, fl.Name, err)
				}
				row[i] = c
			}
			batch = append(batch, row)
			if len(batch) >= webdatasetBatch {
				return flush()
			}
			return nil
		})
		_ = rc.Close()
		if err != nil {
			return fmt.Errorf("%s: %w", f.Path, err)
		}
	}
	return flush()
}
