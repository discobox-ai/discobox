package config

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// exampleHeader introduces the generated reference file.
const exampleHeader = `# yaml-language-server: $schema=` + SchemaID + `
#
# discobox-server configuration reference.
#
# Every setting is listed here, commented out, showing the value it has when
# nothing sets it. Copy this to the path the server reads —
# <XDG config home>/discobox/server.yaml, or wherever DISCOBOX_CONFIG_FILE
# names — and uncomment only what you want to change: deleting the "#" from a
# setting's line leaves correctly indented YAML, at any depth. A nested setting
# needs its parent uncommented too, which is the one place a single deletion is
# not enough. A server with no file at all behaves exactly as one always has.
#
# Every setting can also be set by the environment variable named beside it,
# and the environment wins over this file (ADR 0096).
#
# A key this server does not define is a startup failure naming the key. That
# is the point of the file: a misspelled environment variable is silently the
# default instead.
#
# This file is generated from the tags on config.Config by
# ` + "`go tool task generate`" + `. Edit the struct, not this.
`

// ExampleYAML renders the commented reference form of the configuration file.
//
// It is generated rather than written because a hand-kept reference is a
// reference that is missing the setting somebody added last week, and the
// whole claim of ADR 0096 (configuration file) is that the file holds all valid configuration.
func ExampleYAML() ([]byte, error) {
	var out bytes.Buffer
	out.WriteString(exampleHeader)
	if err := writeExampleFields(&out, reflect.TypeOf(Config{}), ""); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// writeExampleFields renders one struct level.
//
// Indentation goes *outside* the comment marker — `  #relayUrls:`, not
// `#  relayUrls:` — so that deleting the `#` leaves a correctly indented line
// at any depth, and so that a nested setting is still distinguishable from
// prose. With the indent inside, `#  relayUrls:` and a wrapped comment line
// are the same shape, and neither a reader nor a parser can tell them apart.
//
// Prose is `# text` at the same indent; a setting is `#key: value`. That is
// postgresql.conf's convention, extended to nesting.
func writeExampleFields(out *bytes.Buffer, t reflect.Type, yamlIndent string) error {
	first := true
	for i := range t.NumField() {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		if !first || yamlIndent == "" {
			out.WriteString("\n")
		}
		first = false
		if doc := strings.TrimSpace(field.Tag.Get("doc")); doc != "" {
			writeComment(out, yamlIndent, doc)
		}
		if env := field.Tag.Get("env"); env != "" {
			writeComment(out, yamlIndent, "Environment: "+env)
		}
		if field.Type.Kind() == reflect.Struct && field.Type != reflect.TypeOf(time.Time{}) {
			fmt.Fprintf(out, "%s#%s:\n", yamlIndent, name)
			if err := writeExampleFields(out, field.Type, yamlIndent+"  "); err != nil {
				return err
			}
			continue
		}
		value, err := exampleValue(field)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		fmt.Fprintf(out, "%s#%s:%s\n", yamlIndent, name, value)
	}
	return nil
}

// writeComment wraps text into comment lines that stay inside a narrow
// terminal, which is where an operator reads this.
func writeComment(out *bytes.Buffer, yamlIndent, text string) {
	const width = 74
	prefix := yamlIndent + "# "
	for _, paragraph := range strings.Split(text, "\n") {
		line := prefix
		for _, word := range strings.Fields(paragraph) {
			if len(line)+1+len(word) > width && line != prefix {
				out.WriteString(strings.TrimRight(line, " "))
				out.WriteByte('\n')
				line = prefix
			}
			line += word + " "
		}
		if trimmed := strings.TrimRight(line, " "); trimmed != strings.TrimRight(prefix, " ") {
			out.WriteString(trimmed)
			out.WriteByte('\n')
		}
	}
}

// exampleValue renders what a setting holds when nothing sets it: the literal
// default where there is one, and the type's empty value otherwise.
//
// A setting whose default is computed — the directories, the database DSN —
// has no literal to show, and showing a value derived on this machine would
// check somebody's home directory into the repository. Its doc says what it
// derives from instead.
//
// "Unset" is rendered as a bare key rather than as an empty value, because an
// empty value is not what unset means and often does not even parse:
// `archiveRetention: ""` is not a duration, and `dataDir: ""` would claim the
// operator chose the empty string. A key with nothing after it is YAML null,
// which the loader reads as saying nothing — so the whole reference can be
// uncommented and still start a server.
func exampleValue(field reflect.StructField) (string, error) {
	def := strings.TrimSpace(field.Tag.Get("default"))
	if rendersAsBareKey(field) {
		return "", nil
	}
	switch reflect.New(field.Type).Elem().Interface().(type) {
	case time.Duration:
		if def != "" {
			return " " + def, nil
		}
		return "", nil
	case []string:
		if def == "" {
			return "", nil
		}
		var items []string
		for _, item := range splitList(def) {
			items = append(items, strconvQuote(item))
		}
		return " [" + strings.Join(items, ", ") + "]", nil
	}
	switch field.Type.Kind() {
	case reflect.String:
		if def != "" {
			return " " + strconvQuote(def), nil
		}
		return "", nil
	case reflect.Int, reflect.Int64:
		if def != "" {
			return " " + def, nil
		}
		return "", nil
	case reflect.Bool:
		if def != "" {
			return " " + def, nil
		}
		return " false", nil
	default:
		return "", fmt.Errorf("unsupported setting type %s", field.Type)
	}
}

// strconvQuote quotes a YAML scalar. Windows paths are the reason: an
// unquoted `C:\wslc.exe` is a different string than the one an operator typed.
func strconvQuote(value string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`) + `"`
}

// rendersAsBareKey reports whether the reference file writes a setting as a
// key with nothing after it — YAML null, which the loader reads as saying
// nothing.
//
// That is every setting with no literal default, except booleans: false is a
// real and complete answer for one, so there is nothing to leave out. The
// schema generator asks this too, so what the reference writes and what the
// schema permits cannot disagree.
func rendersAsBareKey(field reflect.StructField) bool {
	if strings.TrimSpace(field.Tag.Get("default")) != "" {
		return false
	}
	return field.Type.Kind() != reflect.Bool
}
