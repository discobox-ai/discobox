package layout

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// Windows keeps it in LocalAppData rather than the roaming AppData: a pool's
// state is this machine's, and nothing in it should follow the user elsewhere.
func TestHostStateDefaultsToLocalAppData(t *testing.T) {
	local, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil {
		t.Fatal(err)
	}
	setXDG(t, map[string]string{"XDG_DATA_HOME": ""})
	got, err := hostState()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(local, "discobox", HostStateName); got != want {
		t.Fatalf("hostState = %q, want %q", got, want)
	}
}
