package viewer

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/adler32"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jamesagarside/hangar/internal/datasets"
	"github.com/jamesagarside/hangar/internal/datasets/features"
	"github.com/jamesagarside/hangar/internal/datasets/media"
)

// pyFloat is a float that marshals as orjson writes floats: integral
// values keep ".0", and NaN and infinities are null.
type pyFloat float64

func (f pyFloat) MarshalJSON() ([]byte, error) {
	x := float64(f)
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return []byte("null"), nil
	}
	if x == math.Trunc(x) && math.Abs(x) < 1e16 {
		return []byte(strconv.FormatFloat(x, 'f', 1, 64)), nil
	}
	s := strconv.FormatFloat(x, 'g', -1, 64)
	if mant, exp, ok := strings.Cut(s, "e"); ok {
		exp = strings.TrimPrefix(exp, "+")
		neg := strings.HasPrefix(exp, "-")
		exp = strings.TrimLeft(strings.TrimPrefix(exp, "-"), "0")
		if neg {
			exp = "-" + exp
		}
		s = mant + "e" + exp
	}
	return []byte(s), nil
}

// rawJSON is a decoded Json feature cell, written back verbatim.
type rawJSON []byte

func (r rawJSON) MarshalJSON() ([]byte, error) { return r, nil }

// object is an ordered JSON object.
type object []datasets.Cell

func (o object) MarshalJSON() ([]byte, error) { return datasets.Cells(o).MarshalJSON() }

// mediaRef locates a media cell for an asset URL.
type mediaRef struct {
	rowIdx int64
	column string
	path   []any
	kind   features.Kind
	bytes  []byte
	name   string
}

// cellEncoder turns stored values into the JSON values the Hub's viewer
// returns (libcommon/viewer_utils/features.py get_cell_value).
type cellEncoder struct {
	// assetURL returns the signed URL of a media cell.
	assetURL func(m mediaRef, ext string) string
}

// appendHashSuffix is append_hash_suffix: the JSON path of a nested media
// cell, hashed into the file name.
func appendHashSuffix(name string, path []any) string {
	if len(path) == 0 {
		return name
	}
	b, _ := json.Marshal(path)
	// Python's json.dumps puts a space after commas.
	s := strings.ReplaceAll(string(b), ",", ", ")
	return fmt.Sprintf("%s-%x", name, adler32.Checksum([]byte(s)))
}

func (e *cellEncoder) encode(v any, f *features.Feature, a *features.Arrow, rowIdx int64, column string, path []any) any {
	if v == nil {
		return nil
	}
	switch f.Kind {
	case features.KindImage, features.KindAudio, features.KindVideo, features.KindPdf:
		m, ok := v.([]any)
		if !ok || len(m) < 2 {
			return nil
		}
		b, _ := m[0].([]byte)
		name, _ := m[1].(string)
		if b == nil {
			return nil
		}
		ref := mediaRef{rowIdx: rowIdx, column: column, path: path, kind: f.Kind, bytes: b, name: name}
		switch f.Kind {
		case features.KindImage:
			w, h, mime, err := media.ImageSize(b)
			if err != nil {
				return nil
			}
			ext := "." + strings.TrimPrefix(mime, "image/")
			if ext == ".jpeg" {
				ext = ".jpg"
			}
			return object{{Name: "src", Value: e.assetURL(ref, ext)}, {Name: "height", Value: h}, {Name: "width", Value: w}}
		case features.KindAudio:
			mime, ext := media.AudioMIME(name, b)
			if ext == "" {
				ext = ".wav"
			}
			return []any{object{{Name: "src", Value: e.assetURL(ref, ext)}, {Name: "type", Value: mime}}}
		default:
			ext := strings.ToLower(pathExt(name))
			return object{{Name: "src", Value: e.assetURL(ref, ext)}}
		}
	case features.KindJSON:
		s, ok := v.(string)
		if ok && json.Valid([]byte(s)) {
			return rawJSON(s)
		}
		return v
	case features.KindStruct, features.KindTranslation, features.KindTranslationVariableLanguages:
		vals, ok := v.([]any)
		if !ok || a.ID != features.Struct {
			return nil
		}
		out := make(object, 0, len(a.Fields))
		for i, fl := range a.Fields {
			sub := features.FromArrow(fl.Type)
			if f.Kind == features.KindStruct && i < len(f.Fields) {
				sub = f.Fields[i].Feature
			}
			out = append(out, datasets.Cell{Name: fl.Name, Value: e.encode(vals[i], sub, fl.Type, rowIdx, column, appendPath(path, fl.Name))})
		}
		return out
	case features.KindList, features.KindLargeList, features.KindLegacyList:
		vals, ok := v.([]any)
		if !ok || a.Elem == nil {
			return nil
		}
		out := make([]any, len(vals))
		for i, x := range vals {
			out[i] = e.encode(x, f.Elem, a.Elem, rowIdx, column, appendPath(path, i))
		}
		return out
	}
	return scalar(v, a)
}

