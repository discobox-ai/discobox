package layout

import (
	"path/filepath"
	"testing"
)

func TestHostStateDefaultsToLocalShare(t *testing.T) {
	home := t.TempDir()
	setXDG(t, map[string]string{"HOME": home, "XDG_DATA_HOME": ""})
	got, err := hostState()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "share", "discobox", HostStateName); got != want {
		t.Fatalf("hostState = %q, want %q", got, want)
	}
}
