package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// A setting is one configurable field of Config, described by its struct tags
// (ADR 0096 §2). The struct is the source of truth: the loader binds from
// these, and genschema emits the JSON Schema from the same walk, so a field
// cannot appear in one and not the other.
type setting struct {
	// Path is the dotted YAML path, "dataDir" or "iroh.relayUrls".
	Path string
	// Env is the environment variable that overrides it, empty for a setting
	// the environment cannot reach.
	Env string
	// Doc is the schema description and the operator-facing explanation.
	Doc string
	// Default is the literal default, rendered into the schema. A setting
	// whose default is computed from another setting has none here and
	// documents the derivation in Doc instead.
	Default string
	// Enum constrains the accepted values.
	Enum []string
	// Field is the reflect.StructField, for its type.
	Field reflect.StructField
	// index is the path to the field within Config, for reflect FieldByIndex.
	index []int
}

// settings walks a Config type and returns every bound setting, depth first.
// A field tagged `yaml:"-"` is derived rather than configured and is skipped,
// which is what keeps HostID and DevelopmentImages out of the file and out of
// the schema.
func settings(t reflect.Type) []setting {
	return appendSettings(nil, t, "", nil)
}

func appendSettings(out []setting, t reflect.Type, prefix string, index []int) []setting {
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		at := append(append([]int{}, index...), i)
		if field.Type.Kind() == reflect.Struct && field.Type != reflect.TypeOf(time.Time{}) {
			out = appendSettings(out, field.Type, path, at)
			continue
		}
		s := setting{
			Path:    path,
			Env:     field.Tag.Get("env"),
			Doc:     field.Tag.Get("doc"),
			Default: field.Tag.Get("default"),
			Field:   field,
			index:   at,
		}
		if enum := field.Tag.Get("enum"); enum != "" {
			s.Enum = strings.Split(enum, ",")
		}
		out = append(out, s)
	}
	return out
}

// applyDefaults writes every literal default into cfg. It runs first, so the
// file and then the environment overwrite it (ADR 0096 §4).
func applyDefaults(cfg *Config) error {
	value := reflect.ValueOf(cfg).Elem()
	for _, s := range settings(value.Type()) {
		if s.Default == "" {
			continue
		}
		if err := assign(value.FieldByIndex(s.index), s.Default); err != nil {
			return fmt.Errorf("default for %s: %w", s.Path, err)
		}
	}
	return nil
}

// applyEnv overlays the environment, which wins over the file. It returns the
// paths it set, so a computed default knows whether anything claimed the field.
func applyEnv(cfg *Config, lookup func(string) (string, bool)) (map[string]bool, error) {
	value := reflect.ValueOf(cfg).Elem()
	set := map[string]bool{}
	for _, s := range settings(value.Type()) {
		if s.Env == "" {
			continue
		}
		raw, ok := lookup(s.Env)
		if !ok {
			continue
		}
		// An empty variable means "unset" rather than "set to empty", which is
		// how the environment has always behaved here: getEnv fell back to the
		// default on a blank value, and a caller who exports a variable to the
		// empty string in a shell script means to say nothing.
		if strings.TrimSpace(raw) == "" {
			continue
		}
		if err := assign(value.FieldByIndex(s.index), raw); err != nil {
			return nil, fmt.Errorf("%s: %w", s.Env, err)
		}
		set[s.Path] = true
	}
	return set, nil
}

// decodeFile overlays a YAML document onto cfg and reports which paths it set.
//
// Presence comes from the document rather than from the resulting values,
// because a field set to its zero value and a field left out are different
// things and a zero cannot tell them apart (ADR 0096 §4).
func decodeFile(cfg *Config, data []byte, path string) (map[string]bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if doc.Kind == 0 || len(doc.Content) == 0 {
		return map[string]bool{}, nil // an empty file configures nothing
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	// The whole reason the file earns its place over the environment: a key
	// nothing defines is a failure naming the key, not a silent default
	// (ADR 0096 §3).
	decoder.KnownFields(true)
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return presentPaths(doc.Content[0], ""), nil
}

// presentPaths collects the dotted paths a mapping node actually contains.
func presentPaths(node *yaml.Node, prefix string) map[string]bool {
	out := map[string]bool{}
	if node == nil || node.Kind != yaml.MappingNode {
		return out
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		value := node.Content[i+1]
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		// A key written with nothing after it says nothing. Treating it as
		// configured would make the generated reference unusable: uncommenting
		// `dataDir:` would claim the operator chose the empty string, and the
		// server would refuse to start over a line that expressed no opinion.
		if !isNull(value) {
			out[path] = true
		}
		for nested := range presentPaths(value, path) {
			out[nested] = true
		}
	}
	return out
}

// assign parses raw into field, in the small set of types a setting may have.
func assign(field reflect.Value, raw string) error {
	raw = strings.TrimSpace(raw)
	switch field.Interface().(type) {
	case time.Duration:
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("%q is not a duration", raw)
		}
		field.SetInt(int64(parsed))
		return nil
	case []string:
		field.Set(reflect.ValueOf(splitList(raw)))
		return nil
	}
	switch field.Kind() {
	case reflect.String:
		field.SetString(raw)
	case reflect.Int, reflect.Int64:
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return fmt.Errorf("%q is not a number", raw)
		}
		field.SetInt(int64(parsed))
	case reflect.Bool:
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("%q is not a boolean", raw)
		}
		field.SetBool(parsed)
	default:
		return fmt.Errorf("unsupported setting type %s", field.Type())
	}
	return nil
}

// splitList parses the comma-separated form a list setting takes in the
// environment. The file spells the same setting as a YAML sequence.
func splitList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// isNull reports whether a node is YAML's null: an empty value, "~", or an
// explicit null.
func isNull(node *yaml.Node) bool {
	return node == nil || node.Tag == "!!null"
}
