package stats

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jamesagarside/hangar/internal/datasets/features"
	"github.com/jamesagarside/hangar/internal/datasets/media"
)

// kind returns how a column is summarised, or "" when the Hub skips it.
func kind(f *features.Feature) string {
	switch f.Kind {
	case features.KindList, features.KindLargeList, features.KindLegacyList:
		if f.Kind == features.KindList && f.Length >= 0 {
			// Fixed-length lists are stored as lists too.
			return TypeList
		}
		return TypeList
	case features.KindClassLabel:
		return TypeClassLabel
	case features.KindAudio:
		return TypeAudio
	case features.KindImage:
		return TypeImage
	case features.KindValue:
		switch {
		case strings.HasPrefix(f.Dtype, "int") || strings.HasPrefix(f.Dtype, "uint"):
			return TypeInt
		case strings.HasPrefix(f.Dtype, "float"):
			return TypeFloat
		case f.Dtype == "string" || f.Dtype == "large_string":
			return "string"
		case f.Dtype == "bool":
			return TypeBool
		case strings.HasPrefix(f.Dtype, "timestamp"):
			return TypeDatetime
		}
	}
	return ""
}

// Supported reports whether a column gets statistics.
func Supported(f *features.Feature) bool { return kind(f) != "" }

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int64:
		return float64(x), true
	case uint64:
		return float64(x), true
	case float64:
		return x, true
	}
	return 0, false
}

// Column computes the statistics of one column from its storage values.
func Column(name string, f *features.Feature, values []any) (*Item, error) {
	if len(name) > maxColumnNameLenBytes {
		return nil, fmt.Errorf("%w: column name too long", ErrComputation)
	}
	switch kind(f) {
	case TypeInt, TypeFloat:
		isInt := kind(f) == TypeInt
		vals := make([]float64, 0, len(values))
		nulls := 0
		for _, v := range values {
			x, ok := toFloat(v)
			if !ok || (!isInt && math.IsNaN(x)) {
				nulls++
				continue
			}
			vals = append(vals, x)
		}
		return &Item{name, kind(f), Numbers(vals, nulls, isInt)}, nil
	case "string":
		s := make([]*string, len(values))
		for i, v := range values {
			if x, ok := v.(string); ok {
				s[i] = &x
			}
		}
		t, st := Strings(s)
		return &Item{name, t, st}, nil
	case TypeBool:
		b := make([]*bool, len(values))
		for i, v := range values {
			if x, ok := v.(bool); ok {
				b[i] = &x
			}
		}
		return &Item{name, TypeBool, Bools(b)}, nil
	case TypeClassLabel:
		ids := make([]*int64, len(values))
		for i, v := range values {
			if x, ok := v.(int64); ok {
				ids[i] = &x
			}
		}
		st, err := ClassLabels(ids, f.Names)
		if err != nil {
			return nil, err
		}
		return &Item{name, TypeClassLabel, st}, nil
	case TypeList:
		l := make([]*int, len(values))
		for i, v := range values {
			if x, ok := v.([]any); ok {
				n := len(x)
				l[i] = &n
			}
		}
		return &Item{name, TypeList, Lengths(l, false)}, nil
	case TypeDatetime:
		var ts []time.Time
		nulls := 0
		for _, v := range values {
			if t, ok := v.(time.Time); ok {
				ts = append(ts, t)
			} else {
				nulls++
			}
		}
		return &Item{name, TypeDatetime, Datetimes(ts, nulls, nil)}, nil
	case TypeImage, TypeAudio:
		isImage := kind(f) == TypeImage
		out := make([]float64, len(values))
		for i, v := range values {
			out[i] = math.NaN()
			m, ok := v.([]any)
			if !ok || len(m) == 0 {
				continue
			}
			b, ok := m[0].([]byte)
			if !ok {
				continue
			}
			if isImage {
				w, _, _, err := media.ImageSize(b)
				if err != nil {
					return nil, fmt.Errorf("%w: image in row %d: %w", ErrComputation, i, err)
				}
				out[i] = float64(w)
			} else {
				d, err := media.AudioDuration(b)
				if err != nil {
					return nil, fmt.Errorf("%w: audio in row %d: %w", ErrComputation, i, err)
				}
				out[i] = d
			}
		}
		return &Item{name, kind(f), Measures(out, isImage)}, nil
	}
	return nil, nil
}

// Sort orders items by column name, as the Hub returns them.
func Sort(items []Item) {
	sort.Slice(items, func(i, j int) bool { return items[i].ColumnName < items[j].ColumnName })
}
