package features

import "time"

// Values are held in Go as follows, by Arrow storage type:
//
//   - null: nil, for any type
//   - Bool: bool
//   - signed integers and Uint8 to Uint32: int64; Uint64: uint64
//   - floats: float64 (Float32 and Float16 values widened exactly)
//   - String, LargeString and Decimal128: string
//   - Binary, LargeBinary: []byte
//   - Timestamp, Date32, Date64: time.Time in UTC; the type's TZ says how to
//     show it
//   - Time32, Time64, Duration: int64, a count of the type's Unit
//   - lists: []any
//   - Struct: []any, one value per field in field order

// width returns the byte width of fixed-width types, or 0.
func width(id TypeID) int64 {
	switch id {
	case Int8, Uint8:
		return 1
	case Int16, Uint16, Float16:
		return 2
	case Int32, Uint32, Float32, Date32, Time32:
		return 4
	case Int64, Uint64, Float64, Timestamp, Date64, Time64, Duration:
		return 8
	case Decimal128:
		return 16
	}
	return 0
}

func bitmap(n int64) int64 { return (n + 7) / 8 }

// fill stands for the non-null empty value Arrow stores under a null struct.
type fill struct{}

// NBytes returns the size pyarrow reports as `Array.nbytes` for values of
// type a held in one array: the validity bitmap when any value is null,
// plus the data buffers. pyarrow counts list and string offsets once per
// value. `datasets` sums this over the tables it writes to get a Split's
// `num_bytes`.
func NBytes(a *Arrow, values []any) int64 { return nbytes(a, values, false) }

// NBytesNullChildren is NBytes for arrays where the children of a null
// struct are null rather than filled, as pyarrow's JSON reader builds
// them, so the children count their own validity bitmaps.
func NBytesNullChildren(a *Arrow, values []any) int64 { return nbytes(a, values, true) }

func nbytes(a *Arrow, values []any, nullChildren bool) int64 {
	n := int64(len(values))
	var total int64
	hasNull := false
	for _, v := range values {
		if v == nil {
			hasNull = true
			break
		}
	}
	if a.ID == Null {
		return 0
	}
	if hasNull && a.ID != Struct {
		// pyarrow does not count a struct's own validity bitmap.
		total += bitmap(n)
	}
	switch a.ID {
	case Bool:
		return total + bitmap(n)
	case String, Binary, LargeString, LargeBinary:
		off := int64(4)
		if a.ID == LargeString || a.ID == LargeBinary {
			off = 8
		}
		total += off * n
		for _, v := range values {
			switch x := v.(type) {
			case string:
				total += int64(len(x))
			case []byte:
				total += int64(len(x))
			}
		}
		return total
	case List, LargeList, FixedSizeList:
		switch a.ID {
		case List:
			total += 4 * n
		case LargeList:
			total += 8 * n
		}
		var child []any
		for _, v := range values {
			if l, ok := v.([]any); ok {
				child = append(child, l...)
			} else if a.ID == FixedSizeList {
				for range a.Size {
					child = append(child, nil)
				}
			}
		}
		return total + nbytes(a.Elem, child, nullChildren)
	case Struct:
		for i, f := range a.Fields {
			col := make([]any, len(values))
			for j, v := range values {
				if s, ok := v.([]any); ok && i < len(s) {
					col[j] = s[i]
				} else if (v == nil && !nullChildren) || v == (fill{}) {
					// Arrow fills the children of a null struct built from
					// Python objects with non-null empty values.
					col[j] = fill{}
				}
			}
			total += nbytes(f.Type, col, nullChildren)
		}
		return total
	}
	return total + width(a.ID)*n
}

// DaysToTime converts days since the Unix epoch to a time.
func DaysToTime(days int64) time.Time { return time.Unix(days*86400, 0).UTC() }

// TimeToDays converts a time to whole days since the Unix epoch.
func TimeToDays(t time.Time) int64 {
	s := t.Unix()
	d := s / 86400
	if s%86400 < 0 {
		d--
	}
	return d
}

// UnitsPerSecond returns how many of unit fit in one second.
func UnitsPerSecond(unit string) int64 {
	switch unit {
	case "ms":
		return 1_000
	case "us":
		return 1_000_000
	case "ns":
		return 1_000_000_000
	}
	return 1
}

// TimeFromUnits converts a count of unit since the epoch to a time.
func TimeFromUnits(v int64, unit string) time.Time {
	per := UnitsPerSecond(unit)
	sec, rem := v/per, v%per
	if rem < 0 {
		sec--
		rem += per
	}
	return time.Unix(sec, rem*(1_000_000_000/per)).UTC()
}

// TimeToUnits converts a time to a count of unit since the epoch.
func TimeToUnits(t time.Time, unit string) int64 {
	per := UnitsPerSecond(unit)
	return t.Unix()*per + int64(t.Nanosecond())/(1_000_000_000/per)
}
