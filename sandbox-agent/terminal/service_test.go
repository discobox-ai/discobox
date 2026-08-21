package terminal

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// newHarnesslessService builds a terminal service with no resolved harness, so
// harness resolution falls back to the shell.
func newHarnesslessService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	execManager, err := execs.NewManagerWithConfig(execs.ManagerConfig{
		WorkingRoot: dir,
		RuntimeDir:  filepath.Join(dir, "rt"),
		Env:         map[string]string{"PATH": "/usr/bin"},
		Units:       &fakeUnits{},
	})
	if err != nil {
		t.Fatalf("new exec manager: %v", err)
	}
	svc, err := NewService(ServiceConfig{
		Execs:       execManager,
		WorkingRoot: dir,
		RuntimeDir:  filepath.Join(dir, "rt"),
		Env:         map[string]string{"PATH": "/usr/bin"},
		Units:       &fakeUnits{},
		Installer:   &noopInstaller{},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// Every sandbox gets a default terminal, so a sandbox with no harness configured
// resolves to a shell rather than to nothing at all: clients (run) always have a
// terminal to attach to.
func TestTerminalFallsBackToShellWhenNoHarnessConfigured(t *testing.T) {
	svc := newHarnesslessService(t)
	terminal, err := svc.Create(context.Background(), CreateRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if HarnessID(terminal) != ShellHarnessID {
		t.Fatalf("harnessId = %q, want %q (metadata=%v)", HarnessID(terminal), ShellHarnessID, terminal.Metadata)
	}
	if len(terminal.Command) != 2 || !strings.HasSuffix(terminal.Command[0], "sh") || terminal.Command[1] != "-l" {
		t.Fatalf("command = %v, want a login shell", terminal.Command)
	}
	if !terminal.TTY {
		t.Fatal("shell terminal must allocate a TTY")
	}
	if len(terminal.StartupCommand) != 0 {
		t.Fatalf("shell fallback terminal must not type in a startup command, got %v", terminal.StartupCommand)
	}
}

type fakeUnits struct {
	starts []execs.StartRequest
}

func (f *fakeUnits) Start(_ context.Context, req execs.StartRequest) (execs.StartResult, error) {
	f.starts = append(f.starts, req)
	return execs.StartResult{Unit: req.Unit}, nil
}
func (f *fakeUnits) Stop(context.Context, string) error { return nil }
func (f *fakeUnits) Status(context.Context, string) (execs.UnitStatus, error) {
	return execs.UnitStatus{}, context.Canceled
}
func (f *fakeUnits) List(context.Context) ([]execs.UnitStatus, error) { return nil, nil }

type noopInstaller struct {
	calls []config.Harness
}

func (n *noopInstaller) EnsureInstalled(_ context.Context, harness config.Harness, _ string, _ map[string]string) error {
	n.calls = append(n.calls, harness)
	return nil
}

func (n *noopInstaller) RestoreSecretFiles(context.Context, config.Harness, map[string]string) ([]string, error) {
	return nil, nil
}

func newTestService(t *testing.T, installer Installer) (*Service, *fakeUnits) {
	t.Helper()
	dir := t.TempDir()
	units := &fakeUnits{}
	env := map[string]string{"PATH": "/usr/bin"}
	execManager, err := execs.NewManagerWithConfig(execs.ManagerConfig{
		WorkingRoot: dir,
		RuntimeDir:  filepath.Join(dir, "rt"),
		Env:         env,
		Units:       units,
	})
	if err != nil {
		t.Fatalf("new exec manager: %v", err)
	}
	svc, err := NewService(ServiceConfig{
		Execs:       execManager,
		WorkingRoot: dir,
		RuntimeDir:  filepath.Join(dir, "rt"),
		Env:         env,
		Harness:     config.Harness{ID: "codex", Command: []string{"codex"}},
		Units:       units,
		Installer:   installer,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc, units
}

// A terminal is an exec created in harness mode: it runs as the resolved login
// shell (TTY, real job control) with the harness command typed in as
// StartupCommand, tagged harnessId in metadata, after the installer runs for
// that harness.
func TestServiceCreateResolvesAgentAndTagsMetadata(t *testing.T) {
	installer := &noopInstaller{}
	svc, units := newTestService(t, installer)

	ex, err := svc.Create(context.Background(), CreateRequest{Args: []string{"hello"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if HarnessID(ex) != "codex" {
		t.Fatalf("harnessId = %q, want codex (metadata=%v)", HarnessID(ex), ex.Metadata)
	}
	if !ex.TTY {
		t.Fatalf("terminal exec must allocate a TTY")
	}
	if len(ex.Command) != 2 || !strings.HasSuffix(ex.Command[0], "sh") || ex.Command[1] != "-l" {
		t.Fatalf("command = %v, want a login shell (the harness runs as its typed-in startup command)", ex.Command)
	}
	if want := []string{"codex", "hello"}; len(ex.StartupCommand) != 2 || ex.StartupCommand[0] != want[0] || ex.StartupCommand[1] != want[1] {
		t.Fatalf("startupCommand = %v, want %v", ex.StartupCommand, want)
	}
	if len(units.starts) != 1 {
		t.Fatalf("expected one unit start, got %d", len(units.starts))
	}
	if len(installer.calls) != 1 || installer.calls[0].ID != "codex" {
		t.Fatalf("installer calls = %v", installer.calls)
	}
	if env := units.starts[0].Env["DISCOBOX_TERMINAL_ID"]; env != ex.ID {
		t.Fatalf("DISCOBOX_TERMINAL_ID = %q, want exec id %q", env, ex.ID)
	}
}

// hookInstaller runs a callback while "installing", so tests can observe the
// terminal state the mapper projects as the installing phase.
type hookInstaller struct {
	during func(ctx context.Context)
	err    error
}

func (h hookInstaller) EnsureInstalled(ctx context.Context, _ config.Harness, _ string, _ map[string]string) error {
	if h.during != nil {
		h.during(ctx)
	}
	return h.err
}

func (h hookInstaller) RestoreSecretFiles(context.Context, config.Harness, map[string]string) ([]string, error) {
	return nil, nil
}

// While the install command runs, the terminal record already exists and is
// marked installing, so callers can surface the phase instead of seeing nothing.
func TestServiceCreateMarksInstallingDuringInstall(t *testing.T) {
	var idDuringInstall string
	var installingDuringInstall bool
	installer := hookInstaller{during: func(context.Context) {}}
	svc, _ := newTestService(t, nil)
	// Rewire the service installer to one that can inspect svc mid-install.
	installer.during = func(context.Context) {
		execsDuring := svc.List()
		if len(execsDuring) == 1 {
			idDuringInstall = execsDuring[0].ID
			installingDuringInstall = svc.IsInstalling(idDuringInstall)
		}
	}
	svc.installer = installer

	ex, err := svc.Create(context.Background(), CreateRequest{primary: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if idDuringInstall != ex.ID {
		t.Fatalf("record id during install = %q, want created exec %q", idDuringInstall, ex.ID)
	}
	if !installingDuringInstall {
		t.Fatalf("exec should be marked installing while its install command runs")
	}
	if svc.IsInstalling(ex.ID) {
		t.Fatalf("exec should no longer be installing after Create returns")
	}
}

// A failing install command leaves no half-installed terminal behind.
func TestServiceCreateInstallFailureRemovesRecord(t *testing.T) {
	svc, _ := newTestService(t, hookInstaller{err: errors.New("install boom")})
	if _, err := svc.Create(context.Background(), CreateRequest{primary: true}); err == nil {
		t.Fatalf("expected install failure error")
	}
	if execsAfter := svc.List(); len(execsAfter) != 0 {
		t.Fatalf("failed install must remove the terminal record, got %d execs", len(execsAfter))
	}
}

// An explicit unknown harness is rejected; a known one is honored.
func TestServiceCreateExplicitAgent(t *testing.T) {
	svc, _ := newTestService(t, &noopInstaller{})
	if _, err := svc.Create(context.Background(), CreateRequest{HarnessID: "missing"}); err == nil {
		t.Fatalf("expected error for unknown harness")
	}
	if _, err := svc.Create(context.Background(), CreateRequest{HarnessID: "codex"}); err != nil {
		t.Fatalf("known harness create: %v", err)
	}
}

func TestPrimaryCreateRequest(t *testing.T) {
	harness := config.Harness{ID: "codex", Command: []string{"codex"}, RelaunchCommand: []string{"codex", "resume"}}
	// First start: prompt as args, no relaunch command.
	first := primaryCreateRequest(harness, "codex", []string{"do a thing"}, false)
	if !first.primary || len(first.command) != 0 || len(first.Args) != 1 {
		t.Fatalf("first start = %#v", first)
	}
	// Subsequent start: relaunch command replaces the run command.
	resume := primaryCreateRequest(harness, "codex", []string{"do a thing"}, true)
	if len(resume.command) != 2 || resume.command[1] != "resume" || len(resume.Args) != 0 {
		t.Fatalf("resume = %#v", resume)
	}
	// Subsequent start with no relaunch command: start bare, no prompt replay.
	bare := primaryCreateRequest(config.Harness{ID: "codex", Command: []string{"codex"}}, "codex", []string{"p"}, true)
	if len(bare.command) != 0 || len(bare.Args) != 0 {
		t.Fatalf("bare = %#v", bare)
	}
	// The shell fallback never takes the prompt as arguments, on any start.
	shell := primaryCreateRequest(config.Harness{ID: ShellHarnessID, Command: []string{"/bin/sh", "-l"}}, ShellHarnessID, []string{"p"}, false)
	if len(shell.command) != 0 || len(shell.Args) != 0 {
		t.Fatalf("shell = %#v", shell)
	}
}
