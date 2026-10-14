// Package stats computes the Dataset Viewer's column statistics (DATA-5)
// the way the Hub's dataset-viewer does (libcommon/statistics_utils.py):
// null counts and proportions, min, max, mean, median and standard
// deviation with histograms for numbers, string lengths, list lengths,
// image widths and audio durations, frequencies for class labels, booleans
// and low-cardinality strings, and datetime ranges.
package stats

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jamesagarside/hangar/internal/datasets"
)

// Constants from statistics_utils.py.
const (
	Decimals              = 5
	NumBins               = 10
	MaxProportionLabels   = 0.2
	MaxNumStringLabels    = 1000
	noLabelValue          = -1
	datetimeSampleSize    = 100
	maxColumnNameLenBytes = 1 << 16
)

// Column types, as the Hub names them.
const (
	TypeFloat       = "float"
	TypeInt         = "int"
	TypeBool        = "bool"
	TypeList        = "list"
	TypeClassLabel  = "class_label"
	TypeStringLabel = "string_label"
	TypeStringText  = "string_text"
	TypeAudio       = "audio"
	TypeImage       = "image"
	TypeDatetime    = "datetime"
)

// Object is a JSON object that keeps its key order.
type Object []KV

// KV is a key and value of an Object.
type KV struct {
	Key   string
	Value any
}

// MarshalJSON writes the keys in order.
func (o Object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, kv := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(kv.Key)
		b.Write(k)
		b.WriteByte(':')
		v, err := marshalValue(kv.Value)
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func marshalValue(v any) ([]byte, error) {
	switch x := v.(type) {
	case float64:
		return []byte(pyFloat(x)), nil
	case []float64:
		var b bytes.Buffer
		b.WriteByte('[')
		for i, f := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(pyFloat(f))
		}
		b.WriteByte(']')
		return b.Bytes(), nil
	}
	return json.Marshal(v)
}

// pyFloat writes a float as the Hub's responses do (datasets.FormatFloat).
func pyFloat(f float64) string { return datasets.FormatFloat(f) }

// Get returns a key's value.
func (o Object) Get(key string) (any, bool) {
	for _, kv := range o {
		if kv.Key == key {
			return kv.Value, true
		}
	}
	return nil, false
}

// Item is one column's statistics.
type Item struct {
	ColumnName string `json:"column_name"`
	ColumnType string `json:"column_type"`
	Statistics Object `json:"column_statistics"`
}

// round is np.round(x, 5): multiply, round half to even, divide.
func round(x float64) float64 {
	return math.RoundToEven(x*1e5) / 1e5
}

func proportion(count, n int) float64 {
	if count == 0 || n == 0 {
		return 0.0
	}
	return round(float64(count) / float64(n))
}

// arange is numpy.arange for floats: the length is ceil((stop-start)/step)
// and the values are start + i*delta, with delta recomputed from the
// first two values.
func arange(start, stop, step float64) []float64 {
	n := int(math.Ceil((stop - start) / step))
	if n <= 0 {
		return nil
	}
	delta := (start + step) - start
	out := make([]float64, n)
	for i := range out {
		out[i] = start + float64(i)*delta
	}
	return out
}

// ErrComputation means the Hub would fail to compute the statistics.
var ErrComputation = errors.New("statistics computation error")

// binEdges is generate_bins.
func binEdges(minV, maxV float64, isInt bool) []float64 {
	if minV == maxV {
		return []float64{minV, maxV}
	}
	var edges []float64
	if isInt {
		size := math.Ceil((maxV - minV + 1) / NumBins)
		// np.arange on integers.
		for v := minV; v < maxV; v += size {
			edges = append(edges, v)
		}
	} else {
		edges = arange(minV, maxV, (maxV-minV)/NumBins)
	}
	if len(edges) == 0 || edges[len(edges)-1] != maxV {
		edges = append(edges, maxV)
	}
	return edges
}

// histogram is compute_histogram: bins [e_i, e_i+1) with the first bin
// extended down by 1 and the last bin closed and extended up by 1, as the
// Hub's negated polars histogram counts them.
func histogram(values []float64, minV, maxV float64, isInt bool) Object {
	edges := binEdges(minV, maxV, isInt)
	var hist []int
	if len(edges) == 2 && edges[0] == edges[1] {
		hist = []int{len(values)}
	} else {
		hist = make([]int, len(edges)-1)
		k := len(edges) - 1
		for _, v := range values {
			i := sort.Search(k, func(i int) bool { return edges[i+1] > v }) // first bin whose upper edge > v
			if i >= k {
				i = k - 1
			}
			if i < 0 {
				i = 0
			}
			hist[i]++
		}
	}
	var be any
	if isInt {
		ints := make([]int64, len(edges))
		for i, e := range edges {
			ints[i] = int64(e)
		}
		be = ints
	} else {
		r := make([]float64, len(edges))
		for i, e := range edges {
			r[i] = round(e)
		}
		be = r
	}
	return Object{{"hist", hist}, {"bin_edges", be}}
}

