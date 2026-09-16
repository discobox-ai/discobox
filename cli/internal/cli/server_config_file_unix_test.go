//go:build !windows

package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --config-file reaches the server as the variable it reads, since the server
// takes no flags (ADR 0096 §1). Relative to where the command was run, because
// that is where the user named it from; and set empty when given empty, which
// is how a server is told to read no file at all.
func TestServerConfigFileFlagReachesTheServer(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{"relative", []string{"--config-file", "mine.yaml"}, ServerConfigFileEnv + "=" + filepath.Join(dir, "mine.yaml")},
		{"empty", []string{"--config-file="}, ServerConfigFileEnv + "="},
		{"absent", nil, ServerConfigFileEnv + "=from-environment"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(ServerConfigFileEnv, "from-environment")
			seen := filepath.Join(t.TempDir(), "env")
			server := filepath.Join(t.TempDir(), "discobox-server")
			script := "#!/bin/sh\nenv > '" + seen + "'\n"
			if err := os.WriteFile(server, []byte(script), 0o700); err != nil { //nolint:gosec // G306: a test's own executable.
				t.Fatal(err)
			}

			root, _ := newRootCommand()
			root.SetArgs(append([]string{"admin", "server", "--binary", server}, test.args...))
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			if err := root.Execute(); err != nil {
				t.Fatalf("server: %v", err)
			}

			env, err := os.ReadFile(seen)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, line := range strings.Split(string(env), "\n") {
				if strings.HasPrefix(line, ServerConfigFileEnv+"=") {
					got = append(got, line)
				}
			}
			// The last one wins in a child's environment, but a child that
			// sees two is being told two things; there must be exactly one
			// by the time it runs.
			if len(got) == 0 || got[len(got)-1] != test.want {
				t.Fatalf("server saw %v, want %q", got, test.want)
			}
		})
	}
}
