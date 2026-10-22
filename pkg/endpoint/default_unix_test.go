//go:build !windows

package endpoint

import "testing"

// Parse reads the default endpoint's decoded path back, so a runtime directory
// holding a character a URL gives meaning to must be escaped when it is
// rendered: unescaped, "#" and "?" end the path early and "%" fails to parse.
func TestDefaultEndpointEscapesTheRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/a b#c?d%e")
	got, err := Parse(DefaultEndpoint())
	if err != nil {
		t.Fatalf("Parse(%q): %v", DefaultEndpoint(), err)
	}
	if want := "/run/user/a b#c?d%e/discobox/server.sock"; got.Scheme != "unix" || got.Value != want {
		t.Fatalf("Parse(%q) = %+v, want unix socket %q", DefaultEndpoint(), got, want)
	}
}
