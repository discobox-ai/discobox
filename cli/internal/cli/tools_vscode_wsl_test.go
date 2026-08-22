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

	// cmd.exe answers for the Windows environment, in CRLF as the real one
	// does, from a profile whose name has a space in it as real ones do. It
	// goes where Windows keeps it, and is linked onto PATH unless a test says
	// this is a distribution that does not inherit the Windows PATH.
	system32 := filepath.Join(root, "Windows", "System32")
	if err := os.MkdirAll(system32, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFakeTool(t, filepath.Join(system32, "cmd.exe"), `#!/bin/sh
case "$*" in
*USERPROFILE*) printf 'C:\Users\Ada Lovelace\r\n' ;;
*LOCALAPPDATA*) printf 'C:\Users\Ada Lovelace\AppData\Local\r\n' ;;
*) exit 1 ;;
esac
`)

	machine := wslMachine{windowsPathOnPATH: true}
	for _, opt := range opts {
		opt(&machine)
	}
	if machine.windowsPathOnPATH {
		linkFakeTool(t, filepath.Join(system32, "cmd.exe"), filepath.Join(tools, "cmd.exe"))
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
}

type wslMachineOption func(*wslMachine)

func withoutWindowsPATH(m *wslMachine) { m.windowsPathOnPATH = false }

func linkFakeTool(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Symlink(from, to); err != nil {
		t.Fatalf("link %s: %v", to, err)
	}
}

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
// distribution. See ADR 0068.
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
		// re-enters the distribution and runs it there.
		`    ProxyCommand wsl.exe -d "Ubuntu" -e "`,
		` admin ssh-proxy` + "\n",
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

	// The key is on the Windows side too, because ssh.exe opens it itself.
	if _, err := os.Stat(filepath.Join(windowsState, "id_ed25519")); err != nil {
		t.Fatalf("the identity was not mirrored for Windows: %v", err)
	}

	// The Include goes in the Windows user's ssh_config, quoted, and nothing
	// is written into the distribution's own ~/.ssh at all.
	userConfig := readFile(t, filepath.Join(root, "Users", "Ada Lovelace", ".ssh", "config"))
	if want := `Include "C:\Users\Ada Lovelace\AppData\Local\discobox\cli\ssh\` + resolvedTestProjectID + `\config"` + "\n"; userConfig != want {
		t.Fatalf("Windows ssh_config = %q, want %q", userConfig, want)
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh", "config")); !os.IsNotExist(err) {
		t.Fatalf("the WSL ~/.ssh/config was written for an editor that cannot read it: %v", err)
	}

	// Which side the config was written for is not something to leave the user
	// to work out from a path.
	if !strings.Contains(stderr, "Windows OpenSSH") {
		t.Fatalf("stderr does not say which ssh the config was written for: %q", stderr)
	}

	// A folder URI, never a bare path: a path argument is what the Windows
	// launcher rewrites into a window on the local WSL directory.
	want := []string{"--new-window", "--folder-uri", "vscode-remote://ssh-remote+devbox/home/agent/repo"}
	if got := editorArgs(t, record); !equalStrings(got, want) {
		t.Fatalf("editor args = %v, want %v", got, want)
	}
}

// A Linux editor on a WSL machine is still a Linux editor: it drives this
// distribution's ssh, and nothing crosses the boundary.
func TestToolsVSCodeOnWSLKeepsALinuxEditorLocal(t *testing.T) {
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
	if _, err := os.Stat(filepath.Join(root, "Users")); !os.IsNotExist(err) {
		t.Fatal("a Linux editor caused a write into the Windows profile")
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
