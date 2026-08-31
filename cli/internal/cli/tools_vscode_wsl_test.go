package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeWSLMachine stands up the Windows half of a WSL machine: a mounted drive,
// the interop tools that translate between the two spellings of a path and
// answer for the Windows environment, and a Windows VS Code installed on that
// drive. It returns where the drive is mounted and the file the editor records
// its arguments to.
//
// It is fakeable because everything the WSL branch does across the boundary
// goes through a program — `wslpath`, `cmd.exe`, the editor itself — and a
// program on PATH is a program a test can write.
func fakeWSLMachine(t *testing.T, opts ...wslMachineOption) (root, record string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fakes are shell scripts")
	}
	machine := wslMachine{windowsPathOnPATH: true}
	for _, opt := range opts {
		opt(&machine)
	}
	base := t.TempDir()
	root = filepath.Join(base, "mnt", "c")
	tools := filepath.Join(base, "tools")
	// A Windows VS Code lives on the Windows drive, which is how this command
	// tells it apart from a Linux build in the distribution's own filesystem.
	editorDir := filepath.Join(root, "Program Files", "Microsoft VS Code", "bin")
	for _, dir := range []string{root, tools, editorDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	record = filepath.Join(base, "argv")
	writeFakeTool(t, filepath.Join(editorDir, "code"),
		"#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+record+"'\n")

	// wslpath translates both ways, and says of a path in the distribution
	// exactly what the real one says: it is reachable only as a UNC share.
	writeFakeTool(t, filepath.Join(tools, "wslpath"), strings.ReplaceAll(`#!/bin/sh
root='__ROOT__'
case "$1" in
-w)
	case "$2" in
	"$root"/*) printf 'C:%s\n' "$(printf '%s' "${2#$root}" | tr / '\\')" ;;
	*) printf '\\\\wsl.localhost\\Ubuntu%s\n' "$(printf '%s' "$2" | tr / '\\')" ;;
	esac ;;
-u)
	case "$2" in
	C:*) printf '%s%s\n' "$root" "$(printf '%s' "${2#C:}" | tr '\\' /)" ;;
	*) printf '%s\n' "$2" ;;
	esac ;;
esac
`, "__ROOT__", root))

	system32 := filepath.Join(root, "Windows", "System32")
	if err := os.MkdirAll(system32, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeWindowsTools(t, system32, machine.leakyKeyACL)
	// The Windows programs are on PATH because WSL appends the Windows PATH,
	// unless a test says this is a distribution configured not to.
	if machine.windowsPathOnPATH {
		for _, tool := range []string{"cmd.exe", "icacls.exe"} {
			linkFakeTool(t, filepath.Join(system32, tool), filepath.Join(tools, tool))
		}
	}

	t.Setenv(wslDistroEnv, "Ubuntu")
	t.Setenv("PATH", strings.Join([]string{tools, editorDir, os.Getenv("PATH")}, string(os.PathListSeparator)))
	t.Setenv(vscodeEditorEnv, "")
	return root, record
}

// wslMachine is what a test can vary about the fake machine.
type wslMachine struct {
	// windowsPathOnPATH is WSL's default of appending the Windows PATH to this
	// distribution's. A distribution with appendWindowsPath=false has none of
	// it, and the Windows programs are still installed.
	windowsPathOnPATH bool
	// leakyKeyACL makes icacls report a key another principal can still read,
	// which is the state ssh refuses to use one in.
	leakyKeyACL bool
}

type wslMachineOption func(*wslMachine)

func withoutWindowsPATH(m *wslMachine) { m.windowsPathOnPATH = false }

func withLeakyKeyACL(m *wslMachine) { m.leakyKeyACL = true }

func linkFakeTool(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Symlink(from, to); err != nil {
		t.Fatalf("link %s: %v", to, err)
	}
}

// fakeWindowsTools writes the Windows programs the bridge shells out to: cmd.exe
// for the environment WSL does not inherit, and icacls for the ACL a mirrored
// key needs. The fake icacls records what it was asked to set and answers for
// what the file has, so a test can have it report a key another principal can
// still read -- the state ssh refuses to use one in.
func fakeWindowsTools(t *testing.T, dir string, leakyKeyACL bool) {
	t.Helper()
	writeFakeTool(t, filepath.Join(dir, "cmd.exe"), `#!/bin/sh
case "$*" in
*USERPROFILE*) printf 'C:\Users\Ada Lovelace\r\n' ;;
*LOCALAPPDATA*) printf 'C:\Users\Ada Lovelace\AppData\Local\r\n' ;;
*USERNAME*) printf 'Ada\r\n' ;;
*) exit 1 ;;
esac
`)
	leak := ""
	if leakyKeyACL {
		leak = `\n                     BUILTIN\\Users:(RX)`
	}
	writeFakeTool(t, filepath.Join(dir, "icacls.exe"), `#!/bin/sh
path=$1
shift
if [ $# -gt 0 ]; then
	printf '%s %s\n' "$path" "$*" >> '`+aclLog(dir)+`'
	echo "Successfully processed 1 files; Failed processing 0 files"
	exit 0
fi
printf '%s BEENIE\\Ada:(F)`+leak+`\n' "$path"
`)
}

// aclLog is where the fake icacls records the ACLs it was asked to set.
func aclLog(dir string) string { return filepath.Join(dir, "icacls-log") }

// windowsToolsDir is where fakeWSLMachine keeps them, for a test that wants to
// read what icacls was asked to do.
func windowsToolsDir(root string) string { return filepath.Join(root, "Windows", "System32") }

func writeFakeTool(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // G306: it has to be executable.
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestToolsVSCodeOnWSLWritesTheConfigWindowsReads is the whole WSL bridge: the
// editor is a Windows program, so the ssh that connects for it is Windows
// ssh.exe — which reads a config under the Windows profile, opens files by
// their drive path, and cannot execute this CLI without re-entering the
// distribution. See ADR 0074.
func TestToolsVSCodeOnWSLWritesTheConfigWindowsReads(t *testing.T) {
	root, record := fakeWSLMachine(t)
	home, _, stderr, err := runToolsVSCodeCmd(t, vscodeFakeServer(), "--discobox-id", "sbx_devbox00000001")
	if err != nil {
		t.Fatalf("execute tools vscode: %v", err)
	}

	windowsState := filepath.Join(root, "Users", "Ada Lovelace", "AppData", "Local", "discobox", "cli", "ssh")
	config := readFile(t, filepath.Join(windowsState, resolvedTestProjectID, "config"))
	for _, want := range []string{
		// Windows ssh.exe cannot run a Linux binary: the ProxyCommand
		// re-enters the distribution and runs it there, quoted for the shell
		// that parses each word rather than for %COMSPEC% (ADR 0078 §1).
		`    ProxyCommand wsl.exe -d Ubuntu -e sh -c "exec '`,
		` admin ssh-proxy"` + "\n",
		// Every file the config names is named the way Windows spells it, and
		// quoted because the profile has a space in it.
		`    IdentityFile "C:\Users\Ada Lovelace\AppData\Local\discobox\cli\ssh\id_ed25519"` + "\n",
		`    UserKnownHostsFile "C:\Users\Ada Lovelace\AppData\Local\discobox\cli\ssh\` + resolvedTestProjectID + `\known_hosts"` + "\n",
		"Host devbox ",
	} {
		if !strings.Contains(config, want) {
			t.Fatalf("the Windows config is missing %q:\n%s", want, config)
		}
	}

	// The key is on the Windows side too, because ssh.exe opens it itself --
	// and narrowed to this user, because ssh refuses to read a private key
	// anybody else can (ADR 0078 §2).
	if _, err := os.Stat(filepath.Join(windowsState, "id_ed25519")); err != nil {
		t.Fatalf("the identity was not mirrored for Windows: %v", err)
	}
	acl := readFile(t, aclLog(windowsToolsDir(root)))
	if !strings.Contains(acl, `/inheritance:r /remove:g *S-1-5-32 /grant:r Ada:F`) {
		t.Fatalf("the mirrored key's ACL was not narrowed:\n%s", acl)
	}

	// The Include goes in the Windows user's ssh_config, quoted.
	userConfig := readFile(t, filepath.Join(root, "Users", "Ada Lovelace", ".ssh", "config"))
	if want := `Include "C:\Users\Ada Lovelace\AppData\Local\discobox\cli\ssh\` + resolvedTestProjectID + `\config"` + "\n"; userConfig != want {
		t.Fatalf("Windows ssh_config = %q, want %q", userConfig, want)
	}
	// And this distribution's own ssh is configured too: the machine has two,
	// and the other one is what `git` and `scp` here will use.
	if got := readFile(t, filepath.Join(home, ".ssh", "config")); !strings.Contains(got, "discobox/cli/ssh/"+resolvedTestProjectID) {
		t.Fatalf("the distribution's own ssh_config was not written: %q", got)
	}
	_ = stderr

	// A folder URI, never a bare path: a path argument is what the Windows
	// launcher rewrites into a window on the local WSL directory.
	want := []string{"--new-window", "--folder-uri", "vscode-remote://ssh-remote+devbox/home/agent/repo"}
	if got := editorArgs(t, record); !equalStrings(got, want) {
		t.Fatalf("editor args = %v, want %v", got, want)
	}
}

// A WSL machine has two ssh installations and both get the stanzas, whichever
// editor is being launched. A Linux editor drives this distribution's ssh, and
// the Windows config is written anyway, because the next thing to reach for a
// discobox may be a Windows program.
func TestToolsVSCodeOnWSLWritesBothSidesForALinuxEditor(t *testing.T) {
	root, _ := fakeWSLMachine(t)
	record := fakeVSCode(t)

	home, state, _, err := runToolsVSCodeCmd(t, vscodeFakeServer(), "--discobox-id", "sbx_devbox00000001")
	if err != nil {
		t.Fatalf("execute tools vscode: %v", err)
	}
	configPath, _ := managedPaths(state)
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("the local config was not written: %v", err)
	}
	if got := readFile(t, filepath.Join(home, ".ssh", "config")); !strings.Contains(got, configPath) {
		t.Fatalf("the local ssh_config does not include the managed one: %q", got)
	}
	windowsConfig := filepath.Join(root, "Users", "Ada Lovelace", "AppData", "Local",
		"discobox", "cli", "ssh", resolvedTestProjectID, "config")
	if _, err := os.Stat(windowsConfig); err != nil {
		t.Fatalf("the Windows config was not written for a Linux editor: %v", err)
	}
	// The editor here is a Linux build, so its own stanza names this
	// executable directly rather than re-entering the distribution.
	if got := readFile(t, configPath); strings.Contains(got, "wsl.exe") {
		t.Fatalf("the distribution's own config re-enters WSL to reach itself:\n%s", got)
	}
	if args := editorArgs(t, record); !contains(args, "--folder-uri") {
		t.Fatalf("editor args = %v, want a folder URI", args)
	}
}

// A distribution that does not inherit the Windows PATH still has Windows on
// the disk: cmd.exe is found where Windows keeps it, translated through the
// mount rather than guessed at.
func TestToolsVSCodeOnWSLWithoutTheWindowsPATH(t *testing.T) {
	root, _ := fakeWSLMachine(t, withoutWindowsPATH)
	if _, _, _, err := runToolsVSCodeCmd(t, vscodeFakeServer(), "--discobox-id", "sbx_devbox00000001"); err != nil {
		t.Fatalf("execute tools vscode: %v", err)
	}
	windowsConfig := filepath.Join(root, "Users", "Ada Lovelace", "AppData", "Local",
		"discobox", "cli", "ssh", resolvedTestProjectID, "config")
	if _, err := os.Stat(windowsConfig); err != nil {
		t.Fatalf("the Windows config was not written: %v", err)
	}
}

// A key ssh will not read is worth failing over here, where the reason is
// known, rather than leaving Remote-SSH to say "Permissions for … are too open"
// about a file the user never created.
func TestToolsVSCodeOnWSLRefusesALeakyKey(t *testing.T) {
	fakeWSLMachine(t, withLeakyKeyACL)
	_, _, _, err := runToolsVSCodeCmd(t, vscodeFakeServer(), "--discobox-id", "sbx_devbox00000001")
	if err == nil {
		t.Fatal("expected tools vscode to refuse a key another principal can read")
	}
	for _, want := range []string{"BUILTIN\\Users", "private key"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should name what can read the key and why it matters, got: %v", err)
		}
	}
}

