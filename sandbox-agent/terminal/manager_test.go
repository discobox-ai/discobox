package terminal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/obot-platform/discobox/sandbox-agent/config"
)

func TestManagerCreateListDelete(t *testing.T) {
	runner := &fakeRunner{}
	root := t.TempDir()
	installer := &fakeInstaller{}
	manager, err := NewManager(ManagerConfig{
		Agents: []config.Agent{{
			ID:      "codex",
			Command: []string{"codex"},
		}},
		WorkingRoot: root,
		RuntimeDir:  t.TempDir(),
		Units:       runner,
		Installer:   installer,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	manager.SetHookSocketPath("/run/discobox/hooks.sock")

	created, err := manager.Create(context.Background(), CreateRequest{
		Args:     []string{"--resume"},
		Workdir:  "project",
		Metadata: map[string]string{"purpose": "test"},
	})
	if err != nil {
		t.Fatalf("create terminal: %v", err)
	}
	if created.Status != StatusStarting {
		t.Fatalf("status = %q", created.Status)
	}
	if created.Workdir != filepath.Join(root, "project") {
		t.Fatalf("workdir = %q", created.Workdir)
	}
	if got := runner.starts[0].Command; len(got) != 2 || got[0] != "codex" || got[1] != "--resume" {
		t.Fatalf("command = %#v", got)
	}
	if len(installer.installs) != 1 || installer.installs[0].ID != "codex" {
		t.Fatalf("installs = %#v", installer.installs)
	}
	if runner.starts[0].Env["DISCOBOX_TERMINAL_ID"] != created.ID || runner.starts[0].Env["DISCOBOX_HOOK_SOCKET"] != "/run/discobox/hooks.sock" {
		t.Fatalf("env = %#v", runner.starts[0].Env)
	}
	listed := manager.List()
	if len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("listed = %#v", listed)
	}
	if err := manager.Delete(context.Background(), created.ID); err != nil {
		t.Fatalf("delete terminal: %v", err)
	}
	if len(manager.List()) != 0 {
		t.Fatalf("terminal was not deleted")
	}
	if len(runner.stops) != 1 || runner.stops[0] != created.Unit {
		t.Fatalf("stops = %#v", runner.stops)
	}
}

func TestManagerRejectsWorkdirOutsideRoot(t *testing.T) {
	manager, err := NewManager(ManagerConfig{
		Agents: []config.Agent{{
			ID:      "codex",
			Command: []string{"codex"},
		}},
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       &fakeRunner{},
		Installer:   &fakeInstaller{},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if _, err := manager.Create(context.Background(), CreateRequest{Workdir: "../etc"}); err == nil {
		t.Fatalf("expected workdir error")
	}
}

func TestManagerUsesMarkedDefaultAgent(t *testing.T) {
	runner := &fakeRunner{}
	manager, err := NewManager(ManagerConfig{
		Agents: []config.Agent{
			{
				ID:      "codex",
				Command: []string{"codex"},
			},
			{
				ID:        "claude",
				Command:   []string{"claude"},
				IsDefault: true,
			},
		},
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       runner,
		Installer:   &fakeInstaller{},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if _, err := manager.Create(context.Background(), CreateRequest{}); err != nil {
		t.Fatalf("create terminal: %v", err)
	}
	if got := runner.starts[0].Command; len(got) != 1 || got[0] != "claude" {
		t.Fatalf("command = %#v, want claude", got)
	}
}

func TestManagerMarksFailedWhenRunnerFails(t *testing.T) {
	manager, err := NewManager(ManagerConfig{
		Agents: []config.Agent{{
			ID:      "codex",
			Command: []string{"codex"},
		}},
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       &fakeRunner{startErr: errors.New("boom")},
		Installer:   &fakeInstaller{},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	terminal, err := manager.Create(context.Background(), CreateRequest{})
	if err == nil {
		t.Fatalf("expected start error")
	}
	if terminal.Status != StatusFailed {
		t.Fatalf("status = %q", terminal.Status)
	}
	listed := manager.List()
	if len(listed) != 1 || listed[0].Status != StatusFailed {
		t.Fatalf("listed = %#v", listed)
	}
}

func TestManagerUsesForcedResolvedAgent(t *testing.T) {
	runner := &fakeRunner{}
	forced := config.Agent{ID: "forced", Command: []string{"forced"}}
	manager, err := NewManager(ManagerConfig{
		ResolvedAgentConfig: &forced,
		AgentConfigs: []config.Agent{{
			ID:        "claude-code",
			Command:   []string{"claude"},
			IsDefault: true,
		}},
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       runner,
		Installer:   &fakeInstaller{},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if _, err := manager.Create(context.Background(), CreateRequest{}); err != nil {
		t.Fatalf("create terminal: %v", err)
	}
	if got := runner.starts[0].Command; len(got) != 1 || got[0] != "forced" {
		t.Fatalf("command = %#v, want forced", got)
	}
}

func TestManagerUsesRepoLocalAgentConfig(t *testing.T) {
	runner := &fakeRunner{}
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	workdir := filepath.Join(repo, "sub")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".discobox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".discobox", "agent.json"), []byte(`{
		"agent": "claude-code",
		"installCommand": "npm install local-claude",
		"runCommand": "claude --dangerously-skip-permissions"
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	installer := &fakeInstaller{}
	manager, err := NewManager(ManagerConfig{
		AgentConfigs: []config.Agent{
			{
				ID:             "codex",
				Command:        []string{"codex"},
				InstallCommand: "npm install -g @openai/codex",
				IsDefault:      true,
			},
			{
				ID:             "claude-code",
				Name:           "Claude Code",
				Command:        []string{"claude"},
				InstallCommand: "npm install -g @anthropic-ai/claude-code",
			},
		},
		WorkingRoot: root,
		RuntimeDir:  t.TempDir(),
		Units:       runner,
		Installer:   installer,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if _, err := manager.Create(context.Background(), CreateRequest{Workdir: workdir}); err != nil {
		t.Fatalf("create terminal: %v", err)
	}
	if got := runner.starts[0].Command; len(got) != 3 || got[0] != "/bin/bash" || got[2] != "claude --dangerously-skip-permissions" {
		t.Fatalf("command = %#v, want local claude run command", got)
	}
	if len(installer.installs) != 1 || installer.installs[0].InstallCommand != "npm install local-claude" {
		t.Fatalf("installs = %#v, want local install command", installer.installs)
	}
}

func TestCommandInstallerCachesSuccessfulInstallCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installs.log")
	installer := CommandInstaller{}
	agent := config.Agent{
		ID:             "test-agent",
		Command:        []string{"test-agent"},
		InstallCommand: "printf 'one\\n' >> " + strconv.Quote(path),
	}

	if err := installer.EnsureInstalled(context.Background(), agent, t.TempDir(), nil); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := installer.EnsureInstalled(context.Background(), agent, t.TempDir(), nil); err != nil {
		t.Fatalf("second install: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read install log: %v", err)
	}
	if string(data) != "one\n" {
		t.Fatalf("install log = %q, want one successful run", string(data))
	}

	agent.InstallCommand = "printf 'two\\n' >> " + strconv.Quote(path)
	if err := installer.EnsureInstalled(context.Background(), agent, t.TempDir(), nil); err != nil {
		t.Fatalf("changed install: %v", err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read install log: %v", err)
	}
	if string(data) != "one\ntwo\n" {
		t.Fatalf("install log = %q, want changed command to run", string(data))
	}
}

func TestCommandInstallerDoesNotCacheFailedInstallCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "installs.log")
	installer := CommandInstaller{}
	agent := config.Agent{
		ID:             "test-agent",
		Command:        []string{"test-agent"},
		InstallCommand: "printf 'fail\\n' >> " + strconv.Quote(path) + "; exit 7",
	}

	if err := installer.EnsureInstalled(context.Background(), agent, t.TempDir(), nil); err == nil {
		t.Fatalf("first install should fail")
	}
	if err := installer.EnsureInstalled(context.Background(), agent, t.TempDir(), nil); err == nil {
		t.Fatalf("second install should fail")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read install log: %v", err)
	}
	if string(data) != "fail\nfail\n" {
		t.Fatalf("install log = %q, want failed command retried", string(data))
	}
}

type fakeRunner struct {
	starts   []StartRequest
	stops    []string
	startErr error
	stopErr  error
}

type fakeInstaller struct {
	installs []config.Agent
	err      error
}

func (i *fakeInstaller) EnsureInstalled(_ context.Context, agent config.Agent, _ string, _ map[string]string) error {
	i.installs = append(i.installs, agent)
	return i.err
}

func (r *fakeRunner) Start(_ context.Context, req StartRequest) (StartResult, error) {
	r.starts = append(r.starts, req)
	if r.startErr != nil {
		return StartResult{}, r.startErr
	}
	return StartResult{Unit: req.Unit, PID: 1234, SkipStatusWait: true}, nil
}

func (r *fakeRunner) Stop(_ context.Context, unit string) error {
	r.stops = append(r.stops, unit)
	return r.stopErr
}

func (r *fakeRunner) Status(context.Context, string) (UnitStatus, error) {
	return UnitStatus{}, errors.New("not found")
}

func (r *fakeRunner) List(context.Context) ([]UnitStatus, error) {
	return nil, nil
}
