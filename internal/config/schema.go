package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
)

// Field describes one configuration key.
type Field struct {
	// Key is the dotted key, for example `storage.s3.bucket`. Map entries use
	// `<id>` and list items use `[]`, for example `upstreams.<id>.base_url`.
	Key string
	// Env is the environment variable, for example `HANGAR_STORAGE_S3_BUCKET`.
	// It is empty for keys inside lists, which env vars cannot set one by one.
	Env string
	// Type is a short type name: string, bool, int, size, duration, list or map.
	Type string
	// Default is the default value, formatted as it would appear in YAML.
	Default string
	// Description is the text from the `desc` tag.
	Description string
	// Secret is true for keys that hold secrets.
	Secret bool

	kind reflect.Type
}

var (
	durationType = reflect.TypeFor[time.Duration]()
	byteSizeType = reflect.TypeFor[ByteSize]()
)

// EnvName returns the environment variable for a dotted key.
func EnvName(key string) string {
	return "HANGAR_" + strings.ToUpper(strings.NewReplacer(".", "_", "-", "_").Replace(key))
}

func typeName(t reflect.Type) string {
	switch t {
	case durationType:
		return "duration"
	case byteSizeType:
		return "size"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int64:
		return "int"
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Struct {
			return "list of objects"
		}
		return "list of " + typeName(t.Elem()) + "s"
	case reflect.Map:
		return "map of " + typeName(t.Elem()) + "s"
	default:
		return t.Kind().String()
	}
}

func formatValue(v reflect.Value) string {
	if !v.IsValid() {
		return ""
	}
	switch v.Type() {
	case durationType:
		return formatDuration(time.Duration(v.Int()))
	case byteSizeType:
		return ByteSize(v.Int()).String()
	}
	switch v.Kind() {
	case reflect.String:
		if v.String() == "" {
			return `""`
		}
		return v.String()
	case reflect.Slice:
		parts := make([]string, v.Len())
		for i := range v.Len() {
			parts[i] = formatValue(v.Index(i))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case reflect.Map:
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k.String()+": "+formatValue(v.MapIndex(k)))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprint(v.Interface())
	}
}

// formatDuration prints whole hours, minutes or seconds without the
// trailing zero units that time.Duration.String adds.
func formatDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "0s"
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return d.String()
	}
}

// Fields returns every configuration key in document order, with defaults
// from Default().
func Fields() []Field {
	def := Default()
	var out []Field
	walk(reflect.ValueOf(def), reflect.TypeFor[Config](), "", true, &out)
	return out
}

func walk(v reflect.Value, t reflect.Type, prefix string, envOK bool, out *[]Field) {
	for i := range t.NumField() {
		sf := t.Field(i)
		name := sf.Tag.Get("koanf")
		if name == "" {
			continue
		}
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		var fv reflect.Value
		if v.IsValid() {
			fv = v.Field(i)
		}
		ft := sf.Type
		switch {
		case ft.Kind() == reflect.Struct && ft != durationType:
			walk(fv, ft, key, envOK, out)
		case ft.Kind() == reflect.Map && ft.Elem().Kind() == reflect.Struct:
			// Maps of structs, such as upstreams: document `<id>` entries,
			// with the defaults of the built-in entry if there is one.
			var entry reflect.Value
			if fv.IsValid() && fv.Len() > 0 {
				keys := fv.MapKeys()
				sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
				entry = fv.MapIndex(keys[0])
			}
			walk(entry, ft.Elem(), key+".<id>", envOK, out)
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct:
			*out = append(*out, Field{Key: key, Env: envFor(key, envOK), Type: typeName(ft), Default: formatValue(fv), Description: sf.Tag.Get("desc"), kind: ft})
			walk(reflect.Value{}, ft.Elem(), key+"[]", false, out)
		default:
			*out = append(*out, Field{
				Key:         key,
				Env:         envFor(key, envOK),
				Type:        typeName(ft),
				Default:     formatValue(fv),
				Description: sf.Tag.Get("desc"),
				Secret:      sf.Tag.Get("secret") == "true",
				kind:        ft,
			})
		}
	}
}

func envFor(key string, ok bool) string {
	if !ok {
		return ""
	}
	return EnvName(key)
}

// fieldType returns the Go type of a concrete dotted key, resolving map
// entries such as `upstreams.huggingface.token`.
func fieldType(key string) (reflect.Type, bool) {
	t := reflect.TypeFor[Config]()
	parts := strings.Split(key, ".")
	for i := 0; i < len(parts); i++ {
		if t.Kind() == reflect.Map {
			t = t.Elem()
			continue // parts[i] is the map key
		}
		if t.Kind() != reflect.Struct || t == durationType {
			return nil, false
		}
		found := false
		for j := range t.NumField() {
			if t.Field(j).Tag.Get("koanf") == parts[i] {
				t = t.Field(j).Type
				found = true
				break
			}
		}
		if !found {
			return nil, false
		}
	}
	return t, true
}