func allNull(n int) Object {
	return Object{{"nan_count", n}, {"nan_proportion", 1.0}, {"min", nil}, {"max", nil}, {"mean", nil},
		{"median", nil}, {"std", nil}, {"histogram", nil}}
}

// moments returns min, max, mean, median and the sample standard
// deviation, as polars computes them; std is NaN for one value.
func moments(vals []float64) (minV, maxV, mean, median, std float64) {
	s := append([]float64(nil), vals...)
	sort.Float64s(s)
	minV, maxV = s[0], s[len(s)-1]
	// Kahan summation keeps the mean as exact as polars' pairwise sum.
	var sum, c float64
	for _, v := range s {
		y := v - c
		t := sum + y
		c = (t - sum) - y
		sum = t
	}
	mean = sum / float64(len(s))
	n := len(s)
	if n%2 == 1 {
		median = s[n/2]
	} else {
		median = (s[n/2-1] + s[n/2]) / 2
	}
	if n < 2 {
		return minV, maxV, mean, median, math.NaN()
	}
	var ss float64
	for _, v := range s {
		d := v - mean
		ss += d * d
	}
	std = math.Sqrt(ss / float64(n-1))
	return
}

func nullable(f float64) any {
	if math.IsNaN(f) {
		return nil
	}
	return f
}

// Numbers computes int or float statistics. values holds the non-null
// values; nulls is the number of nulls.
func Numbers(values []float64, nulls int, isInt bool) Object {
	n := len(values) + nulls
	if len(values) == 0 {
		return allNull(n)
	}
	minV, maxV, mean, median, std := moments(values)
	minV, maxV, mean, median = round(minV), round(maxV), round(mean), round(median)
	if !math.IsNaN(std) {
		std = round(std)
	}
	hist := histogram(values, minV, maxV, isInt)
	var minOut, maxOut any = minV, maxV
	if isInt {
		minOut, maxOut = int64(minV), int64(maxV)
	}
	return Object{{"nan_count", nulls}, {"nan_proportion", proportion(nulls, n)}, {"min", minOut}, {"max", maxOut},
		{"mean", mean}, {"median", median}, {"std", nullable(std)}, {"histogram", hist}}
}

// Strings computes statistics of a string column: frequencies for
// low-cardinality columns, datetime statistics when every sampled value
// is a date in one format, otherwise statistics of the lengths. It
// returns the column type.
func Strings(values []*string) (string, Object) {
	n := len(values)
	nulls := 0
	counts := map[string]int{}
	var order []string
	for _, v := range values {
		if v == nil {
			nulls++
			continue
		}
		if _, ok := counts[*v]; !ok {
			order = append(order, *v)
		}
		counts[*v]++
	}
	unique := len(counts)
	if nulls > 0 {
		unique++ // polars counts null as a value
	}
	if t, ok := stringDatetimes(values); ok {
		return TypeDatetime, Datetimes(t.values, nulls, t.tz)
	}
	if isClass(unique, n) {
		return TypeStringLabel, Object{{"nan_count", nulls}, {"nan_proportion", proportion(nulls, n)}, {"no_label_count", 0},
			{"no_label_proportion", 0.0}, {"n_unique", len(counts)}, {"frequencies", frequencies(counts, order)}}
	}
	lengths := make([]float64, 0, n-nulls)
	for _, v := range values {
		if v != nil {
			lengths = append(lengths, float64(len([]rune(*v))))
		}
	}
	return TypeStringText, Numbers(lengths, nulls, true)
}

func isClass(unique, n int) bool {
	if n == 0 {
		return true
	}
	return (float64(unique)/float64(n) <= MaxProportionLabels && unique <= MaxNumStringLabels) || unique <= NumBins
}

// frequencies orders counts by count, then by first appearance.
func frequencies(counts map[string]int, order []string) Object {
	keys := append([]string(nil), order...)
	pos := map[string]int{}
	for i, k := range keys {
		pos[k] = i
	}
	sort.SliceStable(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return pos[keys[i]] < pos[keys[j]]
	})
	out := make(Object, len(keys))
	for i, k := range keys {
		out[i] = KV{k, counts[k]}
	}
	return out
}

// Bools computes statistics of a bool column.
func Bools(values []*bool) Object {
	n := len(values)
	nulls, t, f := 0, 0, 0
	for _, v := range values {
		switch {
		case v == nil:
			nulls++
		case *v:
			t++
		default:
			f++
		}
	}
	freq := Object{}
	if f > 0 {
		freq = append(freq, KV{"False", f})
	}
	if t > 0 {
		freq = append(freq, KV{"True", t})
	}
	return Object{{"nan_count", nulls}, {"nan_proportion", proportion(nulls, n)}, {"frequencies", freq}}
}

