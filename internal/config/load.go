package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// ConfigFileEnv names the environment variable that can give the config
// file path instead of the --config flag.
const ConfigFileEnv = "HANGAR_CONFIG"

// envIgnorePrefixes are HANGAR_* variables that are not configuration keys.
var envIgnorePrefixes = []string{ConfigFileEnv, "HANGAR_TEST_"}

// LoadOptions says where to read configuration from.
type LoadOptions struct {
	// File is a YAML file. Empty means no file, unless HANGAR_CONFIG is set.
	File string
	// Env is the environment, as from os.Environ. Nil means os.Environ().
	Env []string
	// Set holds `key=value` overrides from flags, applied last.
	Set []string
}

// Errors collects every problem found while loading or validating.
type Errors []string

func (e Errors) Error() string {
	if len(e) == 1 {
		return "invalid configuration: " + e[0]
	}
	return fmt.Sprintf("invalid configuration (%d problems):\n  - %s", len(e), strings.Join(e, "\n  - "))
}

func (e *Errors) addf(format string, args ...any) { *e = append(*e, fmt.Sprintf(format, args...)) }

// Load reads defaults, then the YAML file, then environment variables, then
// flag overrides. It reports every parse error at once, but does not run
// Validate.
func Load(opts LoadOptions) (*Config, error) {
	env := opts.Env
	if env == nil {
		env = os.Environ()
	}
	path := opts.File
	if path == "" {
		path = lookupEnv(env, ConfigFileEnv)
	}

	var errs Errors
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(toMap(reflect.ValueOf(Default())), "."), nil); err != nil {
		return nil, fmt.Errorf("load defaults: %w", err)
	}
	if path != "" {
		if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
			errs.addf("read config file %s: %v", path, err)
		}
	}

	known := map[string]string{}
	for _, key := range k.Keys() {
		known[EnvName(key)] = key
	}
	// Add every documented key, including keys of map entries (such as an
	// Upstream defined in the file) that the file leaves unset.
	for _, f := range Fields() {
		if f.Env == "" {
			continue
		}
		mapPrefix, field, isMap := strings.Cut(f.Key, ".<id>.")
		if !isMap {
			known[f.Env] = f.Key
			continue
		}
		for _, id := range k.MapKeys(mapPrefix) {
			key := mapPrefix + "." + id + "." + field
			known[EnvName(key)] = key
		}
	}

	envKeys := make([]string, 0)
	envVals := map[string]string{}
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, "HANGAR_") || ignoredEnv(name) {
			continue
		}
		key, ok := known[name]
		if !ok {
			errs.addf("unknown environment variable %s", name)
			continue
		}
		envKeys = append(envKeys, key)
		envVals[key] = value
	}
	sort.Strings(envKeys)
	for _, key := range envKeys {
		if err := setString(k, key, envVals[key]); err != nil {
			errs.addf("environment variable %s: %v", EnvName(key), err)
		}
	}

	for _, kv := range opts.Set {
		key, value, ok := strings.Cut(kv, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			errs.addf("--set %q: use key=value", kv)
			continue
		}
		if _, ok := fieldType(key); !ok {
			errs.addf("--set %s: unknown configuration key", key)
			continue
		}
		if err := setString(k, key, value); err != nil {
			errs.addf("--set %s: %v", key, err)
		}
	}

	cfg := Config{}
	dc := &mapstructure.DecoderConfig{
		Result:           &cfg,
		TagName:          "koanf",
		WeaklyTypedInput: true,
		ErrorUnused:      true,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.TextUnmarshallerHookFunc(),
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
		),
	}
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{Tag: "koanf", DecoderConfig: dc}); err != nil {
		for _, line := range splitDecodeError(err) {
			errs.addf("%s", line)
		}
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return &cfg, nil
}

func ignoredEnv(name string) bool {
	for _, p := range envIgnorePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func lookupEnv(env []string, name string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v
		}
	}
	return ""
}

// setString sets a key from a string, converting it to the key's type where
// mapstructure's weak typing does not: lists are comma-separated, and lists
// of objects are JSON.
func setString(k *koanf.Koanf, key, value string) error {
	t, ok := fieldType(key)
	if !ok {
		return errors.New("unknown configuration key")
	}
	switch {
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Struct:
		var list []map[string]any
		if err := json.Unmarshal([]byte(value), &list); err != nil {
			return fmt.Errorf("must be a JSON array: %w", err)
		}
		anyList := make([]any, len(list))
		for i, m := range list {
			anyList[i] = m
		}
		return k.Set(key, anyList)
	case t.Kind() == reflect.Slice:
		parts := []string{}
		for _, p := range strings.Split(value, ",") {
			if p = strings.TrimSpace(p); p != "" {
				parts = append(parts, p)
			}
		}
		return k.Set(key, parts)
	case t.Kind() == reflect.Map || t.Kind() == reflect.Struct && t != durationType:
		var m map[string]any
		if err := json.Unmarshal([]byte(value), &m); err != nil {
			return fmt.Errorf("must be a JSON object: %w", err)
		}
		return k.Set(key, m)
	default:
		return k.Set(key, value)
	}
}

// splitDecodeError turns mapstructure's joined error into one line per problem.
func splitDecodeError(err error) []string {
	var lines []string
	for _, l := range strings.Split(err.Error(), "\n") {
		l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "*"))
		if l == "" || strings.HasPrefix(l, "decoding failed due to the following error") || strings.HasSuffix(l, "error(s) decoding:") {
			continue
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		lines = []string{err.Error()}
	}
	return lines
}

// toMap converts a config struct to nested maps keyed by koanf tags.
func toMap(v reflect.Value) map[string]any {
	out := map[string]any{}
	t := v.Type()
	for i := range t.NumField() {
		name := t.Field(i).Tag.Get("koanf")
		if name == "" {
			continue
		}
		fv := v.Field(i)
		switch {
		case fv.Kind() == reflect.Struct && fv.Type() != durationType:
			out[name] = toMap(fv)
		case fv.Kind() == reflect.Map && fv.Type().Elem().Kind() == reflect.Struct:
			m := map[string]any{}
			for _, mk := range fv.MapKeys() {
				m[mk.String()] = toMap(fv.MapIndex(mk))
			}
			out[name] = m
		default:
			out[name] = fv.Interface()
		}
	}
	return out
}
