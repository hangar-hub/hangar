package features_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/hangar-hub/hangar/internal/datasets/features"
)

func TestFeatureJSONRoundTrip(t *testing.T) {
	cases := []string{
		`{"dtype":"string","_type":"Value"}`,
		`{"names":["neg","pos"],"_type":"ClassLabel"}`,
		`{"feature":{"dtype":"int64","_type":"Value"},"_type":"List"}`,
		`{"feature":{"dtype":"int64","_type":"Value"},"length":3,"_type":"List"}`,
		`{"feature":{"dtype":"int64","_type":"Value"},"_type":"LargeList"}`,
		`{"_type":"Image"}`,
		`{"decode":false,"_type":"Image"}`,
		`{"_type":"Audio"}`,
		`{"sampling_rate":16000,"_type":"Audio"}`,
		`{"_type":"Json"}`,
		`{"languages":["en","fr"],"_type":"Translation"}`,
		`{"a":{"dtype":"int32","_type":"Value"},"b":{"x":{"dtype":"bool","_type":"Value"}}}`,
		`[{"dtype":"string","_type":"Value"}]`,
		`{"shape":[2,3],"dtype":"float32","_type":"Array2D"}`,
	}
	for _, c := range cases {
		f, err := features.ParseFeature([]byte(c))
		require.NoError(t, err, c)
		out, err := json.Marshal(f)
		require.NoError(t, err)
		require.JSONEq(t, c, string(out), c)
		require.Equal(t, c, string(out), "key order of %s", c)
	}
}

func TestFeatureNormalisation(t *testing.T) {
	cases := map[string]string{
		// Defaults are left out, aliases become pyarrow names.
		`{"dtype":"float","id":null,"_type":"Value"}`:                      `{"dtype":"float32","_type":"Value"}`,
		`{"decode":true,"mode":null,"_type":"Image"}`:                      `{"_type":"Image"}`,
		`{"feature":{"dtype":"int64","_type":"Value"},"_type":"Sequence"}`: `{"feature":{"dtype":"int64","_type":"Value"},"_type":"List"}`,
		// A Sequence of a struct is a struct of Lists.
		`{"feature":{"a":{"dtype":"int64","_type":"Value"}},"length":-1,"_type":"Sequence"}`: `{"a":{"feature":{"dtype":"int64","_type":"Value"},"_type":"List"}}`,
		`{"num_classes":2,"_type":"ClassLabel"}`:                                             `{"names":["0","1"],"_type":"ClassLabel"}`,
	}
	for in, want := range cases {
		f, err := features.ParseFeature([]byte(in))
		require.NoError(t, err, in)
		out, err := json.Marshal(f)
		require.NoError(t, err)
		require.Equal(t, want, string(out), in)
	}
}

func TestFeatureErrors(t *testing.T) {
	for _, c := range []string{``, `3`, `{"_type":"Nope"}`, `{"_type":"Value"}`, `{"dtype":"wat","_type":"Value"}`,
		`[]`, `[1,2]`, `{"_type":"List"}`, `{"num_classes":-1,"_type":"ClassLabel"}`, `{"a":3}`} {
		_, err := features.ParseFeature([]byte(c))
		require.Error(t, err, c)
	}
	_, err := features.ParseFeatures([]byte(`{"dtype":"int64","_type":"Value"}`))
	require.Error(t, err)
}

func TestParseFeaturesKeepsOrder(t *testing.T) {
	var fs features.Features
	require.NoError(t, json.Unmarshal([]byte(`{"z":{"dtype":"string","_type":"Value"},"a":{"dtype":"int64","_type":"Value"}}`), &fs))
	require.Equal(t, []string{"z", "a"}, fs.Names())
	require.Equal(t, 1, fs.Index("a"))
	require.Equal(t, -1, fs.Index("q"))
	out, err := json.Marshal(fs)
	require.NoError(t, err)
	require.Equal(t, `{"z":{"dtype":"string","_type":"Value"},"a":{"dtype":"int64","_type":"Value"}}`, string(out))
}

func TestStorageAndFromArrow(t *testing.T) {
	fs, err := features.ParseFeatures([]byte(`{
		"label":{"names":["a"],"_type":"ClassLabel"},
		"img":{"_type":"Image"},
		"tags":{"feature":{"dtype":"string","_type":"Value"},"_type":"List"},
		"fixed":{"feature":{"dtype":"float32","_type":"Value"},"length":2,"_type":"List"},
		"big":{"feature":{"dtype":"int8","_type":"Value"},"_type":"LargeList"},
		"meta":{"x":{"dtype":"timestamp[ms, tz=UTC]","_type":"Value"}},
		"j":{"_type":"Json"},
		"tr":{"languages":["fr","en"],"_type":"Translation"},
		"tv":{"_type":"TranslationVariableLanguages"}}`))
	require.NoError(t, err)
	st, err := fs.StorageSchema()
	require.NoError(t, err)
	require.Equal(t, "struct<label: int64, img: struct<bytes: binary, path: string>, tags: list<item: string>, "+
		"fixed: fixed_size_list<item: float32>[2], big: large_list<item: int8>, meta: struct<x: timestamp[ms, tz=UTC]>, "+
		"j: string, tr: struct<en: string, fr: string>, tv: struct<language: list<item: string>, translation: list<item: string>>>", st.String())

	back := features.FromArrowSchema(st)
	out, err := json.Marshal(back[2].Feature)
	require.NoError(t, err)
	require.Equal(t, `{"feature":{"dtype":"string","_type":"Value"},"_type":"List"}`, string(out))
	out, err = json.Marshal(back[3].Feature)
	require.NoError(t, err)
	require.Equal(t, `{"feature":{"dtype":"float32","_type":"Value"},"length":2,"_type":"List"}`, string(out))
	require.True(t, fs[1].Feature.IsMedia())
	require.True(t, fs[2].Feature.IsListKind())
	require.True(t, (&features.Feature{Kind: features.KindStruct, Fields: fs}).Contains(func(f *features.Feature) bool { return f.Kind == features.KindJSON }))

	_, err = (&features.Feature{Kind: features.KindArray2D, Dtype: "int8"}).Storage()
	require.ErrorIs(t, err, features.ErrUnsupported)
}

