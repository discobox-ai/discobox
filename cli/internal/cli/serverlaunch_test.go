package cli

import (
	"slices"
	"testing"
)

// The autolaunched server is this binary re-invoked with these arguments. If
// they do not name the server command, the child exits immediately with
// "unknown command" — into a discarded stderr — and every CLI invocation waits
// out the start timeout before reporting that the server never came up. That
// is exactly what happened when the command moved under `admin` and the
// hardcoded argv did not follow it.
func TestServerLaunchArgsReachTheServerCommand(t *testing.T) {
	root, app := newRootCommand()

	args, err := app.serverLaunchArgs()
	if err != nil {
		t.Fatal(err)
	}
	if len(args) == 0 {
		t.Fatal("serverLaunchArgs returned no arguments")
	}

	found, remaining, err := root.Find(args)
	if err != nil {
		t.Fatalf("root.Find(%v): %v", args, err)
	}
	if found != app.serverCmd {
		t.Fatalf("root.Find(%v) resolved to %q, want the server command", args, found.CommandPath())
	}
	// Every argument has to be consumed by the lookup. A leftover means cobra
	// stopped at an ancestor and would treat the rest as positional arguments.
	if len(remaining) != 0 {
		t.Fatalf("root.Find(%v) left %v unconsumed", args, remaining)
	}
}

// The path is read off the tree, so moving the command moves the argv with it.
func TestServerLaunchArgsFollowTheCommandTree(t *testing.T) {
	_, app := newRootCommand()
	args, err := app.serverLaunchArgs()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"admin", "server"}; !slices.Equal(args, want) {
		t.Fatalf("serverLaunchArgs = %v, want %v (update this if the command moved on purpose)", args, want)
	}
}