// `admin ssh-config --write` on a WSL machine configures both of its ssh
// installations. Which one a given tool drives is not something this command
// can know -- Remote-SSH and JetBrains Gateway are Windows programs, `git` and
// `scp` here are not -- and it was `tools vscode` being the only way to get the
// Windows one that made this worth fixing.
func TestSSHConfigWriteOnWSLWritesBothSides(t *testing.T) {
	root, _ := fakeWSLMachine(t)
	home, state := t.TempDir(), t.TempDir()
	setHome(t, home)
	t.Setenv("XDG_STATE_HOME", state)

	server := writeFakeServer().start(t)
	cmd := NewRootCommand()
	var out, errOut strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "ssh-config", "--write"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute ssh-config --write: %v", err)
	}

	local, _ := managedPaths(state)
	if got := readFile(t, local); strings.Contains(got, "wsl.exe") {
		t.Fatalf("this distribution's config re-enters WSL to reach itself:\n%s", got)
	}
	windows := readFile(t, filepath.Join(root, "Users", "Ada Lovelace", "AppData", "Local",
		"discobox", "cli", "ssh", resolvedTestProjectID, "config"))
	if !strings.Contains(windows, `ProxyCommand wsl.exe -d Ubuntu -e sh -c "exec '`) {
		t.Fatalf("the Windows config does not re-enter the distribution:\n%s", windows)
	}
	// Each side's Include lands in its own ssh_config.
	if got := readFile(t, filepath.Join(home, ".ssh", "config")); !strings.Contains(got, local) {
		t.Fatalf("this distribution's ssh_config has no Include: %q", got)
	}
	if got := readFile(t, filepath.Join(root, "Users", "Ada Lovelace", ".ssh", "config")); !strings.Contains(got, `C:\Users\Ada Lovelace\AppData\Local`) {
		t.Fatalf("the Windows ssh_config has no Include: %q", got)
	}
}

// Printed output is for pasting into a config by hand, and the hand doing that
// is on this side: one config, not two.
func TestSSHConfigPrintOnWSLIsThisSideOnly(t *testing.T) {
	fakeWSLMachine(t)
	setHome(t, t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	server := writeFakeServer().start(t)
	cmd := NewRootCommand()
	var out, errOut strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"--server", server.URL, "--project", "project-1", "admin", "ssh-config"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute ssh-config: %v", err)
	}
	if strings.Contains(out.String(), "wsl.exe") {
		t.Fatalf("printed a config for the other side:\n%s", out.String())
	}
	if strings.Count(out.String(), "ProxyCommand") != 1 {
		t.Fatalf("printed more than one machine's stanzas:\n%s", out.String())
	}
}
