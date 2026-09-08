package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// The generated schema and the loader describe the same surface. If they can
// drift, "all valid configuration is in the file" is a claim nobody checks
// (ADR 0096 §2).
func TestSchemaCoversEverySetting(t *testing.T) {
	schema, err := Schema()
	if err != nil {
		t.Fatalf("Schema() error = %v", err)
	}
	for _, path := range SettingPaths() {
		properties, _ := schema["properties"].(map[string]any)
		node := any(map[string]any{"properties": properties})
		for _, segment := range strings.Split(path, ".") {
			object, ok := node.(map[string]any)
			if !ok {
				t.Fatalf("%s: schema has no object at this level", path)
			}
			props, ok := object["properties"].(map[string]any)
			if !ok {
				t.Fatalf("%s: schema node has no properties", path)
			}
			node, ok = props[segment]
			if !ok {
				t.Fatalf("%s: not in the schema; regenerate server/config.schema.json", path)
			}
		}
	}
}

// Closed objects are what make an editor flag a misspelled key at the moment
// the operator types it, rather than at the moment the server refuses to start.
func TestSchemaObjectsAreClosed(t *testing.T) {
	schema, err := Schema()
	if err != nil {
		t.Fatalf("Schema() error = %v", err)
	}
	var walk func(node map[string]any, path string)
	walk = func(node map[string]any, path string) {
		if node["type"] != "object" {
			return
		}
		if node["additionalProperties"] != false {
			t.Fatalf("%s allows additional properties; an unknown key there would not be caught", path)
		}
		properties, _ := node["properties"].(map[string]any)
		for name, child := range properties {
			if object, ok := child.(map[string]any); ok {
				walk(object, path+"."+name)
			}
		}
	}
	walk(schema, "(root)")
}

// Every setting the environment can reach says so where an operator will read
// it, because "how else do I set this" is the next question after "what is it".
func TestSchemaDescribesTheEnvironmentVariable(t *testing.T) {
	schema, err := Schema()
	if err != nil {
		t.Fatalf("Schema() error = %v", err)
	}
	properties, _ := schema["properties"].(map[string]any)
	for _, path := range SettingPaths() {
		env, ok := SettingEnv(path)
		if !ok || strings.Contains(path, ".") {
			continue
		}
		property, _ := properties[path].(map[string]any)
		description, _ := property["description"].(string)
		if !strings.Contains(description, env) {
			t.Fatalf("%s does not name %s in its description", path, env)
		}
	}
}

// The checked-in schema is what an editor fetches, so a stale one is a schema
// that describes a server nobody is running. task verify enforces this too;
// this reports it as a test failure naming the command that fixes it.
func TestCheckedInSchemaIsCurrent(t *testing.T) {
	schema, err := Schema()
	if err != nil {
		t.Fatalf("Schema() error = %v", err)
	}
	want, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join("..", "..", "config.schema.json")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !reflect.DeepEqual(strings.TrimSpace(string(got)), strings.TrimSpace(string(want))) {
		t.Fatalf("%s is stale; run: go tool task generate", path)
	}
}

// The reference file lists every setting. A reference missing the setting
// somebody added last week is worse than none, because it is read as complete.
func TestExampleListsEverySetting(t *testing.T) {
	body, err := ExampleYAML()
	if err != nil {
		t.Fatalf("ExampleYAML() error = %v", err)
	}
	text := string(body)
	for _, path := range SettingPaths() {
		segments := strings.Split(path, ".")
		key := segments[len(segments)-1]
		if !strings.Contains(text, "#"+key+":") && !strings.Contains(text, "  "+key+":") {
			t.Fatalf("%s is missing from the reference file; run: go tool task generate", path)
		}
		if env, ok := SettingEnv(path); ok && !strings.Contains(text, "Environment: "+env) {
			t.Fatalf("%s does not name %s in the reference file", path, env)
		}
	}
}

