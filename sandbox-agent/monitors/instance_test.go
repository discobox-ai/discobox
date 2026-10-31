package monitors

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEnableIsIdempotentAndNeverReplacesAnInstance(t *testing.T) {
	d := HomeDirs(t.TempDir())
	inst := Instance{Name: "pr-412", Monitor: "github-repo", Options: map[string]string{"repo": "a/b", "pr": "412"}}
	created, err := Enable(d, inst)
	if err != nil || !created {
		t.Fatalf("first enable = %v, %v", created, err)
	}
	created, err = Enable(d, inst)
	if err != nil || created {
		t.Fatalf("same enable again = %v, %v; want nothing changed", created, err)
	}
	other := inst
	other.Options = map[string]string{"repo": "a/b", "pr": "413"}
	if _, err := Enable(d, other); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("enable with other options = %v, want ErrNameTaken", err)
	}
	got, err := ReadInstance(d, "pr-412")
	if err != nil || got.Options["pr"] != "412" {
		t.Fatalf("instance = %+v, %v", got, err)
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
	if _, err := Enable(d, Instance{Name: "ci", Monitor: "github-repo"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a previous instance's state survived a new enable: %v", err)
	}
}

func TestDisableRemovesTheInstanceAndItsState(t *testing.T) {
	d := HomeDirs(t.TempDir())
	if _, err := Enable(d, Instance{Name: "ci", Monitor: "github-repo"}); err != nil {
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
		if _, err := Enable(d, Instance{Name: name, Monitor: "m"}); err == nil {
			t.Errorf("name %q was accepted", name)
		}
	}
}
