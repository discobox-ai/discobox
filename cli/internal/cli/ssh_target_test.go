package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// wslTestWindowsTools puts the Windows programs the bridge shells out to on
// PATH: mirroring a key now sets its ACL, and that is icacls' job.
//
// Like fakeWSLMachine, it is a set of shell scripts, and the bridge it stands
// up only ever runs from inside a distribution: there is no WSL half on a
// native Windows host, which cannot execute the fakes either.
func wslTestWindowsTools(t *testing.T, opts ...wslMachineOption) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fakes are shell scripts, and the bridge runs from the Linux side")
	}
	var machine wslMachine
	for _, opt := range opts {
		opt(&machine)
	}
	dir := t.TempDir()
	fakeWindowsTools(t, dir, machine.leakyKeyACL)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

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

// A machine whose own ssh cannot be resolved is an error, not a remark about
// Windows. Both failures came back on one error return once, every caller read
// that return as the Windows one, and the work then carried on with nothing to
// write for at all.
func TestMachineSSHTargetsSeparatesThisMachineFromWindows(t *testing.T) {
	setHome(t, "")
	targets, err := machineSSHTargets(t.Context())
	if err == nil {
		t.Fatal("expected a machine with no home directory to be an error")
	}
	if targets.windowsErr != nil {
		t.Fatalf("this machine's failure came back as the Windows side's: %v", targets.windowsErr)
	}
	if len(targets.all) != 0 {
		t.Fatalf("targets = %v, want a value with nothing in it", targets.all)
	}
}

// The same at the call site, where the mislabel had its consequence: `--write`
// noted a Windows failure it had not had, wrote no config for anything, and
// returned success.
func TestSSHConfigWriteWithoutAHomeDirectorySaysWhichSSHIsMissing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	setHome(t, "")
	server := writeFakeServer().start(t)
	cmd := NewRootCommand()
	var out, errOut strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "ssh-config", "--write"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("a write with nowhere to write reported success")
	}
	if !strings.Contains(err.Error(), "own ssh") {
		t.Fatalf("the error should name which ssh could not be found, got: %v", err)
	}
	if strings.Contains(errOut.String(), "not writing the Windows ssh_config") {
		t.Fatalf("this machine's failure was reported as the Windows side's:\n%s", errOut.String())
	}
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
//
// How it is quoted is the whole of ADR 0078 §1. wsl.exe does not strip the
// quotes cmd.exe leaves in place, so a word it reads itself must not carry any
// -- quoting them got the Linux side an execvp of a program whose name began
// with a double quote, and ssh a UTF-16 error message where the banner should
// have been.
func TestSSHTargetProxyCommandReEntersWSL(t *testing.T) {
	target := wslTestTarget(t.TempDir())
	line, err := target.proxyCommandLine("http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("proxyCommandLine: %v", err)
	}
	if !strings.HasPrefix(line, "wsl.exe -d Ubuntu -e sh -c \"exec '") {
		t.Fatalf("ProxyCommand does not re-enter the distribution unquoted: %q", line)
	}
	// The endpoint is quoted for the shell that parses it, which is sh.
	if !strings.Contains(line, `--server 'http://127.0.0.1:8080' admin ssh-proxy`) {
		t.Fatalf("ProxyCommand does not quote the endpoint for sh: %q", line)
	}
	// Exactly one double-quoted argument, so cmd.exe and wsl.exe have nothing
	// to disagree about and no nesting to get wrong.
	if got := strings.Count(line, `"`); got != 2 {
		t.Fatalf("ProxyCommand carries %d double quotes, want the 2 around the sh script: %q", got, line)
	}
}

