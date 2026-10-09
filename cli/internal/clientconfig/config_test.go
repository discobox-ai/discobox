package clientconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/adrg/xdg"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(FileVar, path)
	return path
}

func TestLoadReadsTheFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path := writeConfig(t, "new:\n  skills: [team, ~/mine, "+strings.ReplaceAll(filepath.Join(home, "abs"), `\`, `\\`)+"]\n  userSkills: true\n")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	want := []string{filepath.Join(filepath.Dir(path), "team"), filepath.Join(home, "mine"), filepath.Join(home, "abs")}
	if !cfg.Read || cfg.Path != path || !cfg.New.UserSkills || !slices.Equal(cfg.New.Skills, want) {
		t.Fatalf("Load() = %+v, want %s read with skills %v and userSkills", cfg, path, want)
	}
}

// A misspelling is an error naming the key, not a setting silently left at its
// default (ADR 0096 §3, configuration file).
func TestLoadRefusesAKeyNothingDefines(t *testing.T) {
	path := writeConfig(t, "new:\n  userSkils: true\n")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "userSkils") || !strings.Contains(err.Error(), path) {
		t.Fatalf("Load() error = %v, want one naming userSkils and %s", err, path)
	}
}

func TestLoadRefusesAnEmptySkillsDirectory(t *testing.T) {
	writeConfig(t, "new:\n  skills: [\"\"]\n")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "new.skills[0]") {
		t.Fatalf("Load() error = %v, want one naming new.skills[0]", err)
	}
}

// No file at the default path is a client configured by nothing; a path named
// on purpose that is not there is a mistake worth saying.
func TestLoadWithoutAFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	xdg.Reload()
	t.Cleanup(xdg.Reload)
	if err := os.Unsetenv(FileVar); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil || cfg.Read || !reflect.DeepEqual(cfg.New, New{}) {
		t.Fatalf("Load() = %+v, %v; want nothing read and nothing set", cfg, err)
	}

	t.Setenv(FileVar, "")
	if cfg, err := Load(); err != nil || cfg.Path != "" || cfg.Read {
		t.Fatalf("Load() with %s empty = %+v, %v; want no file looked for", FileVar, cfg, err)
	}

	t.Setenv(FileVar, filepath.Join(t.TempDir(), "missing.yaml"))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), FileVar) {
		t.Fatalf("Load() error = %v, want one naming %s", err, FileVar)
	}
}

// The reference uncommented whole configures exactly what no file does.
func TestUncommentedExampleConfiguresNothing(t *testing.T) {
	body, err := ExampleYAML()
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if strings.HasPrefix(trimmed, "#") && !strings.HasPrefix(trimmed, "# ") && trimmed != "#" {
			line = strings.Replace(line, "#", "", 1)
		}
		lines = append(lines, line)
	}
	writeConfig(t, strings.Join(lines, "\n"))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() of the uncommented reference error = %v", err)
	}
	if !reflect.DeepEqual(cfg.New, New{}) {
		t.Fatalf("the uncommented reference configures %+v, want nothing", cfg.New)
	}
}

// The checked-in reference and schema are what a reader copies and an editor
// checks against, so stale ones describe a different client.
func TestCheckedInArtifactsAreCurrent(t *testing.T) {
	example, err := ExampleYAML()
	if err != nil {
		t.Fatal(err)
	}
	schema, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]byte{
		filepath.Join("..", "..", ExampleFileName):      example,
		filepath.Join("..", "..", "config.schema.json"): append(encoded, '\n'),
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != string(want) {
			t.Fatalf("%s is stale; run: go tool task generate", path)
		}
	}
}

func TestRefreshExampleWritesBesideTheFile(t *testing.T) {
	configFile := filepath.Join(t.TempDir(), "nested", DefaultFileName)
	path, err := RefreshExample(configFile)
	if err != nil {
		t.Fatalf("RefreshExample() error = %v", err)
	}
	want, _ := ExampleYAML()
	got, err := os.ReadFile(path)
	if err != nil || path != filepath.Join(filepath.Dir(configFile), ExampleFileName) || string(got) != string(want) {
		t.Fatalf("RefreshExample() wrote %s (%v), want the reference beside %s", path, err, configFile)
	}
}

// The error is one line naming the key, because a console shows one line.
func TestLoadNamesTheKeyOnOneLine(t *testing.T) {
	path := writeConfig(t, "new:\n  userSkils: true\n  skils: []\n")
	_, err := Load()
	if err == nil || strings.Contains(err.Error(), "\n") || !strings.HasPrefix(err.Error(), path+": line 2: field userSkils") || !strings.Contains(err.Error(), "skils not found") {
		t.Fatalf("Load() error = %q, want one line naming both keys", err)
	}
}
