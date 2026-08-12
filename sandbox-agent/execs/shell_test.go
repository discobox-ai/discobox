package execs

import (
	"context"
	"testing"

	"github.com/obot-platform/discobox/sandbox-agent/runuser"
)

// writePasswd is now runuser's business: the passwd format is parsed in one
// place (ADR 0032 §6), so these tests use its fixture rather than a second
// pointer at a second file.
func writePasswd(t *testing.T, _ string) {
	t.Helper()
	t.Cleanup(runuser.FixedDatabase())
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
		// $SHELL is deliberately something else: the run user's own passwd entry
		// is the authority on their shell, not the environment the agent happens
		// to carry.
		Env:         map[string]string{"SHELL": "/bin/ignored"},
		Units:       runner,
		DefaultUser: &User{Name: "dev"},
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

// With no user configured anywhere, the shell is the image account's own -- the
// exec inherits that identity, so it inherits its shell too.
func TestManagerFallsBackToTheImageAccountsShell(t *testing.T) {
	writePasswd(t, "")
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Env:         map[string]string{"SHELL": "/bin/ignored"},
		Units:       &fakeUnitManager{},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	exec, err := manager.Create(context.Background(), CreateRequest{Shell: true, TTY: true})
	if err != nil {
		t.Fatalf("create shell exec: %v", err)
	}
	if exec.Command[0] != "/bin/bash" {
		t.Fatalf("command = %v, want the image account's /bin/bash", exec.Command)
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

func int64ptr(v int64) *int64 { return &v }

// A startup command only makes sense typed into a shell, so it requires Shell
// and never changes the resolved argv: the shell is still what actually runs.
func TestResolveCommandStartupCommandRequiresShell(t *testing.T) {
	writePasswd(t, "")

	if _, err := resolveCommand(CreateRequest{StartupCommand: []string{"claude"}}, nil, nil); err == nil {
		t.Fatal("startup command without shell was accepted")
	}
	command, err := resolveCommand(CreateRequest{Shell: true, StartupCommand: []string{"claude"}}, nil, map[string]string{"SHELL": "/bin/bash"})
	if err != nil {
		t.Fatalf("resolve shell+startup command: %v", err)
	}
	if len(command) != 2 || command[0] != "/bin/bash" || command[1] != "-l" {
		t.Fatalf("command = %v, want the login shell (startup command is typed in, not exec'd)", command)
	}
}

// The exec record reports both: Command is the literal argv actually executed
// (the shell), StartupCommand is what was typed into it and what the launched
// unit is told to type in turn.
func TestManagerReportsStartupCommandSeparatelyFromCommand(t *testing.T) {
	writePasswd(t, "")
	runner := &fakeUnitManager{}
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Env:         map[string]string{"SHELL": "/bin/bash"},
		Units:       runner,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	exec, err := manager.Create(context.Background(), CreateRequest{
		Shell:          true,
		StartupCommand: []string{"claude", "do a thing"},
		TTY:            true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(exec.Command) != 2 || exec.Command[0] != "/bin/bash" || exec.Command[1] != "-l" {
		t.Fatalf("exec.Command = %v, want the login shell", exec.Command)
	}
	want := []string{"claude", "do a thing"}
	if len(exec.StartupCommand) != 2 || exec.StartupCommand[0] != want[0] || exec.StartupCommand[1] != want[1] {
		t.Fatalf("exec.StartupCommand = %v, want %v", exec.StartupCommand, want)
	}
	if len(runner.starts) != 1 {
		t.Fatalf("expected one unit start, got %d", len(runner.starts))
	}
	started := runner.starts[0].StartupCommand
	if len(started) != 2 || started[0] != want[0] || started[1] != want[1] {
		t.Fatalf("started unit startupCommand = %v, want %v", started, want)
	}
}

// The bytes typed into the shell are quoted so the command runs exactly as
// given, argument boundaries included, even when an argument itself contains
// shell metacharacters.
func TestQuoteShellCommand(t *testing.T) {
	got := string(QuoteShellCommand([]string{"claude", "do a thing", "it's fine"}))
	want := `'claude' 'do a thing' 'it'\''s fine'` + "\n"
	if got != want {
		t.Fatalf("quoted = %q, want %q", got, want)
	}
	if QuoteShellCommand(nil) != nil {
		t.Fatal("empty argv must produce no bytes to inject")
	}
}

func TestResolveCommandShellCommandLine(t *testing.T) {
	writePasswd(t, "")

	if _, err := resolveCommand(CreateRequest{Shell: true, Command: []string{"ls"}, ShellCommandLine: "ls -la"}, nil, nil); err == nil {
		t.Fatal("shell with a command was accepted")
	}

	command, err := resolveCommand(CreateRequest{Shell: true, ShellCommandLine: "ls -la"}, nil, map[string]string{"SHELL": "/bin/bash"})
	if err != nil {
		t.Fatalf("resolve shell command line: %v", err)
	}
	want := []string{"/bin/bash", "-lc", "ls -la"}
	if len(command) != len(want) {
		t.Fatalf("command = %v, want %v", command, want)
	}
	for i := range want {
		if command[i] != want[i] {
			t.Fatalf("command = %v, want %v", command, want)
		}
	}
}
