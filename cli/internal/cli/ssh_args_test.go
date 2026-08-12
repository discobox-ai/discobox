package cli

import (
	"reflect"
	"testing"
)

// TestSplitSSHArgs pins the parse that decides where each argument lands.
// `ssh [options] host [command]` is positional, and this command supplies the
// host, so options must be separated from the remote command rather than all
// appended after it — which only appears to work on glibc, whose getopt
// permutes argv, and sends every option to the remote as a command elsewhere.
func TestSplitSSHArgs(t *testing.T) {
	for name, tc := range map[string]struct {
		args       []string
		options    []string
		command    []string
		background bool
	}{
		"nothing": {},
		"a bare remote command": {
			args:    []string{"uname", "-a"},
			command: []string{"uname", "-a"},
		},
		"a forward": {
			args:    []string{"-L", "8080:localhost:3000"},
			options: []string{"-L", "8080:localhost:3000"},
		},
		"a forward with the value attached": {
			args:    []string{"-L8080:localhost:3000", "-N"},
			options: []string{"-L8080:localhost:3000", "-N"},
		},
		"bundled booleans": {
			args:       []string{"-Nf"},
			options:    []string{"-Nf"},
			background: true,
		},
		"options then a command": {
			args:    []string{"-o", "ServerAliveInterval=30", "uname", "-a"},
			options: []string{"-o", "ServerAliveInterval=30"},
			command: []string{"uname", "-a"},
		},
		"-- ends the options and is dropped": {
			args:    []string{"-N", "--", "uname", "-a"},
			options: []string{"-N"},
			command: []string{"uname", "-a"},
		},
		// The f here is part of the forward spec, not a flag.
		"an f inside a value is not -f": {
			args:    []string{"-L", "f:1:2"},
			options: []string{"-L", "f:1:2"},
		},
		"an f attached inside a value is not -f": {
			args:    []string{"-oProxyCommand=f"},
			options: []string{"-oProxyCommand=f"},
		},
		"-f among several": {
			args:       []string{"-N", "-f", "-L", "8080:localhost:3000"},
			options:    []string{"-N", "-f", "-L", "8080:localhost:3000"},
			background: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			options, command, background := splitSSHArgs(tc.args)
			if !reflect.DeepEqual(options, tc.options) {
				t.Fatalf("options = %q, want %q", options, tc.options)
			}
			if !reflect.DeepEqual(command, tc.command) {
				t.Fatalf("command = %q, want %q", command, tc.command)
			}
			if background != tc.background {
				t.Fatalf("background = %v, want %v", background, tc.background)
			}
		})
	}
}