// Uncommenting a setting has to leave a file this server can load, which is
// the only reason the reference is worth shipping.
func TestExampleParsesOnceUncommented(t *testing.T) {
	body, err := ExampleYAML()
	if err != nil {
		t.Fatalf("ExampleYAML() error = %v", err)
	}
	// Delete the leading "#" of every setting line — prose keeps its "# " and
	// stays a comment — which is exactly what an operator does by hand.
	var uncommented []string
	for _, line := range strings.Split(string(body), "\n") {
		indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
		trimmed := strings.TrimLeft(line, " ")
		if strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "# ") {
			line = indent + strings.TrimPrefix(trimmed, "#")
		}
		uncommented = append(uncommented, line)
	}
	// Not just parseable: a server has to start on it. Every setting rendered
	// as a bare key must read as "nothing was said", or the reference would be
	// a file that breaks the server of anyone who uncommented it wholesale.
	clearConfigEnv(t)
	path := filepath.Join(t.TempDir(), "server.yaml")
	if err := os.WriteFile(path, []byte(strings.Join(uncommented, "\n")), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv(ConfigFileVar, path)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("the fully uncommented reference does not load: %v", err)
	}
	// And it has to mean the same thing as no file at all.
	t.Setenv(ConfigFileVar, "")
	bare, err := Load()
	if err != nil {
		t.Fatalf("Load() with no file: %v", err)
	}
	if !reflect.DeepEqual(cfg, bare) {
		t.Fatalf("the uncommented reference configures a different server than no file:\n got %+v\nwant %+v", cfg, bare)
	}
}

// The checked-in reference is what a reader copies, so a stale one hands them
// a file that does not match this server.
func TestCheckedInExampleIsCurrent(t *testing.T) {
	body, err := ExampleYAML()
	if err != nil {
		t.Fatalf("ExampleYAML() error = %v", err)
	}
	path := filepath.Join("..", "..", "server.example.yaml")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != string(body) {
		t.Fatalf("%s is stale; run: go tool task generate", path)
	}
}

// The two generated artifacts have to agree about every setting.
//
// The reference tells an operator to uncomment a line; the schema, which the
// reference's own modeline points their editor at, decides whether that line
// is valid. Nothing checked that they matched, and they did not: the reference
// wrote bare keys for every default-less setting while the schema typed them
// all as scalars, so an editor flagged the exact lines the file invited.
func TestSchemaAcceptsWhatTheReferenceWrites(t *testing.T) {
	schema, err := Schema()
	if err != nil {
		t.Fatalf("Schema() error = %v", err)
	}
	body, err := ExampleYAML()
	if err != nil {
		t.Fatalf("ExampleYAML() error = %v", err)
	}
	rendered := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if !strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "# ") {
			continue
		}
		key, value, _ := strings.Cut(strings.TrimPrefix(trimmed, "#"), ":")
		rendered[strings.TrimSpace(key)] = strings.TrimSpace(value) == ""
	}
	if len(rendered) == 0 {
		t.Fatal("no settings were parsed out of the reference file")
	}

	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		properties, _ := node["properties"].(map[string]any)
		for name, raw := range properties {
			property, _ := raw.(map[string]any)
			bare, listed := rendered[name]
			if !listed {
				t.Fatalf("%s is in the schema but not in the reference file", name)
			}
			types, isUnion := property["type"].([]any)
			acceptsNull := false
			for _, candidate := range types {
				acceptsNull = acceptsNull || candidate == "null"
			}
			switch {
			case bare && (!isUnion || !acceptsNull):
				t.Fatalf("the reference writes %q as a bare key (null) but the schema types it %v", name, property["type"])
			case !bare && isUnion:
				t.Fatalf("the reference writes a value for %q but the schema also admits null (%v)", name, property["type"])
			}
			// A group is checked like any other property first — it is
			// written as a bare key too — and then descended into.
			if _, nested := property["properties"]; nested {
				walk(property)
			}
		}
	}
	walk(schema)
}
