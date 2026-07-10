package terminal

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/obot-platform/discobox/sandbox-agent/config"
	"github.com/obot-platform/discobox/sandbox-agent/execs"
)

// newAgentlessService builds a terminal service with no agents configured, so
// agent resolution has nothing to select.
func newAgentlessService(t *testing.T) *Service {
	t.Helper()
	dir := t.TempDir()
	image := config.ImageConfig{Env: map[string]string{"PATH": "/usr/bin"}}
	execManager, err := execs.NewManagerWithConfig(execs.ManagerConfig{
		WorkingRoot: dir,
		RuntimeDir:  filepath.Join(dir, "rt"),
		ImageConfig: image,
		Units:       &fakeUnits{},
	})
	if err != nil {
		t.Fatalf("new exec manager: %v", err)
	}
	svc, err := NewService(ServiceConfig{
		Execs:       execManager,
		WorkingRoot: dir,
		RuntimeDir:  filepath.Join(dir, "rt"),
		ImageConfig: image,
		Units:       &fakeUnits{},
		Installer:   &noopInstaller{},
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc
}

// EnsurePrimary must surface the no-agent case as ErrNoAgentConfigured rather
// than silently returning nil: a silent no-op left clients waiting forever for a
// primary terminal that would never launch.
func TestEnsurePrimaryReturnsErrNoAgentConfigured(t *testing.T) {
	svc := newAgentlessService(t)
	err := svc.EnsurePrimary(context.Background(), nil)
	if !errors.Is(err, ErrNoAgentConfigured) {
		t.Fatalf("EnsurePrimary = %v, want ErrNoAgentConfigured", err)
	}
	if execs := svc.List(); len(execs) != 0 {
		t.Fatalf("List() = %d execs, want 0", len(execs))
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
	calls []config.Agent
}

func (n *noopInstaller) EnsureInstalled(_ context.Context, agent config.Agent, _ string, _ map[string]string) error {
	n.calls = append(n.calls, agent)
	return nil
}

func newTestService(t *testing.T, installer Installer) (*Service, *fakeUnits) {
	t.Helper()
	dir := t.TempDir()
	units := &fakeUnits{}
	image := config.ImageConfig{Env: map[string]string{"PATH": "/usr/bin"}}
	execManager, err := execs.NewManagerWithConfig(execs.ManagerConfig{
		WorkingRoot: dir,
		RuntimeDir:  filepath.Join(dir, "rt"),
		ImageConfig: image,
		Units:       units,
	})
	if err != nil {
		t.Fatalf("new exec manager: %v", err)
	}
	svc, err := NewService(ServiceConfig{
		Execs:       execManager,
		WorkingRoot: dir,
		RuntimeDir:  filepath.Join(dir, "rt"),
		ImageConfig: image,
		Agents:      []config.Agent{{ID: "codex", Command: []string{"codex"}, IsDefault: true}},
		Units:       units,
		Installer:   installer,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return svc, units
}

// A terminal is an exec created in agent mode: the resolved agent command runs
// with TTY, tagged agentId in metadata, after the installer runs for that agent.
func TestServiceCreateResolvesAgentAndTagsMetadata(t *testing.T) {
	installer := &noopInstaller{}
	svc, units := newTestService(t, installer)

	ex, err := svc.Create(context.Background(), CreateRequest{Args: []string{"hello"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if AgentID(ex) != "codex" {
		t.Fatalf("agentId = %q, want codex (metadata=%v)", AgentID(ex), ex.Metadata)
	}
	if !ex.TTY {
		t.Fatalf("terminal exec must allocate a TTY")
	}
	if want := []string{"codex", "hello"}; len(ex.Command) != 2 || ex.Command[0] != want[0] || ex.Command[1] != want[1] {
		t.Fatalf("command = %v, want %v", ex.Command, want)
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

// An explicit unknown agent is rejected; a known one is honored.
func TestServiceCreateExplicitAgent(t *testing.T) {
	svc, _ := newTestService(t, &noopInstaller{})
	if _, err := svc.Create(context.Background(), CreateRequest{AgentID: "missing"}); err == nil {
		t.Fatalf("expected error for unknown agent")
	}
	if _, err := svc.Create(context.Background(), CreateRequest{AgentID: "codex"}); err != nil {
		t.Fatalf("known agent create: %v", err)
	}
}

func TestPrimaryCreateRequest(t *testing.T) {
	agent := config.Agent{ID: "codex", Command: []string{"codex"}, RelaunchCommand: []string{"codex", "resume"}}
	// First start: prompt as args, no relaunch command.
	first := primaryCreateRequest(agent, []string{"do a thing"}, false)
	if !first.primary || len(first.command) != 0 || len(first.Args) != 1 {
		t.Fatalf("first start = %#v", first)
	}
	// Subsequent start: relaunch command replaces the run command.
	resume := primaryCreateRequest(agent, []string{"do a thing"}, true)
	if len(resume.command) != 2 || resume.command[1] != "resume" || len(resume.Args) != 0 {
		t.Fatalf("resume = %#v", resume)
	}
	// Subsequent start with no relaunch command: start bare, no prompt replay.
	bare := primaryCreateRequest(config.Agent{ID: "codex", Command: []string{"codex"}}, []string{"p"}, true)
	if len(bare.command) != 0 || len(bare.Args) != 0 {
		t.Fatalf("bare = %#v", bare)
	}
}
