package cli

import (
	"os"
	"strings"
	"testing"
)

// One binary is installed under two names so the two Homebrew channels can sit
// beside each other (ADR 0105), and everything a user reads has to say which
// one they ran.
func TestCommandName(t *testing.T) {
	for _, tc := range []struct {
		argv0 string
		want  string
	}{
		{"discobox", "discobox"},
		{"/opt/homebrew/bin/discobox", "discobox"},
		{"/opt/homebrew/bin/discobox-dev", "discobox-dev"},
		{"discobox.exe", "discobox"},
		{"discobox-dev.exe", "discobox-dev"},
		// Not a discobox name: a test binary, `go run`'s temporary file, a copy
		// someone renamed. Output must not depend on where the binary lives.
		{"/tmp/go-build123/b001/cli.test", "discobox"},
		{"/home/ada/Downloads/dbx", "discobox"},
		{"", "discobox"},
	} {
		t.Run(tc.argv0, func(t *testing.T) {
			defer withArgv0(t, tc.argv0)()
			if got := commandName(); got != tc.want {
				t.Fatalf("commandName() with argv[0] %q = %q, want %q", tc.argv0, got, tc.want)
			}
		})
	}
}

// The name reaches the places a reader actually sees it: the usage line, the
// examples in the long help, and the completion command's own instructions.
func TestCommandNameReachesHelp(t *testing.T) {
	defer withArgv0(t, "/opt/homebrew/bin/discobox-dev")()

	root := NewRootCommand()
	if got := root.Name(); got != "discobox-dev" {
		t.Fatalf("root command name = %q, want discobox-dev", got)
	}
	if !strings.Contains(root.Long, "discobox-dev -p 'fix the failing tests'") {
		t.Fatalf("long help does not name the binary it was run as:\n%s", root.Long)
	}

	var completion string
	for _, sub := range root.Commands() {
		if sub.Name() == "completion" {
			completion = sub.Long
		}
	}
	if completion == "" {
		t.Fatal("no completion command")
	}
	// Writing _discobox from a discobox-dev install would overwrite the stable
	// install's completions.
	if !strings.Contains(completion, `"${fpath[1]}/_discobox-dev"`) {
		t.Fatalf("completion help writes the wrong file:\n%s", completion)
	}
}

func withArgv0(t *testing.T, argv0 string) func() {
	t.Helper()
	saved := os.Args
	os.Args = append([]string{argv0}, saved[1:]...)
	return func() { os.Args = saved }
}
