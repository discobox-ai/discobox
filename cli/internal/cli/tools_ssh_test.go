package cli

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// TestSSHClientOptionsCarryEverythingTheSessionNeeds: nothing is written to
// the user's ssh_config, so every decision has to be on the command line.
func TestSSHClientOptionsCarryEverythingTheSessionNeeds(t *testing.T) {
	proxy := `'/usr/local/bin/discobox' --server 'iroh://abc?addr=192.0.2.7%3A11204' admin ssh-proxy`
	args := sshClientOptions("/state/id_ed25519", "/tmp/known_hosts", "prj_1.discobox.internal", proxy)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"-i /state/id_ed25519",
		"-o IdentitiesOnly=yes",
		// ssh percent-expands a ProxyCommand whichever way it is given, so the
		// endpoint's escaped colon is escaped again, as the written config
		// spells it.
		`-o ProxyCommand='/usr/local/bin/discobox' --server 'iroh://abc?addr=192.0.2.7%%3A11204' admin ssh-proxy`,
		// The host is an alias with no address; the key is pinned under the
		// project's alias, as `admin ssh-config` pins it.
		"-o HostKeyAlias=prj_1.discobox.internal",
		"-o UserKnownHostsFile=/tmp/known_hosts",
		"-o StrictHostKeyChecking=yes",
		// A `Host *` block in the user's config could otherwise override the
		// identity, user or proxy just resolved.
		"-F none",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %v missing %q", args, want)
		}
	}
	// No port and no host: there is no address to name, and the host has to
	// sit after the user's own options and before their remote command, so
	// the caller places it.
	for _, arg := range args {
		if arg == "-p" || arg == "-P" || (strings.HasSuffix(arg, hostAliasSuffix) && !strings.HasPrefix(arg, "HostKeyAlias=")) {
			t.Fatalf("the options must name no port or host, got %v", args)
		}
	}
}

// TestSSHClientOptionsSpellTheKnownHostsFileForTheConfigParser: ssh reads a -o
// argument as a config line, so the value is percent-expanded and split on
// whitespace. The temp file lives under the profile, which on a Windows account
// named "Ada Lovelace" is a path with a space in it: unquoted it arrives as two
// filenames, neither of which exists, and StrictHostKeyChecking=yes fails the
// session. The -i beside it is neither expanded nor split, and is passed as it
// is.
func TestSSHClientOptionsSpellTheKnownHostsFileForTheConfigParser(t *testing.T) {
	args := sshClientOptions(`C:\Users\Ada Lovelace\100%\id`,
		`C:\Users\Ada Lovelace\AppData\Local\Temp\100%\known_hosts`, "prj_1.discobox.internal", "discobox")
	if !slices.Contains(args, `UserKnownHostsFile="C:\Users\Ada Lovelace\AppData\Local\Temp\100%%\known_hosts"`) {
		t.Fatalf("the known_hosts option is not spelled for ssh's config parser: %v", args)
	}
	if !slices.Contains(args, `C:\Users\Ada Lovelace\100%\id`) {
		t.Fatalf("the identity should be passed as it is: %v", args)
	}
}

// TestSSHClientProxyCommandIsQuotedForWindows: Windows OpenSSH does not hand a
// ProxyCommand to a POSIX shell, so the words are double-quoted there — the
// executable under "C:\Program Files" is the ordinary case — exactly as the
// written config quotes them, with the percent signs escaped on top.
func TestSSHClientProxyCommandIsQuotedForWindows(t *testing.T) {
	line, err := sshTarget{windows: true}.proxyCommandLine("iroh://abc?addr=192.0.2.7%3A11204")
	if err != nil {
		t.Fatal(err)
	}
	args := sshClientOptions("id", "known_hosts", "prj_1.discobox.internal", line)
	want := "ProxyCommand=" + escapeSSHPercent(line)
	if !slices.Contains(args, want) {
		t.Fatalf("args %v missing %q", args, want)
	}
	if !strings.Contains(want, `--server "iroh://abc?addr=192.0.2.7%%3A11204" admin ssh-proxy`) {
		t.Fatalf("ProxyCommand = %q, want the endpoint double-quoted and percent-escaped", want)
	}
}

