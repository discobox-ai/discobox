package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wslTestTarget is what windowsSSHTarget would have resolved on a machine whose
// Windows drive is mounted at root: the state directory and the user's
// ssh_config in both spellings, and the distribution the ProxyCommand re-enters.
//
// The user's name has a space in it on purpose. A Windows profile routinely
// does, and every path this writes ends up inside one.
func wslTestTarget(root string) sshTarget {
	target := sshTarget{windows: true, wslDistro: "Ubuntu"}
	target.state = target.join(sshPath{
		local:  filepath.Join(root, "Users", "Ada Lovelace", "AppData", "Local"),
		client: `C:\Users\Ada Lovelace\AppData\Local`,
	}, "discobox", "cli")
	target.userConfig = target.join(sshPath{
		local:  filepath.Join(root, "Users", "Ada Lovelace"),
		client: `C:\Users\Ada Lovelace`,
	}, ".ssh", "config")
	return target
}

// A path this process writes and Windows reads has two spellings, and every
// file the config names has to be in the second one.
func TestSSHTargetSpellsPathsForBothSides(t *testing.T) {
	target := wslTestTarget(t.TempDir())
	config := target.configPath("proj_1")
	if want := `C:\Users\Ada Lovelace\AppData\Local\discobox\cli\ssh\proj_1\config`; config.client != want {
		t.Fatalf("client spelling = %q, want %q", config.client, want)
	}
	if !strings.HasSuffix(config.local, filepath.Join("discobox", "cli", "ssh", "proj_1", "config")) {
		t.Fatalf("local spelling = %q, want a path this process can open", config.local)
	}
	if known := target.knownHostsPath("proj_1"); !strings.HasSuffix(known.client, `\proj_1\known_hosts`) {
		t.Fatalf("known_hosts client spelling = %q", known.client)
	}
}

// The ProxyCommand is the one line that has to run on the far side: Windows
// ssh.exe cannot execute a Linux binary, so it re-enters the distribution.
func TestSSHTargetProxyCommandReEntersWSL(t *testing.T) {
	target := wslTestTarget(t.TempDir())
	line, err := target.proxyCommandLine("http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("proxyCommandLine: %v", err)
	}
	if !strings.HasPrefix(line, `wsl.exe -d "Ubuntu" -e "`) {
		t.Fatalf("ProxyCommand does not re-enter the distribution: %q", line)
	}
	// Double quotes, not the shell's single ones: %COMSPEC% is what runs it.
	if !strings.Contains(line, `--server "http://127.0.0.1:8080" admin ssh-proxy`) {
		t.Fatalf("ProxyCommand is not quoted for cmd.exe: %q", line)
	}
	if strings.Contains(line, "'") {
		t.Fatalf("ProxyCommand carries POSIX quoting Windows will not strip: %q", line)
	}
}