func TestParseDType(t *testing.T) {
	for in, want := range map[string]string{
		"int64": "int64", "float": "float32", "double": "float64", "utf8": "string", "boolean": "bool",
		"timestamp[s]": "timestamp[s]", "timestamp[ns, tz=Europe/London]": "timestamp[ns, tz=Europe/London]",
		"time32[ms]": "time32[ms]", "time64[us]": "time64[us]", "duration[s]": "duration[s]",
		"decimal128(10, 2)": "decimal128(10, 2)", "large_binary": "large_binary", "null": "null", "date32": "date32",
	} {
		a, err := features.ParseDType(in)
		require.NoError(t, err, in)
		require.Equal(t, want, a.DType())
	}
	for _, bad := range []string{"timestamp[h]", "time32[us]", "decimal128(0, 1)", "decimal128(5, 9)", "int128", "timestamp[s, utc]"} {
		_, err := features.ParseDType(bad)
		require.Error(t, err, bad)
	}
}

// TestNBytes checks sizes against pyarrow 26's Array.nbytes for the same
// values.
func TestNBytes(t *testing.T) {
	str := features.Primitive(features.String)
	foo := make([]any, 10)
	for i := range foo {
		foo[i] = "foo"
	}
	require.EqualValues(t, 70, features.NBytes(str, foo))
	require.EqualValues(t, 25, features.NBytes(features.Primitive(features.Int64), []any{int64(1), nil, int64(3)}))
	trues := make([]any, 10)
	for i := range trues {
		trues[i] = true
	}
	require.EqualValues(t, 2, features.NBytes(features.Primitive(features.Bool), trues))
	trues[1], trues[3], trues[5], trues[7], trues[9] = nil, nil, nil, nil, nil
	require.EqualValues(t, 4, features.NBytes(features.Primitive(features.Bool), trues))
	require.EqualValues(t, 32, features.NBytes(features.ListOf(features.Primitive(features.Int64)),
		[]any{[]any{int64(1), int64(2)}, []any{int64(3)}}))
	st := features.StructOf(features.ArrowField{Name: "a", Type: features.Primitive(features.Int64)}, features.ArrowField{Name: "b", Type: str})
	require.EqualValues(t, 13, features.NBytes(st, []any{[]any{int64(1), "x"}}))
	require.EqualValues(t, 16, features.NBytes(str, []any{"a", nil, "bb"}))
	require.EqualValues(t, 7, features.NBytes(features.Primitive(features.Binary), []any{[]byte("abc")}))
	require.EqualValues(t, 25, features.NBytes(st, []any{[]any{int64(1), "x"}, nil}))
	require.EqualValues(t, 30, features.NBytes(features.ListOf(features.Primitive(features.Int64)), []any{[]any{int64(1), nil}, nil, []any{}}))
	nested := features.StructOf(features.ArrowField{Name: "a", Type: features.Primitive(features.Int64)})
	require.EqualValues(t, 17, features.NBytes(nested, []any{[]any{nil}, []any{int64(2)}}))
	require.EqualValues(t, 0, features.NBytes(features.Primitive(features.Null), []any{nil, nil}))
	fixed := &features.Arrow{ID: features.FixedSizeList, Size: 2, Elem: features.Primitive(features.Float32)}
	require.EqualValues(t, 18, features.NBytes(fixed, []any{[]any{1.0, 2.0}, nil}))
}

func TestTimeHelpers(t *testing.T) {
	ts := time.Date(1969, 12, 31, 23, 59, 59, 500_000_000, time.UTC)
	require.Equal(t, int64(-500), features.TimeToUnits(ts, "ms"))
	require.Equal(t, ts, features.TimeFromUnits(-500, "ms"))
	require.Equal(t, int64(-1), features.TimeToDays(ts))
	require.Equal(t, time.Date(1970, 1, 3, 0, 0, 0, 0, time.UTC), features.DaysToTime(2))
	require.Equal(t, int64(1_000_000_000), features.UnitsPerSecond("ns"))
}

func TestArrowHelpers(t *testing.T) {
	st := features.StructOf(features.ArrowField{Name: "a", Type: features.Primitive(features.Int64)})
	require.Equal(t, 0, st.Field("a"))
	require.Equal(t, -1, st.Field("b"))
	require.True(t, st.Equal(features.StructOf(features.ArrowField{Name: "a", Type: features.Primitive(features.Int64)})))
	require.False(t, st.Equal(features.StructOf(features.ArrowField{Name: "b", Type: features.Primitive(features.Int64)})))
	require.True(t, features.Primitive(features.Uint8).IsInteger())
	require.True(t, features.Primitive(features.Float16).IsFloat())
	require.True(t, features.Primitive(features.LargeString).IsString())
	require.True(t, features.Primitive(features.LargeBinary).IsBinary())
	require.True(t, features.Primitive(features.Date64).IsTemporal())
	require.True(t, features.ListOf(st).IsList())
	require.True(t, features.Primitive(features.Int8).IsNumeric())
}