func appendPath(path []any, k any) []any {
	out := make([]any, len(path), len(path)+1)
	copy(out, path)
	return append(out, k)
}

func pathExt(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 && !strings.Contains(name[i:], "/") {
		return name[i:]
	}
	return ""
}

// scalar encodes a primitive value as orjson would after pyarrow's
// to_pylist.
func scalar(v any, a *features.Arrow) any {
	switch x := v.(type) {
	case float64:
		return pyFloat(x)
	case []byte:
		return base64.StdEncoding.EncodeToString(x)
	case time.Time:
		switch a.ID {
		case features.Date32, features.Date64:
			return x.Format("2006-01-02")
		}
		s := x.Format("2006-01-02T15:04:05")
		if us := x.Nanosecond() / 1000; us != 0 {
			s += fmt.Sprintf(".%06d", us)
		}
		if a.TZ != "" {
			s += "Z"
		}
		return s
	case int64:
		switch a.ID {
		case features.Time32, features.Time64:
			per := features.UnitsPerSecond(a.Unit)
			sec, rem := x/per, x%per
			s := fmt.Sprintf("%02d:%02d:%02d", sec/3600, (sec%3600)/60, sec%60)
			if rem != 0 {
				s += fmt.Sprintf(".%06d", rem*1_000_000/per)
			}
			return s
		case features.Duration:
			per := features.UnitsPerSecond(a.Unit)
			return pyTimedelta(x, per)
		}
	}
	return v
}

// pyTimedelta is str(datetime.timedelta), which orjson falls back to.
func pyTimedelta(v, per int64) string {
	us := v * (1_000_000 / per)
	if per > 1_000_000 {
		us = v / (per / 1_000_000)
	}
	days := us / 86_400_000_000
	rem := us % 86_400_000_000
	if rem < 0 {
		days--
		rem += 86_400_000_000
	}
	secs, micros := rem/1_000_000, rem%1_000_000
	s := fmt.Sprintf("%d:%02d:%02d", secs/3600, (secs%3600)/60, secs%60)
	if micros != 0 {
		s += fmt.Sprintf(".%06d", micros)
	}
	if days != 0 {
		unit := "days"
		if days == 1 || days == -1 {
			unit = "day"
		}
		s = fmt.Sprintf("%d %s, %s", days, unit, s)
	}
	return s
}

// jsonSize returns the size of a value's JSON serialisation.
func jsonSize(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// truncateCell replaces a cell by its JSON serialisation cut to max
// bytes, at a character boundary, as serialize_and_truncate does. It
// reports false when the cell is already small enough.
func truncateCell(v any, maxBytes int) (string, bool) {
	b, err := json.Marshal(v)
	if err != nil || len(b) <= maxBytes {
		return "", false
	}
	cut := b[:maxBytes]
	for len(cut) > 0 && !utf8.Valid(cut) {
		cut = cut[:len(cut)-1]
	}
	return string(bytes.Clone(cut)), true
}