// The key exists on both sides of the boundary because Windows ssh.exe opens
// the file itself, and it is refreshed on every run so a rotated key cannot
// leave a stale one behind.
func TestSSHTargetMirrorsTheIdentityAcrossTheBoundary(t *testing.T) {
	target := wslTestTarget(t.TempDir())
	source := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(source, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source+".pub", []byte("PUBLIC"), 0o600); err != nil {
		t.Fatal(err)
	}

	mirrored, err := target.mirrorSSHIdentity(source)
	if err != nil {
		t.Fatalf("mirrorSSHIdentity: %v", err)
	}
	if want := `C:\Users\Ada Lovelace\AppData\Local\discobox\cli\ssh\id_ed25519`; mirrored.client != want {
		t.Fatalf("mirrored key = %q, want %q", mirrored.client, want)
	}
	if got := readFile(t, mirrored.local); got != "PRIVATE" {
		t.Fatalf("mirrored key = %q", got)
	}
	if got := readFile(t, mirrored.local+".pub"); got != "PUBLIC" {
		t.Fatalf("mirrored public key = %q", got)
	}

	// A rotated key replaces the copy rather than being ignored.
	if err := os.WriteFile(source, []byte("ROTATED"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := target.mirrorSSHIdentity(source); err != nil {
		t.Fatalf("mirrorSSHIdentity again: %v", err)
	}
	if got := readFile(t, mirrored.local); got != "ROTATED" {
		t.Fatalf("mirrored key after rotation = %q", got)
	}
}

// A key the user enrolled by hand has no .pub beside it more often than not,
// and ssh derives the public half from the private one anyway.
func TestSSHTargetMirrorsAKeyWithNoPublicHalf(t *testing.T) {
	target := wslTestTarget(t.TempDir())
	source := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(source, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	mirrored, err := target.mirrorSSHIdentity(source)
	if err != nil {
		t.Fatalf("mirrorSSHIdentity: %v", err)
	}
	if !strings.HasSuffix(mirrored.client, `\id_rsa`) {
		t.Fatalf("mirrored key = %q, want the source's own name", mirrored.client)
	}
}

// Nothing is copied when both sides are the same side: the local target names
// the key where it already is.
func TestSSHTargetLeavesTheIdentityAloneLocally(t *testing.T) {
	target, err := localSSHTarget()
	if err != nil {
		t.Fatalf("localSSHTarget: %v", err)
	}
	mirrored, err := target.mirrorSSHIdentity("/home/u/.local/state/discobox/cli/ssh/id_ed25519")
	if err != nil {
		t.Fatalf("mirrorSSHIdentity: %v", err)
	}
	if mirrored.local != "/home/u/.local/state/discobox/cli/ssh/id_ed25519" || mirrored.client != mirrored.local {
		t.Fatalf("local identity was moved: %+v", mirrored)
	}
}

// The Include is written and recognized in the spelling the reading ssh uses,
// quotes and backslashes included — otherwise every run appends another line.
func TestSSHTargetIncludeIsIdempotentAcrossTheBoundary(t *testing.T) {
	target := wslTestTarget(t.TempDir())
	managed := target.configPath("proj_1")

	added, dropped, err := target.ensureUserConfigInclude(managed)
	if err != nil {
		t.Fatalf("ensureUserConfigInclude: %v", err)
	}
	if !added || len(dropped) != 0 {
		t.Fatalf("added = %v, dropped = %v, want a single new line", added, dropped)
	}
	first := readFile(t, target.userConfig.local)
	if want := `Include "` + managed.client + `"` + "\n"; first != want {
		t.Fatalf("user config = %q, want %q", first, want)
	}

	added, _, err = target.ensureUserConfigInclude(managed)
	if err != nil {
		t.Fatalf("ensureUserConfigInclude again: %v", err)
	}
	if added {
		t.Fatal("the Include was written twice")
	}
	if got := readFile(t, target.userConfig.local); got != first {
		t.Fatalf("user config changed on the second run:\n%s", got)
	}
}

// A managed Include this CLI no longer writes is dropped whichever way its path
// is spelled: ssh fails outright on an Include it cannot read, so one left over
// from an older state directory breaks every host in the file.
func TestSSHTargetDropsStaleManagedIncludes(t *testing.T) {
	target := wslTestTarget(t.TempDir())
	managed := target.configPath("proj_1")
	stale := `C:\Users\Ada Lovelace\AppData\Roaming\discobox\cli\ssh\proj_1\config`
	existing := `Include "` + stale + `"` + "\n\nHost work\n    HostName work.example.com\n"
	if err := os.MkdirAll(filepath.Dir(target.userConfig.local), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target.userConfig.local, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	added, dropped, err := target.ensureUserConfigInclude(managed)
	if err != nil {
		t.Fatalf("ensureUserConfigInclude: %v", err)
	}
	if !added || len(dropped) != 1 || dropped[0] != target.clean(stale) {
		t.Fatalf("added = %v, dropped = %v, want the stale line gone", added, dropped)
	}
	got := readFile(t, target.userConfig.local)
	if strings.Contains(got, "Roaming") {
		t.Fatalf("the stale Include survived:\n%s", got)
	}
	if !strings.Contains(got, "Host work\n") {
		t.Fatalf("the user's own config was not preserved:\n%s", got)
	}
}

// The path in an Include is one field however many spaces it has in it.
func TestSSHConfigFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
		want []string
	}{
		{name: "plain", line: "Include /home/u/.ssh/config", want: []string{"Include", "/home/u/.ssh/config"}},
		{name: "quoted with a space", line: `Include "C:\Users\Ada Lovelace\.ssh\config"`, want: []string{"Include", `C:\Users\Ada Lovelace\.ssh\config`}},
		{name: "several paths", line: "Include a b", want: []string{"Include", "a", "b"}},
		{name: "extra whitespace", line: "  Include\t a  ", want: []string{"Include", "a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sshConfigFields(tc.line); !equalStrings(got, tc.want) {
				t.Fatalf("sshConfigFields(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}
