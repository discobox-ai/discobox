package terminal

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// newConfigureService is the sandbox that exists to run a harness's setup once,
// rather than to be worked in.
func newConfigureService(t *testing.T) (*Service, *fakeUnits) {
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
		Harness:     config.Harness{ID: "codex", Command: []string{"/usr/local/libexec/discobox/configure-codex"}},
		HarnessMode: configHarnessMode,
		Units:       units,
		Installer:   &noopInstaller{},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc, units
}

// A configure flow runs its command as the exec itself, not as a job typed into
// a login shell.
//
// The whole point of the flow is the command's exit status: the server reads it
// to decide whether the setup worked. A command typed into a shell has none the
// exec can report — the shell outlives it, so the terminal never reaches
// "exited" until somebody types exit, and the code it reports then is the
// shell's, not the setup's.
func TestConfigureRunsItsCommandAsTheExec(t *testing.T) {
	svc, _ := newConfigureService(t)

	created, err := svc.Create(context.Background(), CreateRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if !slices.Equal(created.Command, []string{"/usr/local/libexec/discobox/configure-codex"}) {
		t.Fatalf("exec command = %v, want the configure command itself", created.Command)
	}
	if len(created.StartupCommand) != 0 {
		t.Fatalf("startup command = %v, want nothing typed into a shell", created.StartupCommand)
	}
}

// A terminal you sit in front of still gets the login shell and its job
// control: this is the exception, not the new rule.
func TestANormalHarnessStillRunsInsideALoginShell(t *testing.T) {
	svc, _ := newTestService(t, &noopInstaller{})

	created, err := svc.Create(context.Background(), CreateRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if !slices.Equal(created.StartupCommand, []string{"codex"}) {
		t.Fatalf("startup command = %v, want the harness typed into the shell", created.StartupCommand)
	}
}
