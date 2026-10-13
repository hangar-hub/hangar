package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ByteSize is a size in bytes. In YAML, environment variables and flags it
// accepts a plain integer or a number with a unit: B, KiB, MiB, GiB, TiB
// (powers of 1024) or KB, MB, GB, TB (powers of 1000).
type ByteSize int64

// Common sizes.
const (
	KiB ByteSize = 1 << 10
	MiB ByteSize = 1 << 20
	GiB ByteSize = 1 << 30
	TiB ByteSize = 1 << 40
)

var byteUnits = map[string]float64{
	"": 1, "b": 1,
	"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40,
	"kb": 1e3, "mb": 1e6, "gb": 1e9, "tb": 1e12,
	"k": 1 << 10, "m": 1 << 20, "g": 1 << 30, "t": 1 << 40,
}

// ParseByteSize parses a size such as "64MiB" or "1048576".
func ParseByteSize(s string) (ByteSize, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	num, unit := s[:i], strings.ToLower(strings.TrimSpace(s[i:]))
	mult, ok := byteUnits[unit]
	if !ok || num == "" {
		return 0, fmt.Errorf("invalid size %q: use a number with an optional unit such as KiB, MiB or GiB", s)
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: %w", s, err)
	}
	v := f * mult
	if v > math.MaxInt64 {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return ByteSize(v), nil
}

// String formats the size with the largest exact binary unit.
func (b ByteSize) String() string {
	for _, u := range []struct {
		n    ByteSize
		name string
	}{{TiB, "TiB"}, {GiB, "GiB"}, {MiB, "MiB"}, {KiB, "KiB"}} {
		if b != 0 && b%u.n == 0 {
			return strconv.FormatInt(int64(b/u.n), 10) + u.name
		}
	}
	return strconv.FormatInt(int64(b), 10)
}

// Int64 returns the size as an int64.
func (b ByteSize) Int64() int64 { return int64(b) }

// MarshalText implements encoding.TextMarshaler.
func (b ByteSize) MarshalText() ([]byte, error) { return []byte(b.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (b *ByteSize) UnmarshalText(text []byte) error {
	v, err := ParseByteSize(string(text))
	if err != nil {
		return err
	}
	*b = v
	return nil
}
