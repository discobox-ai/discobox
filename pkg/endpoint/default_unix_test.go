//go:build !windows

package endpoint

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Parse reads the default endpoint's decoded path back, so a runtime directory
// holding a character a URL gives meaning to must be escaped when it is
// rendered: unescaped, "#" and "?" end the path early and "%" fails to parse.
func TestDefaultEndpointEscapesTheRuntimeDir(t *testing.T) {
	t.Setenv(AddressEnv, "")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/a b#c?d%e")
	got, err := Parse(DefaultEndpoint())
	if err != nil {
		t.Fatalf("Parse(%q): %v", DefaultEndpoint(), err)
	}
	if want := "/run/user/a b#c?d%e/discobox/server.sock"; got.Scheme != "unix" || got.Value != want {
		t.Fatalf("Parse(%q) = %+v, want unix socket %q", DefaultEndpoint(), got, want)
	}
}

func TestDefaultEndpointFollowsTMPDIR(t *testing.T) {
	t.Setenv(AddressEnv, "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	for _, temp := range []string{t.TempDir(), "/var/folders/ab/user/T/"} {
		t.Setenv("TMPDIR", temp)
		got, err := Parse(DefaultEndpoint())
		want := filepath.Join(temp, "discobox-"+strconv.Itoa(os.Getuid()), "discobox", "server.sock")
		if err != nil || got.Value != want {
			t.Fatalf("got %+v, %v; want %q", got, err, want)
		}
	}
}