// TestSSHProxyEnvCarriesWhatTheAppDialsWith: the ProxyCommand's command line
// names only the server, so the token and iroh settings reach it through the
// environment — and an App aimed at a registered server, which carries no
// token, must not hand the proxy one this process inherited for the primary.
func TestSSHProxyEnvCarriesWhatTheAppDialsWith(t *testing.T) {
	t.Setenv("DISCOBOX_TOKEN", "primary-token")
	primary := &App{serverURL: "http://primary", token: "primary-token", irohRelayURLs: "https://relay.example"}
	if env := primary.sshProxyEnv(); !slices.Contains(env, "DISCOBOX_TOKEN=primary-token") ||
		!slices.Contains(env, "DISCOBOX_IROH_RELAY_URLS=https://relay.example") {
		t.Fatalf("env = %v, want the primary's token and relays", env)
	}
	env := primary.forServer("http://registered").sshProxyEnv()
	if last := lastEnv(env, "DISCOBOX_TOKEN"); last != "" {
		t.Fatalf("DISCOBOX_TOKEN = %q for a registered server, want it cleared", last)
	}
	if last := lastEnv(env, "DISCOBOX_IROH_RELAY_URLS"); last != "https://relay.example" {
		t.Fatalf("DISCOBOX_IROH_RELAY_URLS = %q, want the relays forServer carries", last)
	}
}

// lastEnv is the value a child process sees for key in env: the last one wins.
func lastEnv(env []string, key string) string {
	value := ""
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == key {
			value = v
		}
	}
	return value
}

// TestWriteTemporaryKnownHostsPinsTheAlias: the entry is written under the
// host key alias the options name, not under a host or port, and is thrown
// away with the command.
func TestWriteTemporaryKnownHostsPinsTheAlias(t *testing.T) {
	path, cleanup, err := writeTemporaryKnownHosts("prj_1.discobox.internal", "ssh-ed25519 AAAAfakehostkey==")
	if err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read known_hosts: %v", err)
	}
	if got, want := strings.TrimSpace(string(data)), "prj_1.discobox.internal ssh-ed25519 AAAAfakehostkey=="; got != want {
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
	if _, _, err := writeTemporaryKnownHosts("prj_1.discobox.internal", "  "); err == nil {
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
// this command: `discobox tools ssh -- -L 8080:localhost:3000` hands ssh its own
// flags, and cobra would reject unknown shorthand flags before RunE ever runs.
// The command is pointed at a dead server so it fails later, on purpose — what
// is asserted is that it got past argument parsing at all. Written where
// DISCOBOX goes, without the --, the same flags are refused with the line
// that works rather than looked for as a discobox.
func TestToolsSSHDoesNotParseSSHFlags(t *testing.T) {
	run := func(args []string) error {
		cmd := NewRootCommand()
		cmd.SetOut(new(strings.Builder))
		cmd.SetErr(new(strings.Builder))
		cmd.SetArgs(append([]string{
			"--server", "unix:///nonexistent/discobox-test.sock", "--auto-start-server=false",
			"--project", "project-1", "tools", "ssh",
		}, args...))
		return cmd.Execute()
	}
	for _, args := range [][]string{
		{"-L", "8080:localhost:3000"},
		{"-N"},
		{"-o", "ServerAliveInterval=30"},
		{"-D", "1080", "-v"},
	} {
		err := run(append([]string{"--"}, args...))
		if err == nil {
			t.Fatalf("%v: expected the dead server to fail the command", args)
		}
		for _, rejected := range []string{"unknown shorthand flag", "unknown flag", "flag needs an argument", "go after --"} {
			if strings.Contains(err.Error(), rejected) {
				t.Fatalf("-- %v: the arguments were not ssh's: %v", args, err)
			}
		}
		if err := run(args); err == nil || !strings.Contains(err.Error(), "ssh's arguments go after --: discobox tools ssh -- "+strings.Join(args, " ")) {
			t.Fatalf("%v without --: error = %v, want the line that works", args, err)
		}
	}
}
