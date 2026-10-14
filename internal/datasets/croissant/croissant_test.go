package croissant_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jamesagarside/hangar/internal/datasets/croissant"
	"github.com/jamesagarside/hangar/internal/datasets/features"
)

func parse(t *testing.T, js string) features.Features {
	t.Helper()
	fs, err := features.ParseFeatures([]byte(js))
	require.NoError(t, err)
	return fs
}

func build(t *testing.T, in croissant.Input) map[string]any {
	t.Helper()
	b, err := croissant.Build(in)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(b, &doc))
	return doc
}

func recordSet(doc map[string]any, id string) map[string]any {
	for _, rs := range doc["recordSet"].([]any) {
		m := rs.(map[string]any)
		if m["@id"] == id {
			return m
		}
	}
	return nil
}

func fieldByID(rs map[string]any, id string) map[string]any {
	for _, f := range rs["field"].([]any) {
		m := f.(map[string]any)
		if m["@id"] == id {
			return m
		}
	}
	return nil
}

func TestBuildDemo(t *testing.T) {
	// The Hub's document for lhoestq/demo1, with the current field types.
	in := croissant.Input{Dataset: "lhoestq/demo1", URL: "https://hangar.example/datasets/lhoestq/demo1",
		ExportURL: "https://hangar.example/datasets/lhoestq/demo1/tree/refs%2Fconvert%2Fparquet",
		Creator:   "lhoestq", CreatorURL: "https://hangar.example/lhoestq", Keywords: []string{"< 1K", "csv"},
		Configs: []croissant.Config{{Name: "default", Splits: []string{"train", "test"}, Features: parse(t,
			`{"id":{"dtype":"string","_type":"Value"},"star":{"dtype":"int64","_type":"Value"}}`)}}}
	b, err := croissant.Build(in)
	require.NoError(t, err)
	doc := build(t, in)
	require.Equal(t, "sc:Dataset", doc["@type"])
	require.Equal(t, "http://mlcommons.org/croissant/1.0", doc["conformsTo"])
	require.Equal(t, "demo1", doc["name"])
	require.Equal(t, "lhoestq/demo1 dataset hosted on Hangar.", doc["description"])
	require.Equal(t, []any{"lhoestq/demo1"}, doc["alternateName"])
	require.Equal(t, map[string]any{"@type": "Person", "name": "lhoestq", "url": "https://hangar.example/lhoestq"}, doc["creator"])
	require.Equal(t, []any{"< 1K", "csv"}, doc["keywords"])
	require.NotContains(t, doc, "license")
	require.NotContains(t, doc, "citeAs")
	require.Equal(t, in.URL, doc["url"])
	// Keys keep the Hub's order.
	require.True(t, strings.HasPrefix(string(b), `{"@context":{"@language":"en","@vocab":"https://schema.org/"`))

	dist := doc["distribution"].([]any)
	require.Len(t, dist, 2)
	require.Equal(t, in.ExportURL, dist[0].(map[string]any)["contentUrl"])
	require.Equal(t, "default/*/*.parquet", dist[1].(map[string]any)["includes"])

	splits := recordSet(doc, "default_splits")
	require.Equal(t, []any{map[string]any{"default_splits/split_name": "train"}, map[string]any{"default_splits/split_name": "test"}}, splits["data"])
	rs := recordSet(doc, "default")
	require.Equal(t, "lhoestq/demo1 - 'default' subset\n\nAdditional information:\n- 2 splits: train, test", rs["description"])
	split := fieldByID(rs, "default/split")
	require.Equal(t, "default/(?:partial-)?(train|test)/.+parquet$", split["source"].(map[string]any)["transform"].(map[string]any)["regex"])
	star := fieldByID(rs, "default/star")
	require.Equal(t, "cr:Int64", star["dataType"])
	require.Equal(t, map[string]any{"fileSet": map[string]any{"@id": "parquet-files-for-config-default"}, "extract": map[string]any{"column": "star"}}, star["source"])
}

