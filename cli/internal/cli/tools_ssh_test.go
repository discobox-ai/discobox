package cli

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// TestSSHBridgeArgsCarryEverythingTheSessionNeeds: nothing is written to the
// user's ssh_config, so every decision has to be on the command line.
func TestSSHBridgeArgsCarryEverythingTheSessionNeeds(t *testing.T) {
	args := sshBridgeArgs(45678, "sbx_devbox00000001", "/state/id_ed25519", "/tmp/known_hosts")
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"-p 45678",
		"-l sbx_devbox00000001",
		"-i /state/id_ed25519",
		"-o IdentitiesOnly=yes",
		"-o UserKnownHostsFile=/tmp/known_hosts",
		"-o StrictHostKeyChecking=yes",
		// The bridge is this process; a `Host *` block in the user's config
		// could otherwise override the identity or user just resolved.
		"-F none",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %v missing %q", args, want)
		}
	}
	// The host is deliberately not here: it has to sit after the user's own
	// options and before their remote command, so the caller places it.
	for _, arg := range args {
		if arg == sshBridgeHost {
			t.Fatalf("the host must not be part of the option list, got %v", args)
		}
	}
}

// TestSSHBridgeSpellsTheKnownHostsFileForTheConfigParser: ssh reads a -o
// argument as a config line, so the value is percent-expanded and split on
// whitespace. The temp file lives under the profile, which on a Windows account
// named "Ada Lovelace" is a path with a space in it: unquoted it arrives as two
// filenames, neither of which exists, and StrictHostKeyChecking=yes below fails
// the session. The -i beside it is neither expanded nor split, and is passed
// as it is.
func TestSSHBridgeSpellsTheKnownHostsFileForTheConfigParser(t *testing.T) {
	args := sshBridgeArgs(45678, "sbx_devbox00000001", `C:\Users\Ada Lovelace\100%\id`,
		`C:\Users\Ada Lovelace\AppData\Local\Temp\100%\known_hosts`)
	if !slices.Contains(args, `UserKnownHostsFile="C:\Users\Ada Lovelace\AppData\Local\Temp\100%%\known_hosts"`) {
		t.Fatalf("the known_hosts option is not spelled for ssh's config parser: %v", args)
	}
	if !slices.Contains(args, `C:\Users\Ada Lovelace\100%\id`) {
		t.Fatalf("the identity should be passed as it is: %v", args)
	}
}

// TestWriteTemporaryKnownHostsPinsThePortInUse: the bridge's port differs every
// run, so the entry is written for the port in use and thrown away with it. A
// stale entry for a reused loopback port would be worse than none.
func TestWriteTemporaryKnownHostsPinsThePortInUse(t *testing.T) {
	path, cleanup, err := writeTemporaryKnownHosts(45678, "ssh-ed25519 AAAAfakehostkey==")
	if err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read known_hosts: %v", err)
	}
	if got, want := strings.TrimSpace(string(data)), "[127.0.0.1]:45678 ssh-ed25519 AAAAfakehostkey=="; got != want {
		t.Fatalf("known_hosts = %q, want %q", got, want)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("known_hosts survived cleanup: %v", err)
	}
}

// TestWriteTemporaryKnownHostsRefusesAnUnverifiableServer: without a host key
// there is nothing to verify against, and connecting anyway would mean turning
// host verification off.
func TestWriteTemporaryKnownHostsRefusesAnUnverifiableServer(t *testing.T) {
	if _, _, err := writeTemporaryKnownHosts(45678, "  "); err == nil {
		t.Fatal("expected a missing host key to be refused")
	}
}

func TestSSHConnectWebSocketURL(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{base: "http://localhost", want: "ws://localhost/ssh/connect"},
		{base: "https://discobox.example.com", want: "wss://discobox.example.com/ssh/connect"},
		// A unix endpoint keeps its placeholder host: the HTTP client dials the
		// socket whatever the URL says, and the host only makes it parseable.
		{base: "http://unix", want: "ws://unix/ssh/connect"},
	} {
		got, err := sshConnectWebSocketURL(tc.base)
		if err != nil {
			t.Fatalf("connect URL for %q: %v", tc.base, err)
		}
		if got != tc.want {
			t.Fatalf("connect URL for %q = %q, want %q", tc.base, got, tc.want)
		}
	}
}

// TestToolsSSHDoesNotParseSSHFlags is the reason flag parsing is disabled on
// this command: `discobox tools ssh -L 8080:localhost:3000` puts ssh's own flags
// first, and cobra rejects unknown shorthand flags before RunE ever runs. The
// command is pointed at a dead server so it fails later, on purpose — what is
// asserted is that it got past argument parsing at all.
func TestToolsSSHDoesNotParseSSHFlags(t *testing.T) {
	for _, args := range [][]string{
		{"-L", "8080:localhost:3000"},
		{"-N"},
		{"-o", "ServerAliveInterval=30"},
		{"-D", "1080", "-v"},
	} {
		cmd := NewRootCommand()
		cmd.SetOut(new(strings.Builder))
		cmd.SetErr(new(strings.Builder))
		cmd.SetArgs(append([]string{
			"--server", "unix:///nonexistent/discobox-test.sock", "--auto-start-server=false",
			"--project", "project-1", "tools", "ssh",
		}, args...))
		err := cmd.Execute()
		if err == nil {
			t.Fatalf("%v: expected the dead server to fail the command", args)
		}
		for _, rejected := range []string{"unknown shorthand flag", "unknown flag", "flag needs an argument"} {
			if strings.Contains(err.Error(), rejected) {
				t.Fatalf("%v: cobra parsed ssh's flags: %v", args, err)
			}
		}
	}
}
