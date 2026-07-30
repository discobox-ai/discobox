package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRememberSelectionRoundTrips(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if got := lastSelection("sandbox:prj_1"); got != "" {
		t.Fatalf("lastSelection on a fresh state dir = %q, want empty", got)
	}
	if err := rememberSelection("sandbox:prj_1", "sbx_a"); err != nil {
		t.Fatalf("rememberSelection: %v", err)
	}
	if err := rememberSelection("sandbox:prj_2", "sbx_b"); err != nil {
		t.Fatalf("rememberSelection: %v", err)
	}
	if err := rememberSelection("sandbox:prj_1", "sbx_c"); err != nil {
		t.Fatalf("rememberSelection: %v", err)
	}
	if got := lastSelection("sandbox:prj_1"); got != "sbx_c" {
		t.Fatalf("lastSelection = %q, want the latest pick sbx_c", got)
	}
	// Keys must not collide: each project remembers its own sandbox.
	if got := lastSelection("sandbox:prj_2"); got != "sbx_b" {
		t.Fatalf("lastSelection = %q, want sbx_b", got)
	}
	if got := lastSelection("sandbox:prj_3"); got != "" {
		t.Fatalf("lastSelection for an unknown key = %q, want empty", got)
	}
}

func TestRecentSelectionsLiveUnderXDGStateHome(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	if err := rememberSelection("sandbox:prj_1", "sbx_a"); err != nil {
		t.Fatalf("rememberSelection: %v", err)
	}
	want := filepath.Join(state, "discobox", "cli", "recent-selections.json")
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("stat %s: %v", want, err)
	}
}

// A corrupt or truncated state file is a fallback, not a failure: the picker
// still opens and the next pick overwrites it.
func TestCorruptRecentSelectionsAreIgnored(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	if err := os.MkdirAll(filepath.Join(state, "discobox", "cli"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "discobox", "cli", "recent-selections.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lastSelection("sandbox:prj_1"); got != "" {
		t.Fatalf("lastSelection = %q, want empty", got)
	}
	if err := rememberSelection("sandbox:prj_1", "sbx_a"); err != nil {
		t.Fatalf("rememberSelection: %v", err)
	}
	if got := lastSelection("sandbox:prj_1"); got != "sbx_a" {
		t.Fatalf("lastSelection = %q, want sbx_a", got)
	}
}

func TestRememberSelectionIgnoresEmptyKeyOrID(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	if err := rememberSelection("", "sbx_a"); err != nil {
		t.Fatalf("rememberSelection with no key: %v", err)
	}
	if err := rememberSelection("sandbox:prj_1", ""); err != nil {
		t.Fatalf("rememberSelection with no id: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "discobox", "cli", "recent-selections.json")); !os.IsNotExist(err) {
		t.Fatalf("stat err = %v, want the state file never written", err)
	}
}

func TestRecentSelectionsAreTrimmedToTheLimit(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for i := 0; i < recentSelectionLimit+10; i++ {
		if err := rememberSelection(string(rune('a'+i%26))+string(rune('a'+i/26)), "sbx_x"); err != nil {
			t.Fatalf("rememberSelection: %v", err)
		}
	}
	if got := len(loadRecentSelections()); got > recentSelectionLimit {
		t.Fatalf("entries = %d, want at most %d", got, recentSelectionLimit)
	}
}