func TestBuildFieldTypes(t *testing.T) {
	fs := parse(t, `{
		"text": {"dtype": "string", "_type": "Value"},
		"flag": {"dtype": "bool", "_type": "Value"},
		"when": {"dtype": "timestamp[us]", "_type": "Value"},
		"odd": {"dtype": "decimal128(10, 2)", "_type": "Value"},
		"label": {"names": ["a", "b"], "_type": "ClassLabel"},
		"image": {"_type": "Image"},
		"audio": {"sampling_rate": 16000, "_type": "Audio"},
		"audio2": {"_type": "Audio"},
		"video": {"_type": "Video"},
		"meta": {"source": {"dtype": "string", "_type": "Value"}, "it's [x]": {"dtype": "float32", "_type": "Value"},
			"pic": {"_type": "Image"}, "deep": {"v": {"dtype": "int8", "_type": "Value"}}},
		"tags": {"feature": {"dtype": "string", "_type": "Value"}, "_type": "List"},
		"vec": {"feature": {"dtype": "float32", "_type": "Value"}, "length": 3, "_type": "List"},
		"grid": {"feature": {"feature": {"dtype": "int32", "_type": "Value"}, "length": 2, "_type": "List"}, "_type": "List"},
		"big": {"feature": {"dtype": "int64", "_type": "Value"}, "_type": "LargeList"},
		"legacy": [{"dtype": "uint8", "_type": "Value"}],
		"bad list": {"feature": {"_type": "Json"}, "_type": "List"},
		"arr": {"shape": [2, 2], "dtype": "float32", "_type": "Array2D"},
		"doc": {"_type": "Json"}
	}`)
	doc := build(t, croissant.Input{Dataset: "ns/types", Configs: []croissant.Config{{Name: "c", Splits: []string{"train"}, Features: fs, Partial: true}}})
	rs := recordSet(doc, "c")
	require.Equal(t, "ns/types - 'c' subset (first 5GB)\n\nAdditional information:\n- 4 skipped columns: odd, bad list, arr, doc", rs["description"])

	types := map[string]any{}
	for _, f := range rs["field"].([]any) {
		m := f.(map[string]any)
		types[m["@id"].(string)] = m["dataType"]
	}
	require.Equal(t, map[string]any{
		"c/split": "sc:Text", "c/text": "sc:Text", "c/flag": "sc:Boolean", "c/when": "sc:DateTime", "c/label": "sc:Integer",
		"c/image": "sc:ImageObject", "c/audio": "sc:AudioObject", "c/audio2": "sc:AudioObject", "c/video": "sc:VideoObject",
		"c/meta": nil, "c/tags": "sc:Text", "c/vec": "cr:Float32", "c/grid": "cr:Int32", "c/big": "cr:Int64", "c/legacy": "cr:UInt8",
	}, types)

	image := fieldByID(rs, "c/image")
	require.Equal(t, map[string]any{"jsonPath": "bytes"}, image["source"].(map[string]any)["transform"])
	audio := fieldByID(rs, "c/audio")
	require.EqualValues(t, 16000, audio["source"].(map[string]any)["sampling_rate"])
	require.NotContains(t, fieldByID(rs, "c/audio2")["source"], "sampling_rate")

	meta := fieldByID(rs, "c/meta")
	subs := meta["subField"].([]any)
	require.Len(t, subs, 4)
	src := subs[0].(map[string]any)
	require.Equal(t, "c/meta/source", src["@id"])
	require.Equal(t, map[string]any{"jsonPath": "source"}, src["source"].(map[string]any)["transform"])
	quoted := subs[1].(map[string]any)
	require.Equal(t, "c/meta/it_s__x_", quoted["@id"])
	require.Equal(t, map[string]any{"jsonPath": `['it\'s \[x\]']`}, quoted["source"].(map[string]any)["transform"])
	pic := subs[2].(map[string]any)
	require.Equal(t, []any{map[string]any{"jsonPath": "pic"}, map[string]any{"jsonPath": "bytes"}}, pic["source"].(map[string]any)["transform"])
	deep := subs[3].(map[string]any)["subField"].([]any)[0].(map[string]any)
	require.Equal(t, []any{map[string]any{"jsonPath": "deep"}, map[string]any{"jsonPath": "v"}}, deep["source"].(map[string]any)["transform"])

	vec := fieldByID(rs, "c/vec")
	require.Equal(t, true, vec["isArray"])
	require.Equal(t, "3", vec["arrayShape"])
	require.Equal(t, "-1,2", fieldByID(rs, "c/grid")["arrayShape"])
	require.Equal(t, "-1", fieldByID(rs, "c/big")["arrayShape"])
	require.Equal(t, "-1", fieldByID(rs, "c/legacy")["arrayShape"])
}

