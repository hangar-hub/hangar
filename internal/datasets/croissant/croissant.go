// Package croissant builds MLCommons Croissant JSON-LD metadata for a
// dataset (DATA-10), as the Hub's /api/datasets/{repo}/croissant does:
// the dataset's card metadata, the Parquet export as the distribution and
// one record set per Config with a field per column (dataset-viewer
// libcommon/croissant_utils.py and croissant_crumbs.py).
package croissant

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jamesagarside/hangar/internal/datasets/features"
)

// MaxConfigs is CROISSANT_MAX_CONFIGS.
const MaxConfigs = 100

// MaxColumns is the most fields of one record set.
const MaxColumns = 1000

// maxDescription is the length of the card text kept in the description.
const maxDescription = 1500

// Config is a converted Config.
type Config struct {
	Name     string
	Features features.Features
	Splits   []string
	Partial  bool
}

// Input is what the metadata is built from.
type Input struct {
	// Dataset is `namespace/name`.
	Dataset string
	// URL is the dataset page, and ExportURL the export ref's tree page.
	URL       string
	ExportURL string
	// Card metadata.
	PrettyName  string
	Description string
	Licenses    []string
	Keywords    []string
	Citation    string
	Creator     string
	CreatorURL  string
	Configs     []Config
}

type kv struct {
	k string
	v any
}

// obj is an ordered JSON object.
type obj []kv

