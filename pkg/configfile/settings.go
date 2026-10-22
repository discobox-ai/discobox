// Package configfile is the engine behind a schema-checked YAML configuration
// file whose struct is its source of truth (ADR 0096): struct tags declare each
// setting, and from the one walk over them come the strict decode, the
// environment overlay, the commented reference file and the JSON Schema. The
// server's server.yaml and the CLI's client.yaml are both read through it.
package configfile

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// A Setting is one configurable field of a configuration struct, described by
// its struct tags (ADR 0096 §2, configuration file). The struct is the source of
// truth: the loader binds from these, and the schema and reference are emitted
// from the same walk, so a field cannot appear in one and not the other.
type Setting struct {
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
	// index is the path to the field within the struct, for reflect
	// FieldByIndex.
	index []int
}

// Settings walks a configuration struct type and returns every bound setting,
// depth first. A field tagged `yaml:"-"` is derived rather than configured and
// is skipped, which is what keeps it out of the file and out of the schema.
func Settings(t reflect.Type) []Setting {
	return appendSettings(nil, t, "", nil)
}

// SettingPaths lists every configurable path of a configuration struct type,
// sorted. It exists for tests that assert the file, the schema and the loader
// describe the same surface.
func SettingPaths(t reflect.Type) []string {
	var paths []string
	for _, s := range Settings(t) {
		paths = append(paths, s.Path)
	}
	sort.Strings(paths)
	return paths
}

func appendSettings(out []Setting, t reflect.Type, prefix string, index []int) []Setting {
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
		s := Setting{
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

// ApplyDefaults writes every literal default into cfg, a pointer to a
// configuration struct. It runs first, so the file and then the environment
// overwrite it (ADR 0096 §4, configuration file).
func ApplyDefaults(cfg any) error {
	value := reflect.ValueOf(cfg).Elem()
	for _, s := range Settings(value.Type()) {
		if s.Default == "" {
			continue
		}
		if err := assign(value.FieldByIndex(s.index), s.Default); err != nil {
			return fmt.Errorf("default for %s: %w", s.Path, err)
		}
	}
	return nil
}

// ApplyEnv overlays the environment onto cfg, a pointer to a configuration
// struct; the environment wins over the file. It returns the paths it set, so a
// computed default knows whether anything claimed the field.
func ApplyEnv(cfg any, lookup func(string) (string, bool)) (map[string]bool, error) {
	value := reflect.ValueOf(cfg).Elem()
	set := map[string]bool{}
	for _, s := range Settings(value.Type()) {
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

// Decode overlays a YAML document read from path onto cfg, a pointer to a
// configuration struct, and reports which paths it set.
//
// Presence comes from the document rather than from the resulting values,
// because a field set to its zero value and a field left out are different
// things and a zero cannot tell them apart (ADR 0096 §4, configuration file).
func Decode(cfg any, data []byte, path string) (map[string]bool, error) {
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
	// (ADR 0096 §3, configuration file).
	decoder.KnownFields(true)
	if err := decoder.Decode(cfg); err != nil {
		// yaml reports unknown keys one per line under a heading, and the
		// key is the part that matters: a console shows an error on one
		// line, and cut at the first newline it names no key at all.
		var typeErr *yaml.TypeError
		if errors.As(err, &typeErr) {
			return nil, fmt.Errorf("%s: %s", path, strings.Join(typeErr.Errors, "; "))
		}
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
		// loader would refuse a line that expressed no opinion.
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
