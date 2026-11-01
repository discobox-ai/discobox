package monitors

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnableIsIdempotentAndNeverReplacesAnInstance(t *testing.T) {
	d := HomeDirs(t.TempDir())
	inst := Instance{Name: "pr-412", Monitor: "github-repo", Options: map[string]string{"repo": "a/b", "pr": "412"}}
	result, err := Enable(d, inst, false)
	if err != nil || result != Created {
		t.Fatalf("first enable = %v, %v", result, err)
	}
	result, err = Enable(d, inst, false)
	if err != nil || result != Unchanged {
		t.Fatalf("same enable again = %v, %v; want nothing changed", result, err)
	}
	other := inst
	other.Options = map[string]string{"repo": "a/b", "pr": "413"}
	if _, err := Enable(d, other, false); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("enable with other options = %v, want ErrNameTaken", err)
	}
	got, err := ReadInstance(d, "pr-412")
	if err != nil || got.Options["pr"] != "412" {
		t.Fatalf("instance = %+v, %v", got, err)
	}
}

func TestEnableUpdateChangesOptionsKeepingTheInstanceAndItsState(t *testing.T) {
	d := HomeDirs(t.TempDir())
	inst := Instance{Name: "lead", Monitor: "github-repo", Options: map[string]string{"repo": "a/b", "pr": "412"}}
	if _, err := Enable(d, inst, true); err != nil {
		t.Fatal(err)
	}
	before, err := ReadInstance(d, "lead")
	if err != nil {
		t.Fatal(err)
	}
	baseline := filepath.Join(d.MonitorStateDir("lead", before.ID), "since")
	if err := os.MkdirAll(filepath.Dir(baseline), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(baseline, []byte("then"), 0o644); err != nil {
		t.Fatal(err)
	}

	grown := inst
	grown.Options = map[string]string{"repo": "a/b", "pr": "412,413"}
	if result, err := Enable(d, grown, true); err != nil || result != Updated {
		t.Fatalf("enable --update = %v, %v; want Updated", result, err)
	}
	after, err := ReadInstance(d, "lead")
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID || after.Options["pr"] != "412,413" || after.UpdatedAt == nil || !after.EnabledAt.Equal(before.EnabledAt) {
		t.Fatalf("updated instance = %+v, was %+v", after, before)
	}
	if data, err := os.ReadFile(baseline); err != nil || string(data) != "then" {
		t.Fatalf("state after update = %q, %v; want it kept", data, err)
	}
	if result, err := Enable(d, grown, true); err != nil || result != Unchanged {
		t.Fatalf("same update again = %v, %v; want Unchanged", result, err)
	}

	// An update resolves the declaration again: a repository's copy now
	// replacing the skill's is what the instance runs.
	moved := grown
	moved.Path = "/repo/.discobox/monitors/github-repo.sh"
	if result, err := Enable(d, moved, true); err != nil || result != Updated {
		t.Fatalf("enable --update with a new declaration = %v, %v; want Updated", result, err)
	}
	if got, _ := ReadInstance(d, "lead"); got.Path != moved.Path || got.ID != before.ID {
		t.Fatalf("instance after a new declaration = %+v", got)
	}
	// Without --update, the same options naming another script of that id —
	// a skill's, named by its path — are refused, not taken for this one.
	if _, err := Enable(d, grown, false); !errors.Is(err, ErrNameTaken) || !strings.Contains(err.Error(), "running "+moved.Path) {
		t.Fatalf("plain enable of another script = %v; want ErrNameTaken naming the script it runs", err)
	}
	if result, err := Enable(d, moved, false); err != nil || result != Unchanged {
		t.Fatalf("plain enable of what runs = %v, %v; want Unchanged", result, err)
	}

	other := grown
	other.Monitor = "url"
	if _, err := Enable(d, other, true); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("enable --update of another monitor = %v, want ErrNameTaken", err)
	}
}

func TestANewInstanceStartsFromEmptyState(t *testing.T) {
	d := HomeDirs(t.TempDir())
	stale := filepath.Join(d.MonitorStateDir("ci", "old"), "since")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("yesterday"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Enable(d, Instance{Name: "ci", Monitor: "github-repo"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a previous instance's state survived a new enable: %v", err)
	}
}

func TestDisableRemovesTheInstanceAndItsState(t *testing.T) {
	d := HomeDirs(t.TempDir())
	if _, err := Enable(d, Instance{Name: "ci", Monitor: "github-repo"}, false); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(d.MonitorStateDir("ci", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Disable(d, "ci"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(d.InstanceStateDir("ci")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state left behind: %v", err)
	}
	if err := Disable(d, "ci"); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("second disable = %v, want ErrNotEnabled", err)
	}
	if instances, problems := Enabled(d); len(instances) != 0 || len(problems) != 0 {
		t.Fatalf("enabled = %v, %v", instances, problems)
	}
}

func TestEnableRefusesANameThatCannotBeAFile(t *testing.T) {
	d := HomeDirs(t.TempDir())
	for _, name := range []string{"", "../x", "Has-Caps", "a/b", ".hidden"} {
		if _, err := Enable(d, Instance{Name: name, Monitor: "m"}, false); err == nil {
			t.Errorf("name %q was accepted", name)
		}
	}
}
