package configfile

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Schema builds the JSON Schema for a configuration file from the tags on its
// struct type (ADR 0096 §2, configuration file), with the identity a
// `# yaml-language-server: $schema=` modeline points at.
//
// It walks the same settings the loader binds, so a field cannot be loadable
// and undocumented, or documented and unloadable. Objects are closed —
// `additionalProperties: false` — which is what makes an editor flag a
// misspelled key at the same moment loading would.
func Schema(t reflect.Type, id, title, description string) (map[string]any, error) {
	root, err := objectSchema(t, "")
	if err != nil {
		return nil, err
	}
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	root["$id"] = id
	root["title"] = title
	root["description"] = description
	return root, nil
}

// A Schemer is a type that spells its own JSON Schema, for a value no
// struct can describe — one written either as a boolean or as an object.
type Schemer interface {
	ConfigSchema() map[string]any
}

func objectSchema(t reflect.Type, prefix string) (map[string]any, error) {
	properties := map[string]any{}
	var required []any
	for i := range t.NumField() {
		field := t.Field(i)
		// An embedded struct's fields are its embedder's, as encoding/json
		// reads them.
		if field.Anonymous && field.Type.Kind() == reflect.Struct && field.Tag.Get("yaml") == "" && field.Tag.Get("json") == "" {
			embedded, err := objectSchema(field.Type, prefix)
			if err != nil {
				return nil, err
			}
			embeddedProperties, _ := embedded["properties"].(map[string]any)
			for name, property := range embeddedProperties {
				properties[name] = property
			}
			if names, ok := embedded["required"].([]any); ok {
				required = append(required, names...)
			}
			continue
		}
		if !field.IsExported() {
			continue
		}
		name, isRequired := fieldKey(field)
		if name == "" || name == "-" {
			continue
		}
		if isRequired {
			required = append(required, name)
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
			_, yamlKey := field.Tag.Lookup("yaml")
			nested["type"] = nullableType("object", yamlKey)
			properties[name] = nested
			continue
		}
		property, err := valueSchema(field)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		properties[name] = property
	}
	out := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out, nil
}

// fieldKey is the key a field is written under, and whether it is required.
// A YAML configuration file names it in its yaml tag; a JSON file, read with
// encoding/json, in its json tag, where a field without omitempty is one
// every value writes.
func fieldKey(field reflect.StructField) (string, bool) {
	if tag, ok := field.Tag.Lookup("yaml"); ok {
		name, _, _ := strings.Cut(tag, ",")
		return name, false
	}
	name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
	if name == "" || name == "-" {
		return "", false
	}
	return name, !strings.Contains(","+options+",", ",omitempty,")
}

// elementSchema is the schema of a map's values or a list's items.
func elementSchema(t reflect.Type, prefix string) (map[string]any, error) {
	if schemer, ok := reflect.New(t).Elem().Interface().(Schemer); ok {
		return schemer.ConfigSchema(), nil
	}
	switch t.Kind() {
	case reflect.Struct:
		return objectSchema(t, prefix)
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int64:
		return map[string]any{"type": "integer"}, nil
	}
	return nil, fmt.Errorf("unsupported element type %s", t)
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
	// An editor offers these as completions, which is where an operator who
	// does not know a setting's shape is already looking.
	if example := ExampleTag(field); example != "" {
		var value any
		if err := yaml.Unmarshal([]byte(example), &value); err != nil {
			return nil, fmt.Errorf("example %q is not YAML: %w", example, err)
		}
		out["examples"] = []any{value}
	}
	// The reference file renders some settings as a bare key, which is YAML
	// null and which the loader reads as saying nothing. The schema has to
	// accept null for exactly those, or an editor flags the very lines the
	// generated reference tells an operator to uncomment. Both artifacts ask
	// the same function so they cannot drift.
	// A JSON file has no reference to uncomment, so none of its keys does.
	_, yamlKey := field.Tag.Lookup("yaml")
	nullable := yamlKey && rendersAsBareKey(field)
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

	switch value := reflect.New(field.Type).Elem().Interface().(type) {
	case Schemer:
		for key, keyword := range value.ConfigSchema() {
			out[key] = keyword
		}
		return out, nil
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
	case reflect.Map:
		if field.Type.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("unsupported setting type %s", field.Type)
		}
		values, err := elementSchema(field.Type.Elem(), "")
		if err != nil {
			return nil, err
		}
		out["type"] = nullableType("object", nullable)
		out["additionalProperties"] = values
	case reflect.Slice:
		items, err := elementSchema(field.Type.Elem(), "")
		if err != nil {
			return nil, err
		}
		out["type"] = nullableType("array", nullable)
		out["items"] = items
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