// The key exists on both sides of the boundary because Windows ssh.exe opens
// the file itself, and it is refreshed on every run so a rotated key cannot
// leave a stale one behind.
func TestSSHTargetMirrorsTheIdentityAcrossTheBoundary(t *testing.T) {
	tools := wslTestWindowsTools(t)
	target := wslTestTarget(t.TempDir())
	source := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(source, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source+".pub", []byte("PUBLIC"), 0o600); err != nil {
		t.Fatal(err)
	}

	mirrored, err := target.mirrorSSHIdentity(t.Context(), source)
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

	// The copy is no use to ssh unless its ACL is the one ssh reads a private
	// key under: the user, SYSTEM and Administrators, granted rather than
	// inherited, and the well-known two by SID because their names are
	// localized (ADR 0102).
	acl := readFile(t, aclLog(tools))
	for _, want := range []string{
		mirrored.client, "/inheritance:r", "/remove:g *S-1-5-32",
		"/grant:r Ada:F", "/grant:r *S-1-5-18:F", "/grant:r *S-1-5-32-544:F",
	} {
		if !strings.Contains(acl, want) {
			t.Fatalf("the key's ACL was not set with %q:\n%s", want, acl)
		}
	}
	if strings.Contains(acl, ".pub") {
		t.Fatalf("the public half was restricted too, which it need not be:\n%s", acl)
	}

	// A rotated key replaces the copy rather than being ignored.
	if err := os.WriteFile(source, []byte("ROTATED"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := target.mirrorSSHIdentity(t.Context(), source); err != nil {
		t.Fatalf("mirrorSSHIdentity again: %v", err)
	}
	if got := readFile(t, mirrored.local); got != "ROTATED" {
		t.Fatalf("mirrored key after rotation = %q", got)
	}
}

// A copy this cannot make safe does not stay on the Windows filesystem. The
// caller above may turn the failure into a warning and carry on (ADR 0102 §3),
// and a run that went on to succeed having left an enrolled private key under
// the ACL it just refused is the thing the check exists to prevent.
func TestSSHTargetRemovesAKeyItCannotNarrow(t *testing.T) {
	wslTestWindowsTools(t, withLeakyKeyACL)
	target := wslTestTarget(t.TempDir())
	source := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(source, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Where the copy would have landed, worked out here rather than taken from
	// the return: a failed mirror reports no path at all, which is the whole
	// reason the file it wrote has to be gone.
	copied := target.join(target.sshDir(), "id_ed25519").local
	_, err := target.mirrorSSHIdentity(t.Context(), source)
	if err == nil {
		t.Fatal("expected a key another principal can read to be refused")
	}
	if !strings.Contains(err.Error(), `BUILTIN\Users`) {
		t.Fatalf("the error should name what can read the key, got: %v", err)
	}
	if _, err := os.Stat(copied); !os.IsNotExist(err) {
		t.Fatalf("the copy was left behind at %s: %v", copied, err)
	}
}

// A key the user enrolled by hand has no .pub beside it more often than not,
// and ssh derives the public half from the private one anyway.
func TestSSHTargetMirrorsAKeyWithNoPublicHalf(t *testing.T) {
	wslTestWindowsTools(t)
	target := wslTestTarget(t.TempDir())
	source := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(source, []byte("PRIVATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	mirrored, err := target.mirrorSSHIdentity(t.Context(), source)
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
	mirrored, err := target.mirrorSSHIdentity(t.Context(), "/home/u/.local/state/discobox/cli/ssh/id_ed25519")
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

	edits, err := target.ensureUserConfigInclude(managed)
	if err != nil {
		t.Fatalf("ensureUserConfigInclude: %v", err)
	}
	if !edits.added || len(edits.dropped) != 0 {
		t.Fatalf("edits = %+v, want a single new line", edits)
	}
	first := readFile(t, target.userConfig.local)
	if want := `Include "` + managed.client + `"` + "\n"; first != want {
		t.Fatalf("user config = %q, want %q", first, want)
	}

	edits, err = target.ensureUserConfigInclude(managed)
	if err != nil {
		t.Fatalf("ensureUserConfigInclude again: %v", err)
	}
	if edits.added {
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

	edits, err := target.ensureUserConfigInclude(managed)
	if err != nil {
		t.Fatalf("ensureUserConfigInclude: %v", err)
	}
	if !edits.added || len(edits.dropped) != 1 || edits.dropped[0] != target.clean(stale) {
		t.Fatalf("edits = %+v, want the stale line gone", edits)
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
