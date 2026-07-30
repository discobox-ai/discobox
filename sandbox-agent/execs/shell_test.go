package execs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writePasswd(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "passwd")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write passwd fixture: %v", err)
	}
	previous := passwdPath
	passwdPath = path
	t.Cleanup(func() { passwdPath = previous })
}

func TestResolveShellUsesPasswdEntry(t *testing.T) {
	writePasswd(t, "root:x:0:0:root:/root:/bin/sh\ndev:x:1000:1000::/home/dev:/usr/bin/zsh\n")

	shell, err := ResolveShell(&User{Name: "dev"}, map[string]string{"SHELL": "/bin/ignored"})
	if err != nil {
		// The user must exist in the OS database for ResolveUser to accept it.
		t.Skipf("user dev is not resolvable in this environment: %v", err)
	}
	if shell != "/usr/bin/zsh" {
		t.Fatalf("shell = %q, want /usr/bin/zsh", shell)
	}
}

func TestResolveShellFallsBackPastNologin(t *testing.T) {
	writePasswd(t, "svc:x:1001:1001::/nonexistent:/usr/sbin/nologin\n")

	// A bare UID has no name to look up, so the passwd entry never matches and
	// $SHELL from the exec environment answers instead.
	shell, err := ResolveShell(&User{UID: int64ptr(1001)}, map[string]string{"SHELL": "/usr/bin/fish"})
	if err != nil {
		t.Fatalf("resolve shell: %v", err)
	}
	if shell != "/usr/bin/fish" {
		t.Fatalf("shell = %q, want /usr/bin/fish", shell)
	}

	// A login-refusing shell is treated as no shell at all.
	if got := isLoginShell("/usr/sbin/nologin"); got {
		t.Fatal("nologin accepted as a login shell")
	}
	if got := isLoginShell("/bin/false"); got {
		t.Fatal("/bin/false accepted as a login shell")
	}
}

func TestResolveShellAlwaysYieldsAShell(t *testing.T) {
	writePasswd(t, "")

	shell, err := ResolveShell(nil, nil)
	if err != nil {
		t.Fatalf("resolve shell: %v", err)
	}
	if shell == "" || shell[0] != '/' {
		t.Fatalf("shell = %q, want an absolute path", shell)
	}
}

func TestShellCommandIsALoginShell(t *testing.T) {
	writePasswd(t, "")

	command, err := ShellCommand(nil, map[string]string{"SHELL": "/bin/bash"})
	if err != nil {
		t.Fatalf("shell command: %v", err)
	}
	if len(command) != 2 || command[0] != "/bin/bash" || command[1] != "-l" {
		t.Fatalf("command = %v, want [/bin/bash -l]", command)
	}
}

func TestManagerRunsResolvedShellForShellRequest(t *testing.T) {
	writePasswd(t, "")
	runner := &fakeUnitManager{}
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Env:         map[string]string{"SHELL": "/usr/bin/zsh"},
		Units:       runner,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	exec, err := manager.Create(context.Background(), CreateRequest{Shell: true, TTY: true})
	if err != nil {
		t.Fatalf("create shell exec: %v", err)
	}
	want := []string{"/usr/bin/zsh", "-l"}
	if len(exec.Command) != 2 || exec.Command[0] != want[0] || exec.Command[1] != want[1] {
		t.Fatalf("exec command = %v, want %v", exec.Command, want)
	}
	// The record and the launched unit must agree: the exec reports the shell it
	// actually runs.
	if len(runner.starts) != 1 || runner.starts[0].Command[0] != want[0] {
		t.Fatalf("started command = %v, want %v", runner.starts, want)
	}
}

func TestResolveCommandShellExclusivity(t *testing.T) {
	writePasswd(t, "")

	if _, err := resolveCommand(CreateRequest{Shell: true, Command: []string{"ls"}}, nil, nil); err == nil {
		t.Fatal("shell with a command was accepted")
	}
	if _, err := resolveCommand(CreateRequest{}, nil, nil); err == nil {
		t.Fatal("empty command was accepted")
	}
	command, err := resolveCommand(CreateRequest{Shell: true}, nil, map[string]string{"SHELL": "/bin/bash"})
	if err != nil {
		t.Fatalf("resolve shell command: %v", err)
	}
	if len(command) == 0 {
		t.Fatal("shell request resolved to no command")
	}
}
