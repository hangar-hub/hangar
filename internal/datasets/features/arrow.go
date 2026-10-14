// Package features models dataset columns the way the Hugging Face
// `datasets` library does: Features (Value, ClassLabel, List, Image, Audio,
// nested structs and the rest) and the Arrow storage types behind them. It
// reads and writes the Features JSON that the Hub's Dataset Viewer returns
// and that Parquet exports carry in their `huggingface` metadata, converts
// between Features and Arrow types, and computes Arrow's in-memory size of
// values, which the Hub reports as `num_bytes`.
package features

import (
	"fmt"
	"strconv"
	"strings"
)

// TypeID names an Arrow storage type.
type TypeID int

// Arrow storage types that Hangar reads and writes.
const (
	Null TypeID = iota
	Bool
	Int8
	Int16
	Int32
	Int64
	Uint8
	Uint16
	Uint32
	Uint64
	Float16
	Float32
	Float64
	String
	LargeString
	Binary
	LargeBinary
	Timestamp
	Date32
	Date64
	Time32
	Time64
	Duration
	Decimal128
	List
	LargeList
	FixedSizeList
	Struct
)

// Arrow is an Arrow storage type.
type Arrow struct {
	ID TypeID
	// Unit is s, ms, us or ns for Timestamp, Time32, Time64 and Duration.
	Unit string
	// TZ is the time zone of a Timestamp, or "" for naive timestamps.
	TZ string
	// Precision and Scale describe a Decimal128.
	Precision, Scale int
	// Size is the length of a FixedSizeList.
	Size int
	// Elem is the element type of lists.
	Elem *Arrow
	// Fields are the children of a Struct, in order.
	Fields []ArrowField
}

// ArrowField is a named child of a Struct.
type ArrowField struct {
	Name string
	Type *Arrow
}

// IsInteger reports whether the type is a signed or unsigned integer.
func (a *Arrow) IsInteger() bool { return a.ID >= Int8 && a.ID <= Uint64 }

// IsFloat reports whether the type is a floating-point number.
func (a *Arrow) IsFloat() bool { return a.ID == Float16 || a.ID == Float32 || a.ID == Float64 }

// IsNumeric reports whether the type is an integer or a float.
func (a *Arrow) IsNumeric() bool { return a.IsInteger() || a.IsFloat() }

// IsString reports whether the type is a string.
func (a *Arrow) IsString() bool { return a.ID == String || a.ID == LargeString }

// IsBinary reports whether the type is a binary string.
func (a *Arrow) IsBinary() bool { return a.ID == Binary || a.ID == LargeBinary }

// IsList reports whether the type is any kind of list.
func (a *Arrow) IsList() bool { return a.ID == List || a.ID == LargeList || a.ID == FixedSizeList }

// IsTemporal reports whether values are times or dates.
func (a *Arrow) IsTemporal() bool {
	return a.ID == Timestamp || a.ID == Date32 || a.ID == Date64 || a.ID == Time32 || a.ID == Time64 || a.ID == Duration
}

// Field returns the index of a Struct child, or -1.
func (a *Arrow) Field(name string) int {
	for i, f := range a.Fields {
		if f.Name == name {
			return i
		}
	}
	return -1
}

// Equal reports whether two types are the same.
func (a *Arrow) Equal(b *Arrow) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.ID != b.ID || a.Unit != b.Unit || a.TZ != b.TZ || a.Precision != b.Precision || a.Scale != b.Scale || a.Size != b.Size {
		return false
	}
	if (a.Elem == nil) != (b.Elem == nil) || (a.Elem != nil && !a.Elem.Equal(b.Elem)) {
		return false
	}
	if len(a.Fields) != len(b.Fields) {
		return false
	}
	for i := range a.Fields {
		if a.Fields[i].Name != b.Fields[i].Name || !a.Fields[i].Type.Equal(b.Fields[i].Type) {
			return false
		}
	}
	return true
}

var simpleNames = map[TypeID]string{
	Null: "null", Bool: "bool", Int8: "int8", Int16: "int16", Int32: "int32", Int64: "int64",
	Uint8: "uint8", Uint16: "uint16", Uint32: "uint32", Uint64: "uint64",
	Float16: "float16", Float32: "float32", Float64: "float64",
	String: "string", LargeString: "large_string", Binary: "binary", LargeBinary: "large_binary",
	Date32: "date32", Date64: "date64",
}

