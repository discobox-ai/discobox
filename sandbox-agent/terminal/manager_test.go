package terminal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/sandbox-agent/config"
	"github.com/obot-platform/discobox/sandbox-agent/execs"
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

func TestManagerAllowsWorkdirOutsideRoot(t *testing.T) {
	runner := &fakeRunner{}
	manager, err := NewManager(ManagerConfig{
		Agents: []config.Agent{{
			ID:      "codex",
			Command: []string{"codex"},
		}},
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       runner,
		Installer:   &fakeInstaller{},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	created, err := manager.Create(context.Background(), CreateRequest{Workdir: "/tmp/../etc"})
	if err != nil {
		t.Fatalf("create terminal: %v", err)
	}
	if created.Workdir != "/etc" {
		t.Fatalf("workdir = %q, want /etc", created.Workdir)
	}
	if runner.starts[0].Workdir != "/etc" {
		t.Fatalf("start workdir = %q, want /etc", runner.starts[0].Workdir)
	}
}

func TestManagerMergesConfigEnvWithRequestOverrides(t *testing.T) {
	runner := &fakeRunner{}
	manager, err := NewManager(ManagerConfig{
		Agents: []config.Agent{{
			ID:      "codex",
			Command: []string{"codex"},
		}},
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Env:         map[string]string{"BASE": "sandbox", "OVERRIDE": "sandbox"},
		Units:       runner,
		Installer:   &fakeInstaller{},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if _, err := manager.Create(context.Background(), CreateRequest{
		Env: map[string]string{"OVERRIDE": "terminal", "LOCAL": "terminal"},
	}); err != nil {
		t.Fatalf("create terminal: %v", err)
	}

	env := runner.starts[0].Env
	if env["BASE"] != "sandbox" || env["OVERRIDE"] != "terminal" || env["LOCAL"] != "terminal" {
		t.Fatalf("env = %#v, want config env with request override", env)
	}
	if env["TERM"] != defaultTerm {
		t.Fatalf("TERM = %q, want %q", env["TERM"], defaultTerm)
	}
}

func TestManagerPreservesTerminalTermOverrides(t *testing.T) {
	runner := &fakeRunner{}
	manager, err := NewManager(ManagerConfig{
		Agents: []config.Agent{{
			ID:      "codex",
			Command: []string{"codex"},
		}},
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Env:         map[string]string{"TERM": "screen-256color"},
		Units:       runner,
		Installer:   &fakeInstaller{},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if _, err := manager.Create(context.Background(), CreateRequest{
		Env: map[string]string{"TERM": "vt100"},
	}); err != nil {
		t.Fatalf("create terminal: %v", err)
	}

	if got := runner.starts[0].Env["TERM"]; got != "vt100" {
		t.Fatalf("TERM = %q, want terminal override", got)
	}
}

func TestManagerDefaultsTerminalNPMPrefixFromHome(t *testing.T) {
	runner := &fakeRunner{}
	manager, err := NewManager(ManagerConfig{
		Agents: []config.Agent{{
			ID:      "codex",
			Command: []string{"codex"},
		}},
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Env:         map[string]string{"HOME": "/home/darren"},
		ImageConfig: testImageConfig(),
		Units:       runner,
		Installer:   &fakeInstaller{},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if _, err := manager.Create(context.Background(), CreateRequest{}); err != nil {
		t.Fatalf("create terminal: %v", err)
	}

	if got := runner.starts[0].Env["NPM_CONFIG_PREFIX"]; got != "/home/darren/.npm-global" {
		t.Fatalf("NPM_CONFIG_PREFIX = %q, want user prefix", got)
	}
	if got := runner.starts[0].Env["PATH"]; !strings.HasPrefix(got, "/home/darren/.npm-global/bin"+string(os.PathListSeparator)) {
		t.Fatalf("PATH = %q, want npm prefix bin first", got)
	}
	if strings.Contains(runner.starts[0].Env["PATH"], "/root/") {
		t.Fatalf("PATH = %q, should not include root home paths", runner.starts[0].Env["PATH"])
	}
}

func TestManagerDefaultsTerminalUserFromSandboxConfig(t *testing.T) {
	runner := &fakeRunner{}
	uid := int64(1000)
	gid := int64(1001)
	manager, err := NewManager(ManagerConfig{
		Agents: []config.Agent{{
			ID:      "claude-code",
			Command: []string{"claude"},
		}},
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		ExecDefaults: config.ExecDefaults{
			Username:      "darren",
			UID:           &uid,
			GID:           &gid,
			HomeDirectory: "/home/darren",
		},
		ImageConfig: testImageConfig(),
		Units:       runner,
		Installer:   &fakeInstaller{},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if _, err := manager.Create(context.Background(), CreateRequest{}); err != nil {
		t.Fatalf("create terminal: %v", err)
	}

	start := runner.starts[0]
	if start.User == nil || start.User.Name != "darren" || start.User.UID == nil || *start.User.UID != uid || start.User.GID == nil || *start.User.GID != gid {
		t.Fatalf("start user = %#v, want sandbox default user", start.User)
	}
	if start.Env["HOME"] != "/home/darren" || start.Env["USER"] != "darren" || start.Env["LOGNAME"] != "darren" {
		t.Fatalf("start env = %#v, want default user env", start.Env)
	}
	if got := start.Env["PATH"]; !strings.HasPrefix(got, "/home/darren/.npm-global/bin"+string(os.PathListSeparator)) {
		t.Fatalf("PATH = %q, want user-home path from image config", got)
	}
}

func TestManagerDefaultsTerminalWorkdirFromSandboxConfig(t *testing.T) {
	runner := &fakeRunner{}
	manager, err := NewManager(ManagerConfig{
		Agents: []config.Agent{{
			ID:      "claude-code",
			Command: []string{"claude"},
		}},
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		ExecDefaults: config.ExecDefaults{
			Workdir: "/home/darren/src/disco2",
		},
		Units:     runner,
		Installer: &fakeInstaller{},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	created, err := manager.Create(context.Background(), CreateRequest{})
	if err != nil {
		t.Fatalf("create terminal: %v", err)
	}

	if created.Workdir != "/home/darren/src/disco2" {
		t.Fatalf("workdir = %q, want sandbox source working directory", created.Workdir)
	}
	if runner.starts[0].Workdir != "/home/darren/src/disco2" {
		t.Fatalf("start workdir = %q, want sandbox source working directory", runner.starts[0].Workdir)
	}
}

func TestManagerDefaultsRelativeTerminalWorkdirFromWorkingRoot(t *testing.T) {
	runner := &fakeRunner{}
	manager, err := NewManager(ManagerConfig{
		Agents: []config.Agent{{
			ID:      "claude-code",
			Command: []string{"claude"},
		}},
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		ExecDefaults: config.ExecDefaults{
			Workdir: "project",
		},
		Units:     runner,
		Installer: &fakeInstaller{},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	created, err := manager.Create(context.Background(), CreateRequest{})
	if err != nil {
		t.Fatalf("create terminal: %v", err)
	}

	if created.Workdir != "/workspace/project" {
		t.Fatalf("workdir = %q, want relative sandbox source working directory under working root", created.Workdir)
	}
}

func TestManagerPreservesTerminalNPMAndPathOverrides(t *testing.T) {
	runner := &fakeRunner{}
	manager, err := NewManager(ManagerConfig{
		Agents: []config.Agent{{
			ID:      "codex",
			Command: []string{"codex"},
		}},
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Env: map[string]string{
			"HOME":              "/home/darren",
			"NPM_CONFIG_PREFIX": "/custom/npm",
			"PATH":              "/custom/bin",
		},
		ImageConfig: testImageConfig(),
		Units:       runner,
		Installer:   &fakeInstaller{},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if _, err := manager.Create(context.Background(), CreateRequest{}); err != nil {
		t.Fatalf("create terminal: %v", err)
	}

	if got := runner.starts[0].Env["NPM_CONFIG_PREFIX"]; got != "/custom/npm" {
		t.Fatalf("NPM_CONFIG_PREFIX = %q, want override", got)
	}
	if got := runner.starts[0].Env["PATH"]; got != "/custom/bin" {
		t.Fatalf("PATH = %q, want override", got)
	}
}

func testImageConfig() config.ImageConfig {
	return config.ImageConfig{Env: map[string]string{
		"NPM_CONFIG_PREFIX": "%HOME%/.npm-global",
		"PATH":              "%HOME%/.npm-global/bin:%HOME%/.cargo/bin:%HOME%/.nix-profile/bin:%HOME%/.local/bin:/nix/var/nix/profiles/default/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}}
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
		"installCommand": ["npm", "install", "local-claude"],
		"runCommand": ["claude", "--dangerously-skip-permissions"]
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	installer := &fakeInstaller{}
	manager, err := NewManager(ManagerConfig{
		AgentConfigs: []config.Agent{
			{
				ID:             "codex",
				Command:        []string{"codex"},
				InstallCommand: []string{"npm", "install", "-g", "@openai/codex"},
				IsDefault:      true,
			},
			{
				ID:             "claude-code",
				Name:           "Claude Code",
				Command:        []string{"claude"},
				InstallCommand: []string{"npm", "install", "-g", "@anthropic-ai/claude-code"},
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
	if got := runner.starts[0].Command; len(got) != 2 || got[0] != "claude" || got[1] != "--dangerously-skip-permissions" {
		t.Fatalf("command = %#v, want local claude run command", got)
	}
	if got := installer.installs[0].InstallCommand; len(installer.installs) != 1 || len(got) != 3 || got[0] != "npm" || got[1] != "install" || got[2] != "local-claude" {
		t.Fatalf("installs = %#v, want local install command", installer.installs)
	}
}

func TestCommandInstallerCachesSuccessfulInstallCommand(t *testing.T) {
	runner := &fakeRunner{}
	installer := CommandInstaller{
		Units:      runner,
		RuntimeDir: t.TempDir(),
		LogDir:     t.TempDir(),
		startFromSocket: func(context.Context, string) (Terminal, error) {
			return Terminal{Status: StatusRunning}, nil
		},
		statusFromSocket: func(context.Context, string) (Terminal, error) {
			code := int64(0)
			return Terminal{Status: StatusExited, ExitCode: &code}, nil
		},
	}
	agent := config.Agent{
		ID:             "test-agent",
		Command:        []string{"test-agent"},
		InstallCommand: []string{"echo", "one"},
	}

	if err := installer.EnsureInstalled(context.Background(), agent, t.TempDir(), nil); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := installer.EnsureInstalled(context.Background(), agent, t.TempDir(), nil); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if len(runner.starts) != 1 {
		t.Fatalf("starts = %d, want one successful run", len(runner.starts))
	}
	if got := runner.starts[0].Command; len(got) != 2 || got[0] != "echo" || got[1] != "one" {
		t.Fatalf("command = %#v, want install command argv", got)
	}

	agent.InstallCommand = []string{"echo", "two"}
	if err := installer.EnsureInstalled(context.Background(), agent, t.TempDir(), nil); err != nil {
		t.Fatalf("changed install: %v", err)
	}
	if len(runner.starts) != 2 {
		t.Fatalf("starts = %d, want changed command to run", len(runner.starts))
	}
}

func TestCommandInstallerDoesNotCacheFailedInstallCommand(t *testing.T) {
	runner := &fakeRunner{}
	installer := CommandInstaller{
		Units:      runner,
		RuntimeDir: t.TempDir(),
		LogDir:     t.TempDir(),
		startFromSocket: func(context.Context, string) (Terminal, error) {
			return Terminal{Status: StatusRunning}, nil
		},
		statusFromSocket: func(context.Context, string) (Terminal, error) {
			code := int64(7)
			return Terminal{Status: StatusFailed, ExitCode: &code, Error: "exit status 7"}, nil
		},
	}
	agent := config.Agent{
		ID:             "test-agent",
		Command:        []string{"test-agent"},
		InstallCommand: []string{"sh", "-c", "printf 'fail\\n'; exit 7"},
	}

	if err := installer.EnsureInstalled(context.Background(), agent, t.TempDir(), nil); err == nil {
		t.Fatalf("first install should fail")
	}
	if err := installer.EnsureInstalled(context.Background(), agent, t.TempDir(), nil); err == nil {
		t.Fatalf("second install should fail")
	}
	if len(runner.starts) != 2 {
		t.Fatalf("starts = %d, want failed command retried", len(runner.starts))
	}
}

func TestCommandInstallerPassesEnvironmentToTerminalRunner(t *testing.T) {
	runner := &fakeRunner{}
	uid := int64(1000)
	installer := CommandInstaller{
		Units:      runner,
		RuntimeDir: t.TempDir(),
		LogDir:     t.TempDir(),
		User: &execs.User{
			Name: "darren",
			UID:  &uid,
		},
		startFromSocket: func(context.Context, string) (Terminal, error) {
			return Terminal{Status: StatusRunning}, nil
		},
		statusFromSocket: func(context.Context, string) (Terminal, error) {
			code := int64(0)
			return Terminal{Status: StatusExited, ExitCode: &code}, nil
		},
	}
	agent := config.Agent{
		ID:             "test-agent",
		Command:        []string{"test-agent"},
		InstallCommand: []string{"npm", "install", "-g", "test-agent"},
	}

	if err := installer.EnsureInstalled(context.Background(), agent, "/workspace/project", map[string]string{
		"HOME":              "/home/darren",
		"NPM_CONFIG_PREFIX": "/home/darren/.npm-global",
		"PATH":              "/home/darren/.npm-global/bin:/usr/bin",
	}); err != nil {
		t.Fatalf("install: %v", err)
	}

	if len(runner.starts) != 1 {
		t.Fatalf("starts = %d, want one", len(runner.starts))
	}
	start := runner.starts[0]
	if start.Workdir != "/workspace/project" {
		t.Fatalf("workdir = %q, want request workdir", start.Workdir)
	}
	if start.User == nil || start.User.Name != "darren" || start.User.UID == nil || *start.User.UID != uid {
		t.Fatalf("user = %#v, want installer user", start.User)
	}
	if start.Env["NPM_CONFIG_PREFIX"] != "/home/darren/.npm-global" || start.Env["PATH"] != "/home/darren/.npm-global/bin:/usr/bin" {
		t.Fatalf("env = %#v, want terminal env passed to installer", start.Env)
	}
}

func TestFileInstallerWritesFilesUnderHomeDirectory(t *testing.T) {
	home := t.TempDir()
	installer := FileInstaller{HomeDirectory: home}
	agent := config.Agent{
		ID: "claude-code",
		Files: []config.AgentFile{
			{Path: ".claude/settings.json", Content: `{"theme":"dark"}`},
		},
	}

	if err := installer.EnsureInstalled(context.Background(), agent, "/workspace/project", nil); err != nil {
		t.Fatalf("install: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read installed file: %v", err)
	}
	if string(data) != `{"theme":"dark"}` {
		t.Fatalf("content = %q, want agent file content", string(data))
	}
	info, err := os.Stat(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("stat installed file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("mode = %o, want world-readable 0644", got)
	}
}

func TestFileInstallerNoopsWithoutFiles(t *testing.T) {
	installer := FileInstaller{}
	agent := config.Agent{ID: "claude-code"}
	if err := installer.EnsureInstalled(context.Background(), agent, "/workspace/project", nil); err != nil {
		t.Fatalf("install: %v", err)
	}
}

func TestFileInstallerRequiresHomeDirectoryWhenFilesConfigured(t *testing.T) {
	installer := FileInstaller{}
	agent := config.Agent{
		ID:    "claude-code",
		Files: []config.AgentFile{{Path: ".claude/settings.json", Content: "{}"}},
	}
	if err := installer.EnsureInstalled(context.Background(), agent, "/workspace/project", nil); err == nil {
		t.Fatalf("expected error when no home directory is configured")
	}
}

func TestFileInstallerRejectsPathEscapingHomeDirectory(t *testing.T) {
	home := t.TempDir()
	installer := FileInstaller{HomeDirectory: home}
	agent := config.Agent{
		ID:    "claude-code",
		Files: []config.AgentFile{{Path: "../outside.txt", Content: "nope"}},
	}
	if err := installer.EnsureInstalled(context.Background(), agent, "/workspace/project", nil); err == nil {
		t.Fatalf("expected error for path escaping home directory")
	}
}

func TestFileInstallerRejectsAbsolutePath(t *testing.T) {
	home := t.TempDir()
	installer := FileInstaller{HomeDirectory: home}
	agent := config.Agent{
		ID:    "claude-code",
		Files: []config.AgentFile{{Path: "/etc/passwd", Content: "nope"}},
	}
	if err := installer.EnsureInstalled(context.Background(), agent, "/workspace/project", nil); err == nil {
		t.Fatalf("expected error for absolute path")
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
