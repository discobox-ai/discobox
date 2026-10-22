package configfile

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// Example renders the commented reference form of a configuration file: header,
// then every setting of t, commented out, at the value it has when nothing sets
// it.
//
// It is generated rather than written because a hand-kept reference is a
// reference that is missing the setting somebody added last week, and the
// whole claim of ADR 0096 (configuration file) is that the file holds all
// valid configuration.
func Example(header string, t reflect.Type) ([]byte, error) {
	var out bytes.Buffer
	out.WriteString(header)
	if err := writeExampleFields(&out, t, ""); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// WriteExample writes body to path, the reference file beside where a
// configuration file belongs.
//
// An unchanged reference is left untouched, and a changed one is replaced by
// rename, so two processes writing it at once cannot leave half a file.
func WriteExample(path string, body []byte) error {
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, body) {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: the user's own configuration directory, which holds nothing secret until they put it there.
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
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
		// Unwrapped, unlike the prose around it, so the line can be copied
		// whole: a wrapped value is a value that no longer parses.
		if example := ExampleTag(field); example != "" {
			fmt.Fprintf(out, "%s# Example: %s: %s\n", yamlIndent, name, example)
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

// ExampleTag is a setting's example value, written as the YAML an operator
// would put after its key, or empty for a setting that has none. "-" says a
// setting has none on purpose — a value that must be unique to each
// deployment, such as a key, is one an example would get copied verbatim — and
// its doc says how to make one instead.
func ExampleTag(field reflect.StructField) string {
	example := strings.TrimSpace(field.Tag.Get("example"))
	if example == "-" {
		return ""
	}
	return example
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
// uncommented and still configure exactly what no file does.
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