// ClassLabels computes statistics of a ClassLabel column; nil values are nulls.
func ClassLabels(values []*int64, names []string) (Object, error) {
	n := len(values)
	nulls, noLabel := 0, 0
	counts := map[int64]int{}
	for _, v := range values {
		switch {
		case v == nil:
			nulls++
		case *v == noLabelValue:
			noLabel++
		default:
			counts[*v]++
		}
	}
	for id := range counts {
		if id < 0 || id >= int64(len(names)) {
			return nil, fmt.Errorf("%w: label %d is outside the %d class names", ErrComputation, id, len(names))
		}
	}
	freq := make(Object, len(names))
	for i, name := range names {
		freq[i] = KV{name, counts[int64(i)]}
	}
	return Object{{"nan_count", nulls}, {"nan_proportion", proportion(nulls, n)}, {"no_label_count", noLabel},
		{"no_label_proportion", proportion(noLabel, n)}, {"n_unique", len(names)}, {"frequencies", freq}}, nil
}

// Lengths computes statistics of list lengths; nil means a null list.
func Lengths(lengths []*int, isMediaDurations bool) Object {
	n := len(lengths)
	var vals []float64
	for _, l := range lengths {
		if l != nil {
			vals = append(vals, float64(*l))
		}
	}
	if len(vals) == 0 {
		return allNull(n)
	}
	return Numbers(vals, n-len(vals), !isMediaDurations)
}

// Measures computes statistics of image widths (ints) or audio durations
// (floats); NaN marks a missing value.
func Measures(values []float64, isInt bool) Object {
	var vals []float64
	for _, v := range values {
		if !math.IsNaN(v) {
			vals = append(vals, v)
		}
	}
	return Numbers(vals, len(values)-len(vals), isInt)
}

// datetimeFormats is identify_datetime_format's list of strptime formats,
// tried in order.
var datetimeFormats = []string{
	"%Y-%m-%dT%H:%M:%S%Z",
	"%Y-%m-%dT%H:%M:%S%z",
	"%Y-%m-%dT%H:%M:%S",
	"%Y-%m-%dT%H:%M:%S.%f",
	"%Y-%m-%d %H:%M:%S%Z",
	"%Y-%m-%d %H:%M:%S%z",
	"%Y-%m-%d %H:%M:%S",
	"%Y-%m-%d %H:%M",
	"%Y-%m-%d",
	"%d-%m-%Y %H:%M:%S%Z",
	"%d-%m-%Y %H:%M:%S%z",
	"%d-%m-%Y %H:%M:%S",
	"%d-%m-%Y %H:%M",
	"%d-%m-%Y",
	"%m-%d-%Y %H:%M:%S%Z",
	"%m-%d-%Y %H:%M:%S%z",
	"%m-%d-%Y %H:%M:%S",
	"%m-%d-%Y %H:%M",
	"%m-%d-%Y",
	"%Y/%m/%d %H:%M:%S%Z",
	"%Y/%m/%d %H:%M:%S%z",
	"%Y/%m/%d %H:%M:%S",
	"%Y/%m/%d %H:%M",
	"%Y/%m/%d",
	"%d/%m/%Y %H:%M:%S%Z",
	"%d/%m/%Y %H:%M:%S%z",
	"%d/%m/%Y %H:%M:%S",
	"%d/%m/%Y %H:%M",
	"%d/%m/%Y",
	"%m/%d/%Y %H:%M:%S%Z",
	"%m/%d/%Y %H:%M:%S%z",
	"%m/%d/%Y %H:%M:%S",
	"%m/%d/%Y %H:%M",
	"%m/%d/%Y",
	"%B %d, %Y",
	"%d %B %Y",
	"%m-%Y",
	"%Y-%m",
	"%m/%Y",
	"%Y/%m",
	"%Y",
}

// strptimeLayouts translates a strptime format to Go layouts. Python
// accepts one or two digits for days, months and times, and %z accepts
// "Z" and offsets with or without a colon.
func strptimeLayouts(f string) (layouts []string, zone bool, fraction bool) {
	r := strings.NewReplacer("%Y", "2006", "%m", "1", "%d", "2", "%H", "15", "%M", "4", "%S", "5", "%B", "January", "%f", "999999")
	base := r.Replace(f)
	switch {
	case strings.HasSuffix(base, "%Z"):
		b := strings.TrimSuffix(base, "%Z")
		return []string{b + "MST"}, true, false
	case strings.HasSuffix(base, "%z"):
		b := strings.TrimSuffix(base, "%z")
		return []string{b + "Z0700", b + "Z07:00"}, true, false
	}
	return []string{base}, false, strings.Contains(f, "%f")
}