// DType returns the `datasets` dtype string of a primitive type, such as
// "int64", "timestamp[s]" or "decimal128(10, 2)", as Value features name
// it. It returns "" for lists and structs.
func (a *Arrow) DType() string {
	if n, ok := simpleNames[a.ID]; ok {
		return n
	}
	switch a.ID {
	case Timestamp:
		if a.TZ != "" {
			return "timestamp[" + a.Unit + ", tz=" + a.TZ + "]"
		}
		return "timestamp[" + a.Unit + "]"
	case Time32:
		return "time32[" + a.Unit + "]"
	case Time64:
		return "time64[" + a.Unit + "]"
	case Duration:
		return "duration[" + a.Unit + "]"
	case Decimal128:
		return fmt.Sprintf("decimal128(%d, %d)", a.Precision, a.Scale)
	}
	return ""
}

// String returns a readable form of the type, as pyarrow prints it.
func (a *Arrow) String() string {
	if d := a.DType(); d != "" {
		return d
	}
	switch a.ID {
	case List:
		return "list<item: " + a.Elem.String() + ">"
	case LargeList:
		return "large_list<item: " + a.Elem.String() + ">"
	case FixedSizeList:
		return "fixed_size_list<item: " + a.Elem.String() + ">[" + strconv.Itoa(a.Size) + "]"
	case Struct:
		parts := make([]string, len(a.Fields))
		for i, f := range a.Fields {
			parts[i] = f.Name + ": " + f.Type.String()
		}
		return "struct<" + strings.Join(parts, ", ") + ">"
	}
	return "unknown"
}

// ParseDType parses a `datasets` Value dtype into an Arrow type. It accepts
// the pyarrow names and the aliases `datasets` accepts (float, double, int,
// halffloat, utf8, large_utf8, boolean).
func ParseDType(s string) (*Arrow, error) {
	switch s {
	case "float", "float32":
		return &Arrow{ID: Float32}, nil
	case "double", "float64":
		return &Arrow{ID: Float64}, nil
	case "halffloat", "float16":
		return &Arrow{ID: Float16}, nil
	case "int":
		return &Arrow{ID: Int64}, nil
	case "utf8":
		return &Arrow{ID: String}, nil
	case "large_utf8":
		return &Arrow{ID: LargeString}, nil
	case "boolean":
		return &Arrow{ID: Bool}, nil
	}
	for id, n := range simpleNames {
		if n == s {
			return &Arrow{ID: id}, nil
		}
	}
	unit := func(prefix string) (string, string, bool) {
		rest, ok := strings.CutPrefix(s, prefix+"[")
		if !ok || !strings.HasSuffix(rest, "]") {
			return "", "", false
		}
		rest = strings.TrimSuffix(rest, "]")
		u, tz, _ := strings.Cut(rest, ",")
		u = strings.TrimSpace(u)
		tz = strings.TrimSpace(tz)
		if tz != "" {
			var ok bool
			if tz, ok = strings.CutPrefix(tz, "tz="); !ok {
				return "", "", false
			}
		}
		switch u {
		case "s", "ms", "us", "ns":
			return u, tz, true
		}
		return "", "", false
	}
	if u, tz, ok := unit("timestamp"); ok {
		return &Arrow{ID: Timestamp, Unit: u, TZ: tz}, nil
	}
	if u, tz, ok := unit("time32"); ok && tz == "" && (u == "s" || u == "ms") {
		return &Arrow{ID: Time32, Unit: u}, nil
	}
	if u, tz, ok := unit("time64"); ok && tz == "" && (u == "us" || u == "ns") {
		return &Arrow{ID: Time64, Unit: u}, nil
	}
	if u, tz, ok := unit("duration"); ok && tz == "" {
		return &Arrow{ID: Duration, Unit: u}, nil
	}
	if rest, ok := strings.CutPrefix(s, "decimal128("); ok && strings.HasSuffix(rest, ")") {
		p, sc, ok := strings.Cut(strings.TrimSuffix(rest, ")"), ",")
		if ok {
			precision, err1 := strconv.Atoi(strings.TrimSpace(p))
			scale, err2 := strconv.Atoi(strings.TrimSpace(sc))
			if err1 == nil && err2 == nil && precision > 0 && precision <= 38 && scale >= 0 && scale <= precision {
				return &Arrow{ID: Decimal128, Precision: precision, Scale: scale}, nil
			}
		}
	}
	return nil, fmt.Errorf("unsupported dtype %q", s)
}

// Primitive returns a primitive Arrow type.
func Primitive(id TypeID) *Arrow { return &Arrow{ID: id} }

// ListOf returns a list type.
func ListOf(elem *Arrow) *Arrow { return &Arrow{ID: List, Elem: elem} }

// StructOf returns a struct type.
func StructOf(fields ...ArrowField) *Arrow { return &Arrow{ID: Struct, Fields: fields} }

// MediaStorage is the storage type of Image, Audio, Video and Pdf features.
func MediaStorage() *Arrow {
	return StructOf(ArrowField{Name: "bytes", Type: Primitive(Binary)}, ArrowField{Name: "path", Type: Primitive(String)})
}
