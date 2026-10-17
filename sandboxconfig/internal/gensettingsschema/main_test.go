package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/sandboxconfig"
)

// The schema describes every key Settings reads, and the checked-in copy is
// what an editor checks a repository's file against.
func TestSettingsSchemaIsCurrent(t *testing.T) {
	built, err := schema()
	if err != nil {
		t.Fatal(err)
	}
	full, err := json.Marshal(sandboxconfig.Settings{
		Schema: "s", Sources: map[string]string{"a": "b"}, Features: map[string]sandboxconfig.Feature{"a": {}},
		ProjectLayer: sandboxconfig.ProjectLayer{RunCommand: []string{"a"}, RelaunchCommand: []string{"a"}, WorkingDirectorySubpath: "a", FilesAdd: []sandboxconfig.File{{}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]any
	if err := json.Unmarshal(full, &keys); err != nil {
		t.Fatal(err)
	}
	properties, _ := built["properties"].(map[string]any)
	if len(properties) != len(keys) {
		t.Fatalf("schema properties %v, want Settings' keys %v", properties, keys)
	}
	for key := range keys {
		if _, ok := properties[key]; !ok {
			t.Fatalf("schema has no %q", key)
		}
	}
	want, err := schemaJSON()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("..", "..", "..", "settings.schema.json")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s is stale; run: go tool task generate", path)
	}
}
