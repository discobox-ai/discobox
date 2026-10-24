package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/adrg/xdg"
)

// A console stays open across creates, so a client.yaml fixed between two of
// them is what the second one reads, not the failure the first one did.
func TestNewSkillsReadsTheFileAsItIsNow(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "client.yaml")
	t.Setenv("DISCOBOX_CLIENT_CONFIG_FILE", configFile)
	if err := os.WriteFile(configFile, []byte("run:\n  skils: [team]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := newSkills(nil, nil); err == nil {
		t.Fatal("newSkills() with a misspelled key = nil error, want one")
	}
	if err := os.WriteFile(configFile, []byte("run:\n  skills: [team]\n  userSkills: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirs, user, err := newSkills(nil, nil)
	if err != nil || !user || !slices.Equal(dirs, []string{filepath.Join(dir, "team")}) {
		t.Fatalf("newSkills() = %v, %v, %v; want the fixed file's team directory and userSkills", dirs, user, err)
	}
	// Given flags replace what the file says.
	no := false
	if dirs, user, err := newSkills([]string{"x"}, &no); err != nil || user || !slices.Equal(dirs, []string{"x"}) {
		t.Fatalf("newSkills(flags) = %v, %v, %v; want the flags as given", dirs, user, err)
	}
	// And a file that does not parse stops a create they would not have
	// needed it for.
	if err := os.WriteFile(configFile, []byte("run:\n  skils: [team]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := newSkills([]string{"x"}, &no); err == nil {
		t.Fatal("newSkills(flags) with a misspelled key = nil error, want one")
	}
}

// The reference is written beside where client.yaml belongs by default, and
// nowhere else: not beside a path the variable names, which is the user's own,
// and not when there is a client.yaml, even one that does not parse.
func TestRefreshClientConfigExample(t *testing.T) {
	config := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", config)
	xdg.Reload()
	t.Cleanup(xdg.Reload)
	dir := filepath.Join(config, "discobox")
	example := filepath.Join(dir, "client.example.yaml")
	written := func() bool {
		_, err := os.Stat(example)
		return err == nil
	}

	refreshClientConfigExample()
	if !written() {
		t.Fatal("no reference written beside a missing client.yaml at the default path")
	}
	if err := os.Remove(example); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "client.yaml"), []byte("not: [valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	refreshClientConfigExample()
	if written() {
		t.Fatal("a reference was written beside a client.yaml that exists")
	}
	if err := os.Remove(filepath.Join(dir, "client.yaml")); err != nil {
		t.Fatal(err)
	}

	named := filepath.Join(t.TempDir(), "typo", "client.yaml")
	for _, value := range []string{named, ""} {
		t.Setenv("DISCOBOX_CLIENT_CONFIG_FILE", value)
		refreshClientConfigExample()
		if written() {
			t.Fatalf("a reference was written with DISCOBOX_CLIENT_CONFIG_FILE=%q", value)
		}
	}
	if _, err := os.Stat(filepath.Dir(named)); err == nil {
		t.Fatal("the directory of a named, missing client.yaml was created")
	}
}
