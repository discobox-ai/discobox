package cli

import (
	"strings"
	"testing"
)

// The flags a child is given are the ones this invocation is using, so it talks
// to the same server, project and directory — and the token is not among them,
// since every process on the machine can read an argument list.
func TestGlobalFlagsCarryTheSessionButNotTheToken(t *testing.T) {
	app := &App{serverURL: "unix:///run/x.sock", projectID: "obot", source: "/src/disco2", token: "secret", autoStart: autoStartServerFalse}
	flags := strings.Join(app.globalFlags(), " ")

	for _, want := range []string{"--server unix:///run/x.sock", "--project obot", "--chdir /src/disco2", "--auto-start-server=false"} {
		if !strings.Contains(flags, want) {
			t.Errorf("flags %q missing %q", flags, want)
		}
	}
	if strings.Contains(flags, "secret") {
		t.Errorf("the token should not be in the argument list: %q", flags)
	}
}

// The auto case is not forwarded at all: the child makes its own build and
// environment answer rather than being handed a fixed one, exactly as this
// invocation would have if nothing had told it otherwise.
func TestGlobalFlagsLeavesAutoStartUnsaidWhenAuto(t *testing.T) {
	app := &App{serverURL: "unix:///run/x.sock", projectID: "obot", autoStart: autoStartServerAuto}
	flags := strings.Join(app.globalFlags(), " ")

	if strings.Contains(flags, "auto-start-server") {
		t.Errorf("flags %q should not mention auto-start-server", flags)
	}
}