func TestBuildCardMetadata(t *testing.T) {
	long := strings.Repeat("é", 1600)
	doc := build(t, croissant.Input{Dataset: "ns/d", URL: "https://h/datasets/ns/d", PrettyName: "Pretty", Description: "  " + long + "  ",
		Licenses: []string{"MIT", "other-licence"}, Citation: "@misc{x}",
		Configs: []croissant.Config{{Name: "ns/d", Splits: []string{"train"}, Features: parse(t, `{"a":{"dtype":"int8","_type":"Value"}}`)}}})
	require.Equal(t, "Pretty", doc["name"])
	require.Equal(t, strings.Repeat("é", 1500)+"… See the full description on the dataset page: https://h/datasets/ns/d.", doc["description"])
	require.Equal(t, []any{"https://choosealicense.com/licenses/mit/", "other-licence"}, doc["license"])
	require.Equal(t, "@misc{x}", doc["citeAs"])
	// A Config named like the dataset gets a distinct record set name,
	// with characters Croissant reserves replaced.
	require.NotNil(t, recordSet(doc, "record_set_ns_d"))

	doc = build(t, croissant.Input{Dataset: "ns/d", Licenses: []string{"apache-2.0"}, Description: "Short.",
		Configs: []croissant.Config{{Name: "a", Splits: []string{"train"}}}})
	require.Equal(t, "https://choosealicense.com/licenses/apache-2.0/", doc["license"])
	require.Equal(t, "Short.", doc["description"])
}

func TestBuildLimits(t *testing.T) {
	var configs []croissant.Config
	for i := range croissant.MaxConfigs + 5 {
		configs = append(configs, croissant.Config{Name: fmt.Sprintf("c%d", i), Splits: []string{"train"}})
	}
	// Two Configs whose names escape to the same ID stay distinct.
	configs[0].Name, configs[1].Name = "a b", "a_b"
	doc := build(t, croissant.Input{Dataset: "ns/many", Configs: configs})
	require.Len(t, doc["recordSet"], 2*croissant.MaxConfigs)
	rs := recordSet(doc, "c2")
	require.Contains(t, rs["description"], "(only the first 100 subsets are included in this metadata)")
	require.NotNil(t, recordSet(doc, "a_b"))
	require.NotNil(t, recordSet(doc, "a_b_0"))

	var cols []string
	for i := range croissant.MaxColumns + 2 {
		cols = append(cols, fmt.Sprintf(`"col%d":{"dtype":"int8","_type":"Value"}`, i))
	}
	fs := parse(t, "{"+strings.Join(cols, ",")+"}")
	doc = build(t, croissant.Input{Dataset: "ns/wide", Configs: []croissant.Config{{Name: "w", Splits: []string{"train"}, Features: fs}}})
	rs = recordSet(doc, "w")
	require.Len(t, rs["field"], croissant.MaxColumns)
	require.Contains(t, rs["description"], "- 3 skipped columns (max number of columns reached)")
}