func (o obj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	for _, x := range o {
		if x.v == nil {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		k, _ := json.Marshal(x.k)
		b.Write(k)
		b.WriteByte(':')
		v, err := json.Marshal(x.v)
		if err != nil {
			return nil, err
		}
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

var namePattern = regexp.MustCompile(`[^a-zA-Z0-9\-_.]`)

// escapeID is escape_ids: unique IDs without characters Croissant reserves.
func escapeID(id string, ids map[string]bool) string {
	e := namePattern.ReplaceAllString(id, "_")
	for ids[e] {
		e += "_0"
	}
	ids[e] = true
	return e
}

var valueTypes = map[string]string{
	"binary": "sc:Text", "bool": "sc:Boolean", "date32": "sc:Date", "date64": "sc:Date", "float8": "sc:Float",
	"float16": "cr:Float16", "float32": "cr:Float32", "float64": "cr:Float64", "int8": "cr:Int8", "int16": "cr:Int16",
	"int32": "cr:Int32", "int64": "cr:Int64", "large_string": "sc:Text", "string": "sc:Text", "time32[s]": "sc:Time",
	"time32[ms]": "sc:Time", "time64[us]": "sc:Time", "time64[ns]": "sc:Time", "timestamp[s]": "sc:DateTime",
	"timestamp[ms]": "sc:DateTime", "timestamp[us]": "sc:DateTime", "timestamp[ns]": "sc:DateTime", "uint8": "cr:UInt8",
	"uint16": "cr:UInt16", "uint32": "cr:UInt32", "uint64": "cr:UInt64",
}

var jsonPathKey = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

func escapeJSONPath(name string) string {
	if jsonPathKey.MatchString(name) {
		return name
	}
	name = strings.NewReplacer(`'`, `\'`, `[`, `\[`, `]`, `\]`).Replace(name)
	return "['" + name + "']"
}

func source(fileSet, column string, transform bool, jsonPath []string) obj {
	src := obj{{"fileSet", obj{{"@id", fileSet}}}, {"extract", obj{{"column", column}}}}
	if transform && len(jsonPath) > 0 {
		if len(jsonPath) == 1 {
			src = append(src, kv{"transform", obj{{"jsonPath", jsonPath[0]}}})
		} else {
			var t []any
			for _, p := range jsonPath {
				t = append(t, obj{{"jsonPath", p}})
			}
			src = append(src, kv{"transform", t})
		}
	}
	return src
}

// field is feature_to_croissant_field, with the field name and
// description the Hub's /croissant adds.
func field(fileSet, fieldID, column string, f *features.Feature, ids map[string]bool, transform bool, jsonPath []string) obj {
	desc := fmt.Sprintf("Column '%s' from the Hugging Face parquet file.", column)
	head := obj{{"@type", "cr:Field"}, {"@id", fieldID}, {"name", fieldID}, {"description", desc}}
	switch f.Kind {
	case features.KindValue:
		t, ok := valueTypes[f.Dtype]
		if !ok {
			return nil
		}
		return append(head, kv{"dataType", t}, kv{"source", source(fileSet, column, transform, jsonPath)})
	case features.KindImage:
		src := source(fileSet, column, transform, jsonPath)
		bytesPath := obj{{"jsonPath", "bytes"}}
		if len(src) == 3 {
			src[2] = kv{"transform", []any{src[2].v, bytesPath}}
		} else {
			src = append(src, kv{"transform", bytesPath})
		}
		return append(head, kv{"dataType", "sc:ImageObject"}, kv{"source", src})
	case features.KindAudio:
		src := source(fileSet, column, transform, jsonPath)
		if rate := f.Param("sampling_rate"); rate != nil && string(rate) != "null" {
			var r any
			if json.Unmarshal(rate, &r) == nil {
				src = append(src, kv{"sampling_rate", r})
			}
		}
		return append(head, kv{"dataType", "sc:AudioObject"}, kv{"source", src})
	case features.KindVideo:
		return append(head, kv{"dataType", "sc:VideoObject"}, kv{"source", source(fileSet, column, transform, jsonPath)})
	case features.KindClassLabel:
		return append(head, kv{"dataType", "sc:Integer"}, kv{"source", source(fileSet, column, transform, jsonPath)})
	case features.KindStruct:
		var subs []any
		for _, sf := range f.Fields {
			p := append(append([]string{}, jsonPath...), escapeJSONPath(sf.Name))
			sub := field(fileSet, fieldID+"/"+escapeID(sf.Name, ids), column, sf.Feature, ids, true, p)
			if sub != nil {
				subs = append(subs, sub)
			}
		}
		return append(head, kv{"subField", subs})
	case features.KindList, features.KindLargeList, features.KindLegacyList:
		var shape []string
		sub := f
		for sub.IsListKind() {
			l := -1
			if sub.Kind == features.KindList {
				l = sub.Length
			}
			if l == 0 {
				l = -1
			}
			shape = append(shape, fmt.Sprint(l))
			sub = sub.Elem
			if sub.Kind != features.KindList {
				break
			}
		}
		out := field(fileSet, fieldID, column, sub, ids, true, jsonPath)
		if out == nil {
			return nil
		}
		return append(out, kv{"isArray", true}, kv{"arrayShape", strings.Join(shape, ",")})
	}
	return nil
}

func jsonLDContext() obj {
	return obj{{"@language", "en"}, {"@vocab", "https://schema.org/"}, {"arrayShape", "cr:arrayShape"}, {"citeAs", "cr:citeAs"},
		{"column", "cr:column"}, {"conformsTo", "dct:conformsTo"}, {"cr", "http://mlcommons.org/croissant/"},
		{"data", obj{{"@id", "cr:data"}, {"@type", "@json"}}}, {"dataBiases", "cr:dataBiases"}, {"dataCollection", "cr:dataCollection"},
		{"dataType", obj{{"@id", "cr:dataType"}, {"@type", "@vocab"}}}, {"dct", "http://purl.org/dc/terms/"}, {"extract", "cr:extract"},
		{"field", "cr:field"}, {"fileProperty", "cr:fileProperty"}, {"fileObject", "cr:fileObject"}, {"fileSet", "cr:fileSet"},
		{"format", "cr:format"}, {"includes", "cr:includes"}, {"isArray", "cr:isArray"}, {"isLiveDataset", "cr:isLiveDataset"},
		{"jsonPath", "cr:jsonPath"}, {"key", "cr:key"}, {"md5", "cr:md5"}, {"parentField", "cr:parentField"}, {"path", "cr:path"},
		{"personalSensitiveInformation", "cr:personalSensitiveInformation"}, {"recordSet", "cr:recordSet"},
		{"references", "cr:references"}, {"regex", "cr:regex"}, {"repeated", "cr:repeated"}, {"replace", "cr:replace"},
		{"sc", "https://schema.org/"}, {"separator", "cr:separator"}, {"source", "cr:source"}, {"subField", "cr:subField"},
		{"transform", "cr:transform"}}
}

// Build returns the Croissant JSON-LD document.
func Build(in Input) ([]byte, error) {
	ids := map[string]bool{"repo": true}
	distribution := []any{obj{{"@type", "cr:FileObject"}, {"@id", "repo"}, {"name", "repo"},
		{"description", "The Hugging Face git repository."}, {"contentUrl", in.ExportURL}, {"encodingFormat", "git+https"},
		{"sha256", "https://github.com/mlcommons/croissant/issues/80"}}}
	var recordSets []any
	configs := in.Configs
	truncated := len(configs) > MaxConfigs
	if truncated {
		configs = configs[:MaxConfigs]
	}
	_, datasetName, _ := strings.Cut(in.Dataset, "/")
	for _, c := range configs {
		fileSet := escapeID("parquet-files-for-config-"+c.Name, ids)
		distribution = append(distribution, obj{{"@type", "cr:FileSet"}, {"@id", fileSet}, {"name", fileSet},
			{"description", "The underlying Parquet files as converted by Hangar."}, {"containedIn", obj{{"@id", "repo"}}},
			{"encodingFormat", "application/x-parquet"}, {"includes", c.Name + "/*/*.parquet"}})
		rsName := c.Name
		if rsName == in.Dataset {
			rsName = "record_set_" + rsName
		}
		rsName = escapeID(rsName, ids)
		splitRS := rsName + "_splits"
		var data []any
		for _, s := range c.Splits {
			data = append(data, obj{{splitRS + "/split_name", s}})
		}
		recordSets = append(recordSets, obj{{"@type", "cr:RecordSet"}, {"dataType", "cr:Split"}, {"key", obj{{"@id", splitRS + "/split_name"}}},
			{"@id", splitRS}, {"name", splitRS}, {"description", "Splits for the " + rsName + " config."},
			{"field", []any{obj{{"@type", "cr:Field"}, {"@id", splitRS + "/split_name"}, {"name", "split_name"},
				{"description", "The name of the split."}, {"dataType", "sc:Text"}}}}, {"data", data}})
		quoted := make([]string, len(c.Splits))
		for i, s := range c.Splits {
			quoted[i] = regexp.QuoteMeta(s)
		}
		fields := []any{obj{{"@type", "cr:Field"}, {"@id", rsName + "/split"}, {"name", rsName + "/split"},
			{"description", "Split to which the example belongs to."}, {"dataType", "sc:Text"},
			{"source", obj{{"fileSet", obj{{"@id", fileSet}}}, {"extract", obj{{"fileProperty", "fullpath"}}},
				{"transform", obj{{"regex", regexp.QuoteMeta(c.Name) + "/(?:partial-)?(" + strings.Join(quoted, "|") + ")/.+parquet$"}}}}},
			{"references", obj{{"field", obj{{"@id", splitRS + "/split_name"}}}}}}}
		var skipped []string
		for _, f := range c.Features {
			fieldNames := map[string]bool{}
			fid := rsName + "/" + escapeID(f.Name, fieldNames)
			fl := field(fileSet, fid, f.Name, f.Feature, ids, false, nil)
			if fl == nil {
				skipped = append(skipped, f.Name)
				continue
			}
			fields = append(fields, fl)
		}
		desc := fmt.Sprintf("%s - '%s' subset", in.Dataset, c.Name)
		if c.Partial {
			desc += " (first 5GB)"
		}
		if truncated {
			desc += fmt.Sprintf(" (only the first %d subsets are included in this metadata)", MaxConfigs)
		}
		body := ""
		if len(c.Splits) > 1 {
			body += fmt.Sprintf("\n- %d splits: %s", len(c.Splits), strings.Join(c.Splits, ", "))
		}
		if len(skipped) > 0 {
			s := ""
			if len(skipped) > 1 {
				s = "s"
			}
			body += fmt.Sprintf("\n- %d skipped column%s: %s", len(skipped), s, strings.Join(skipped, ", "))
		}
		if len(fields) > MaxColumns {
			body += fmt.Sprintf("\n- %d skipped columns (max number of columns reached)", len(fields)-MaxColumns)
			fields = fields[:MaxColumns]
		}
		if body != "" {
			desc += "\n\nAdditional information:" + body
		}
		recordSets = append(recordSets, obj{{"@type", "cr:RecordSet"}, {"@id", rsName}, {"name", rsName}, {"description", desc}, {"field", fields}})
	}
	name := in.PrettyName
	if name == "" {
		name = datasetName
	}
	description := strings.TrimSpace(in.Description)
	if utf8.RuneCountInString(description) > maxDescription {
		r := []rune(description)
		description = string(r[:maxDescription]) + "… See the full description on the dataset page: " + in.URL + "."
	}
	if description == "" {
		description = in.Dataset + " dataset hosted on Hangar."
	}
	doc := obj{{"@context", jsonLDContext()}, {"@type", "sc:Dataset"}, {"conformsTo", "http://mlcommons.org/croissant/1.0"},
		{"distribution", distribution}, {"recordSet", recordSets}, {"name", name}, {"description", description},
		{"alternateName", []string{in.Dataset}}}
	if in.Creator != "" {
		doc = append(doc, kv{"creator", obj{{"@type", "Person"}, {"name", in.Creator}, {"url", in.CreatorURL}}})
	}
	if len(in.Keywords) > 0 {
		doc = append(doc, kv{"keywords", in.Keywords})
	}
	if len(in.Licenses) > 0 {
		var lic []any
		for _, l := range in.Licenses {
			lic = append(lic, licenseURL(l))
		}
		if len(lic) == 1 {
			doc = append(doc, kv{"license", lic[0]})
		} else {
			doc = append(doc, kv{"license", lic})
		}
	}
	if in.Citation != "" {
		doc = append(doc, kv{"citeAs", in.Citation})
	}
	doc = append(doc, kv{"url", in.URL})
	return json.Marshal(doc)
}

// licenseURL turns a Hub licence identifier into a URL when it is a known
// SPDX identifier, as the Hub's Croissant does.
func licenseURL(id string) string {
	known := map[string]string{"mit": "MIT", "apache-2.0": "Apache-2.0", "cc-by-4.0": "CC-BY-4.0", "cc-by-sa-4.0": "CC-BY-SA-4.0",
		"cc0-1.0": "CC0-1.0", "cc-by-nc-4.0": "CC-BY-NC-4.0", "cc-by-nc-sa-4.0": "CC-BY-NC-SA-4.0", "bsd-3-clause": "BSD-3-Clause",
		"gpl-3.0": "GPL-3.0", "odc-by": "ODC-By-1.0", "openrail": "OpenRAIL"}
	if spdx, ok := known[strings.ToLower(id)]; ok {
		return "https://choosealicense.com/licenses/" + strings.ToLower(spdx) + "/"
	}
	return id
}
