package config

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SchemaID is where the generated schema is published, and what a
// `# yaml-language-server: $schema=` modeline points at.
const SchemaID = "https://raw.githubusercontent.com/discobox-ai/discobox/main/server/config.schema.json"

// Schema builds the JSON Schema for the configuration file from the tags on
// Config (ADR 0096 §2).
//
// It walks the same settings the loader binds, so a field cannot be loadable
// and undocumented, or documented and unloadable. Objects are closed —
// `additionalProperties: false` — which is what makes an editor flag a
// misspelled key at the same moment startup would.
func Schema() (map[string]any, error) {
	root, err := objectSchema(reflect.TypeOf(Config{}), "")
	if err != nil {
		return nil, err
	}
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["$id"] = SchemaID
	root["title"] = "Discobox server configuration"
	root["description"] = "Configuration for discobox-server. Every setting may also be set by the environment variable named in its description, which takes precedence over this file (ADR 0096)."
	return root, nil
}

func objectSchema(t reflect.Type, prefix string) (map[string]any, error) {
	properties := map[string]any{}
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		if field.Type.Kind() == reflect.Struct && field.Type != reflect.TypeOf(time.Time{}) {
			nested, err := objectSchema(field.Type, path)
			if err != nil {
				return nil, err
			}
			nested["description"] = field.Tag.Get("doc")
			// A group is written by the reference as a bare key too — that is
			// how its children stay commented — so it needs null for the same
			// reason every default-less scalar does.
			nested["type"] = nullableType("object", true)
			properties[name] = nested
			continue
		}
		property, err := valueSchema(field)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		properties[name] = property
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
	}, nil
}

func valueSchema(field reflect.StructField) (map[string]any, error) {
	out := map[string]any{}
	description := strings.TrimSpace(field.Tag.Get("doc"))
	// The environment variable belongs in the description rather than in a
	// custom keyword: an editor renders the description on hover, and the
	// operator's next question after "what is this" is "how else do I set it".
	if env := field.Tag.Get("env"); env != "" {
		if description != "" {
			description += " "
		}
		description += "Environment: " + env + " (takes precedence over this file)."
	}
	if description != "" {
		out["description"] = description
	}
	// The reference file renders some settings as a bare key, which is YAML
	// null and which the loader reads as saying nothing. The schema has to
	// accept null for exactly those, or an editor flags the very lines the
	// generated reference tells an operator to uncomment. Both artifacts ask
	// the same function so they cannot drift.
	nullable := rendersAsBareKey(field)
	if enum := field.Tag.Get("enum"); enum != "" {
		values := strings.Split(enum, ",")
		anyValues := make([]any, 0, len(values)+1)
		for _, value := range values {
			anyValues = append(anyValues, value)
		}
		if nullable {
			anyValues = append(anyValues, nil)
		}
		out["enum"] = anyValues
	}

	switch reflect.New(field.Type).Elem().Interface().(type) {
	case time.Duration:
		// Durations are Go duration strings. Spelling them as a string with a
		// pattern gives an editor something to check; spelling them as an
		// integer would silently mean nanoseconds.
		out["type"] = nullableType("string", nullable)
		out["pattern"] = `^\d+(\.\d+)?(ns|us|ms|s|m|h)([0-9.]+(ns|us|ms|s|m|h))*$`
		if _, ok := out["description"]; !ok {
			out["description"] = "A Go duration, such as 30s or 24h."
		}
		if def := field.Tag.Get("default"); def != "" {
			out["default"] = def
		}
		return out, nil
	case []string:
		out["type"] = nullableType("array", nullable)
		out["items"] = map[string]any{"type": "string"}
		if def := field.Tag.Get("default"); def != "" {
			out["default"] = splitList(def)
		}
		return out, nil
	}

	def := field.Tag.Get("default")
	switch field.Type.Kind() {
	case reflect.String:
		out["type"] = nullableType("string", nullable)
		if def != "" {
			out["default"] = def
		}
	case reflect.Int, reflect.Int64:
		out["type"] = nullableType("integer", nullable)
		if def != "" {
			parsed, err := strconv.Atoi(def)
			if err != nil {
				return nil, fmt.Errorf("default %q is not a number", def)
			}
			out["default"] = parsed
		}
	case reflect.Bool:
		out["type"] = nullableType("boolean", nullable)
		if def != "" {
			parsed, err := strconv.ParseBool(def)
			if err != nil {
				return nil, fmt.Errorf("default %q is not a boolean", def)
			}
			out["default"] = parsed
		}
	default:
		return nil, fmt.Errorf("unsupported setting type %s", field.Type)
	}
	return out, nil
}

// nullableType renders a property's type, admitting null for a setting the
// reference file writes as a bare key.
func nullableType(name string, nullable bool) any {
	if !nullable {
		return name
	}
	return []any{name, "null"}
}

// SettingPaths lists every configurable path, sorted. It exists for tests that
// assert the file, the schema and the loader describe the same surface.
func SettingPaths() []string {
	var paths []string
	for _, s := range settings(reflect.TypeOf(Config{})) {
		paths = append(paths, s.Path)
	}
	sort.Strings(paths)
	return paths
}

// SettingEnv returns the environment variable that overrides a path, and
// whether it has one.
func SettingEnv(path string) (string, bool) {
	for _, s := range settings(reflect.TypeOf(Config{})) {
		if s.Path == path {
			return s.Env, s.Env != ""
		}
	}
	return "", false
}
