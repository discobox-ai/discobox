package meta

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/sandboxmeta"
)

func TestReadMissingFileIsEmptyMeta(t *testing.T) {
	got, observedAt, err := New(t.TempDir(), Owner{}).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got.Description != "" || got.Tags == nil || len(got.Tags) != 0 {
		t.Fatalf("Read() = %#v, want empty meta", got)
	}
	if observedAt.IsZero() {
		t.Fatal("Read() observedAt is zero")
	}
}

func TestReadWhatTheSandboxWrote(t *testing.T) {
	home := t.TempDir()
	writeMetaFile(t, home, `{"description": "fix the reaper", "tags": {"wip": "", "ticket": "ENG-12"}}`)
	got, _, err := New(home, Owner{}).Read()
	if err != nil {
		t.Fatal(err)
	}
	want := sandboxmeta.Meta{Description: "fix the reaper", Tags: map[string]string{"wip": "", "ticket": "ENG-12"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Read() = %v, want %v", got, want)
	}
}

func TestReadInvalidFile(t *testing.T) {
	home := t.TempDir()
	writeMetaFile(t, home, `{"tags": ["wip"]}`)
	if _, _, err := New(home, Owner{}).Read(); !errors.Is(err, ErrInvalidFile) {
		t.Fatalf("Read() error = %v, want ErrInvalidFile", err)
	}
}

func TestReadRefusesAFileThatIsNotRegular(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, sandboxmeta.RelativePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := New(home, Owner{}).Read(); !errors.Is(err, ErrInvalidFile) {
		t.Fatalf("Read() error = %v, want ErrInvalidFile", err)
	}
}

// An update keeps what the sandbox wrote and the change does not name, and it
// creates ~/.discobox when the sandbox never has.
func TestUpdateMergesIntoTheFile(t *testing.T) {
	home := t.TempDir()
	file := New(home, Owner{})
	description := "fix the reaper"
	if _, _, err := file.Update(sandboxmeta.Change{Description: &description, SetTags: map[string]string{"wip": ""}}); err != nil {
		t.Fatalf("first Update() error = %v", err)
	}
	got, observedAt, err := file.Update(sandboxmeta.Change{SetTags: map[string]string{"ticket": "ENG-12"}, RemoveTags: []string{"absent"}})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	want := sandboxmeta.Meta{Description: "fix the reaper", Tags: map[string]string{"wip": "", "ticket": "ENG-12"}}
	if !reflect.DeepEqual(got, want) || observedAt.IsZero() {
		t.Fatalf("Update() = %v at %v, want %v", got, observedAt, want)
	}
	data, err := os.ReadFile(file.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{\n  \"description\": \"fix the reaper\",\n  \"tags\": {\n    \"ticket\": \"ENG-12\",\n    \"wip\": \"\"\n  }\n}\n" {
		t.Fatalf("file = %q", data)
	}
	info, err := os.Stat(file.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("file mode = %v, want 0644", info.Mode().Perm())
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(file.Path()), ".meta-*")); len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v", leftovers)
	}
}

func TestUpdateRefusesAnInvalidChange(t *testing.T) {
	file := New(t.TempDir(), Owner{})
	if _, _, err := file.Update(sandboxmeta.Change{SetTags: map[string]string{"bad key": ""}}); !errors.Is(err, ErrInvalidChange) {
		t.Fatalf("Update() with an invalid key error = %v, want ErrInvalidChange", err)
	}
	if _, err := os.Stat(file.Path()); !os.IsNotExist(err) {
		t.Fatalf("a refused change wrote the file: %v", err)
	}
}

// A file somebody wrote by hand and got wrong is not overwritten: the change
// did not name what they meant, and replacing it would lose it.
func TestUpdateRefusesToReplaceAnInvalidFile(t *testing.T) {
	home := t.TempDir()
	writeMetaFile(t, home, `{"tags": {"note": 1}}`)
	_, _, err := New(home, Owner{}).Update(sandboxmeta.Change{SetTags: map[string]string{"wip": ""}})
	if !errors.Is(err, ErrInvalidFile) {
		t.Fatalf("Update() error = %v, want ErrInvalidFile", err)
	}
	data, _ := os.ReadFile(filepath.Join(home, sandboxmeta.RelativePath))
	if !strings.Contains(string(data), `"note": 1`) {
		t.Fatalf("the invalid file was replaced: %q", data)
	}
}

// The description a sandbox was created with seeds the file once. After that
// the file is the description, including when it has been emptied.
func TestSeedWritesOnlyWhenThereIsNoFile(t *testing.T) {
	home := t.TempDir()
	file := New(home, Owner{})
	if err := file.Seed(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file.Path()); !os.IsNotExist(err) {
		t.Fatal("an empty seed wrote a file")
	}
	if err := file.Seed("created to fix the reaper"); err != nil {
		t.Fatal(err)
	}
	got, _, err := file.Read()
	if err != nil || got.Description != "created to fix the reaper" {
		t.Fatalf("Read() after Seed() = %v, %v", got, err)
	}
	cleared := ""
	if _, _, err := file.Update(sandboxmeta.Change{Description: &cleared}); err != nil {
		t.Fatal(err)
	}
	if err := file.Seed("created to fix the reaper"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := file.Read(); got.Description != "" {
		t.Fatalf("Seed() overwrote a description the sandbox cleared: %q", got.Description)
	}
}

func writeMetaFile(t *testing.T, home, content string) {
	t.Helper()
	path := filepath.Join(home, sandboxmeta.RelativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
