package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A console stays open across creates, so a client.yaml fixed between two of
// them is what the second one reads, not the failure the first one did.
func TestNewSkillsReadsTheFileAsItIsNow(t *testing.T) {
	dir := t.TempDir()
	configFile := filepath.Join(dir, "client.yaml")
	t.Setenv("DISCOBOX_CLIENT_CONFIG_FILE", configFile)
	if err := os.WriteFile(configFile, []byte("new:\n  skils: [team]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := newSkills(nil, nil); err == nil {
		t.Fatal("newSkills() with a misspelled key = nil error, want one")
	}
	if err := os.WriteFile(configFile, []byte("new:\n  skills: [team]\n  userSkills: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dirs, user, err := newSkills(nil, nil)
	if err != nil || !user || !slices.Equal(dirs, []string{filepath.Join(dir, "team")}) {
		t.Fatalf("newSkills() = %v, %v, %v; want the fixed file's team directory and userSkills", dirs, user, err)
	}
	// Given flags are what they say, whatever the file does.
	no := false
	if dirs, user, err := newSkills([]string{"x"}, &no); err != nil || user || !slices.Equal(dirs, []string{"x"}) {
		t.Fatalf("newSkills(flags) = %v, %v, %v; want the flags as given", dirs, user, err)
	}
}