// identifyFormat returns the index of the first format a value matches.
func identifyFormat(s string) int {
	for i, f := range datetimeFormats {
		if _, ok := parseFormat(f, s); ok {
			return i
		}
	}
	return -1
}

func parseFormat(f, s string) (time.Time, bool) {
	layouts, _, fraction := strptimeLayouts(f)
	for _, l := range layouts {
		t, err := time.Parse(l, s)
		if err != nil {
			continue
		}
		// Go accepts a fraction after seconds even when the layout has
		// none; strptime does not.
		if !fraction && strings.Contains(f, "%S") && strings.Count(s, ".") > strings.Count(f, ".") {
			continue
		}
		return t, true
	}
	return time.Time{}, false
}

type parsedDatetimes struct {
	values []time.Time
	tz     *time.Location
}

// stringDatetimes parses a string column as datetimes when its first
// sampled values share one known format and every value parses with it.
func stringDatetimes(values []*string) (parsedDatetimes, bool) {
	format := -1
	seen := 0
	for _, v := range values {
		if v == nil {
			continue
		}
		f := identifyFormat(*v)
		if f < 0 || (format >= 0 && f != format) {
			return parsedDatetimes{}, false
		}
		format = f
		seen++
		if seen == datetimeSampleSize {
			break
		}
	}
	if format < 0 {
		return parsedDatetimes{}, false
	}
	out := parsedDatetimes{}
	for _, v := range values {
		if v == nil {
			continue
		}
		t, ok := parseFormat(datetimeFormats[format], *v)
		if !ok {
			return parsedDatetimes{}, false
		}
		if _, zone, _ := strptimeLayouts(datetimeFormats[format]); out.tz == nil && zone {
			out.tz = t.Location()
		}
		out.values = append(out.values, t)
	}
	return out, true
}

// datetimeString is libcommon.utils.datetime_to_string.
func datetimeString(t time.Time, aware bool) string {
	s := t.Format("2006-01-02 15:04:05")
	if aware {
		_, off := t.Zone()
		if off != 0 {
			sign := "+"
			if off < 0 {
				sign, off = "-", -off
			}
			s += fmt.Sprintf("%s%02d%02d", sign, off/3600, (off%3600)/60)
		}
	}
	return s
}

// pyTimedelta formats seconds as Python's str(timedelta).
func pyTimedelta(seconds float64) string {
	us := int64(math.Round(seconds * 1e6))
	neg := us < 0
	days := us / 86_400_000_000
	rem := us % 86_400_000_000
	if neg && rem != 0 {
		days--
		rem += 86_400_000_000
	}
	secs := rem / 1_000_000
	micros := rem % 1_000_000
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

// Datetimes computes datetime statistics from the non-null values; tz is
// the zone of time-zone-aware values, or nil.
func Datetimes(values []time.Time, nulls int, tz *time.Location) Object {
	n := len(values) + nulls
	if len(values) == 0 {
		return Object{{"nan_count", n}, {"nan_proportion", 1.0}, {"min", nil}, {"max", nil}, {"mean", nil},
			{"median", nil}, {"std", nil}, {"histogram", nil}}
	}
	minT := values[0]
	for _, t := range values[1:] {
		if t.Before(minT) {
			minT = t
		}
	}
	// polars' dt.total_seconds truncates to whole seconds.
	deltas := make([]float64, len(values))
	for i, t := range values {
		deltas[i] = float64(int64(t.Sub(minT) / time.Second))
	}
	st := Numbers(deltas, 0, true)
	aware := tz != nil
	base := minT
	if aware {
		base = minT.In(tz)
	}
	shift := func(sec float64) string {
		return datetimeString(base.Add(time.Duration(math.Round(sec*1e6))*time.Microsecond), aware)
	}
	get := func(k string) float64 {
		v, _ := st.Get(k)
		switch x := v.(type) {
		case int64:
			return float64(x)
		case float64:
			return x
		}
		return 0
	}
	h, _ := st.Get("histogram")
	hist := h.(Object)
	hv, _ := hist.Get("hist")
	ev, _ := hist.Get("bin_edges")
	var edges []string
	for _, e := range ev.([]int64) {
		edges = append(edges, shift(float64(e)))
	}
	stdV, _ := st.Get("std")
	var std any
	if f, ok := stdV.(float64); ok {
		std = pyTimedelta(f)
	}
	return Object{{"nan_count", nulls}, {"nan_proportion", proportion(nulls, n)}, {"min", datetimeString(base, aware)},
		{"max", shift(get("max"))}, {"mean", shift(get("mean"))}, {"median", shift(get("median"))}, {"std", std},
		{"histogram", Object{{"hist", hv}, {"bin_edges", edges}}}}
}
