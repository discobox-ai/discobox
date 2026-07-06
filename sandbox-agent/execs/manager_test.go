package execs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/sandbox-agent/config"
)

func TestManagerMergesConfigEnvWithRequestOverrides(t *testing.T) {
	runner := &fakeUnitManager{}
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Env:         map[string]string{"BASE": "sandbox", "OVERRIDE": "sandbox"},
		Units:       runner,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if _, err := manager.Create(context.Background(), CreateRequest{
		Command: []string{"echo", "ok"},
		Env:     map[string]string{"OVERRIDE": "exec", "LOCAL": "exec"},
	}); err != nil {
		t.Fatalf("create exec: %v", err)
	}

	env := runner.starts[0].Env
	if env["BASE"] != "sandbox" || env["OVERRIDE"] != "exec" || env["LOCAL"] != "exec" {
		t.Fatalf("env = %#v, want config env with request override", env)
	}
	if env["TERM"] != defaultTerm {
		t.Fatalf("TERM = %q, want %q", env["TERM"], defaultTerm)
	}
}

func TestManagerPreservesExecTermOverrides(t *testing.T) {
	runner := &fakeUnitManager{}
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Env:         map[string]string{"TERM": "screen-256color"},
		Units:       runner,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if _, err := manager.Create(context.Background(), CreateRequest{
		Command: []string{"echo", "ok"},
		Env:     map[string]string{"TERM": "vt100"},
	}); err != nil {
		t.Fatalf("create exec: %v", err)
	}

	if got := runner.starts[0].Env["TERM"]; got != "vt100" {
		t.Fatalf("TERM = %q, want exec override", got)
	}
}

func TestManagerDefaultsExecFromSandboxConfig(t *testing.T) {
	runner := &fakeUnitManager{}
	uid := int64(1000)
	gid := int64(1001)
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot:    "/workspace",
		DefaultWorkdir: "project",
		DefaultUser: &User{
			Name:          "darren",
			UID:           &uid,
			GID:           &gid,
			HomeDirectory: "/home/darren",
		},
		RuntimeDir:  t.TempDir(),
		ImageConfig: testImageConfig(),
		Units:       runner,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	created, err := manager.Create(context.Background(), CreateRequest{
		Command: []string{"pwd"},
	})
	if err != nil {
		t.Fatalf("create exec: %v", err)
	}

	wantWorkdir := filepath.Join("/workspace", "project")
	if created.Workdir != wantWorkdir {
		t.Fatalf("workdir = %q, want %q", created.Workdir, wantWorkdir)
	}
	if created.User == nil || created.User.Name != "darren" || created.User.UID == nil || *created.User.UID != uid || created.User.GID == nil || *created.User.GID != gid {
		t.Fatalf("user = %#v, want sandbox default user", created.User)
	}
	if runner.starts[0].Workdir != wantWorkdir {
		t.Fatalf("start workdir = %q, want %q", runner.starts[0].Workdir, wantWorkdir)
	}
	if runner.starts[0].User == nil || runner.starts[0].User.Name != "darren" {
		t.Fatalf("start user = %#v, want darren", runner.starts[0].User)
	}
	if runner.starts[0].Env["HOME"] != "/home/darren" || runner.starts[0].Env["USER"] != "darren" || runner.starts[0].Env["LOGNAME"] != "darren" {
		t.Fatalf("start env = %#v, want default user env", runner.starts[0].Env)
	}
	if runner.starts[0].Env["NPM_CONFIG_PREFIX"] != "/home/darren/.npm-global" {
		t.Fatalf("NPM_CONFIG_PREFIX = %q, want user prefix", runner.starts[0].Env["NPM_CONFIG_PREFIX"])
	}
	if got := runner.starts[0].Env["PATH"]; !strings.HasPrefix(got, "/home/darren/.npm-global/bin"+string(os.PathListSeparator)) {
		t.Fatalf("PATH = %q, want npm prefix bin first", got)
	}
	if strings.Contains(runner.starts[0].Env["PATH"], "/root/") {
		t.Fatalf("PATH = %q, should not include root home paths", runner.starts[0].Env["PATH"])
	}
}

func TestManagerPreservesExecNPMAndPathOverrides(t *testing.T) {
	runner := &fakeUnitManager{}
	uid := int64(1000)
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		DefaultUser: &User{
			Name:          "darren",
			UID:           &uid,
			HomeDirectory: "/home/darren",
		},
		RuntimeDir:  t.TempDir(),
		ImageConfig: testImageConfig(),
		Env: map[string]string{
			"NPM_CONFIG_PREFIX": "/custom/npm",
			"PATH":              "/custom/bin",
		},
		Units: runner,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if _, err := manager.Create(context.Background(), CreateRequest{
		Command: []string{"echo", "ok"},
	}); err != nil {
		t.Fatalf("create exec: %v", err)
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

func TestManagerExecRequestOverridesDefaultUserAndWorkdir(t *testing.T) {
	runner := &fakeUnitManager{}
	defaultUID := int64(1000)
	requestUID := int64(2000)
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot:    "/workspace",
		DefaultWorkdir: "project",
		DefaultUser: &User{
			Name: "darren",
			UID:  &defaultUID,
		},
		RuntimeDir: t.TempDir(),
		Units:      runner,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	created, err := manager.Create(context.Background(), CreateRequest{
		Command: []string{"pwd"},
		Workdir: "/tmp",
		User: &User{
			Name: "override",
			UID:  &requestUID,
		},
	})
	if err != nil {
		t.Fatalf("create exec: %v", err)
	}

	if created.Workdir != "/tmp" {
		t.Fatalf("workdir = %q, want /tmp", created.Workdir)
	}
	if created.User == nil || created.User.Name != "override" || created.User.UID == nil || *created.User.UID != requestUID {
		t.Fatalf("user = %#v, want override user", created.User)
	}
}

func TestManagerAllowsAbsoluteWorkdirOutsideWorkingRoot(t *testing.T) {
	runner := &fakeUnitManager{}
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       runner,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	created, err := manager.Create(context.Background(), CreateRequest{
		Command: []string{"pwd"},
		Workdir: "/tmp/../etc",
	})
	if err != nil {
		t.Fatalf("create exec: %v", err)
	}

	if created.Workdir != "/etc" {
		t.Fatalf("workdir = %q, want /etc", created.Workdir)
	}
	if runner.starts[0].Workdir != "/etc" {
		t.Fatalf("start workdir = %q, want /etc", runner.starts[0].Workdir)
	}
}

func TestManagerResolvesRelativeWorkdirFromWorkingRoot(t *testing.T) {
	runner := &fakeUnitManager{}
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       runner,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	created, err := manager.Create(context.Background(), CreateRequest{
		Command: []string{"pwd"},
		Workdir: "project",
	})
	if err != nil {
		t.Fatalf("create exec: %v", err)
	}

	want := filepath.Join("/workspace", "project")
	if created.Workdir != want {
		t.Fatalf("workdir = %q, want %q", created.Workdir, want)
	}
}

func TestManagerRelativeWorkdirCanCleanOutsideWorkingRoot(t *testing.T) {
	runner := &fakeUnitManager{}
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       runner,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	created, err := manager.Create(context.Background(), CreateRequest{
		Command: []string{"pwd"},
		Workdir: "../etc",
	})
	if err != nil {
		t.Fatalf("create exec: %v", err)
	}

	if created.Workdir != "/etc" {
		t.Fatalf("workdir = %q, want /etc", created.Workdir)
	}
}

type fakeUnitManager struct {
	starts []StartRequest
}

func (m *fakeUnitManager) Start(_ context.Context, req StartRequest) (StartResult, error) {
	m.starts = append(m.starts, req)
	return StartResult{Unit: req.Unit, PID: 1234}, nil
}

func (m *fakeUnitManager) Status(context.Context, string) (UnitStatus, error) {
	return UnitStatus{}, errors.New("not found")
}

func (m *fakeUnitManager) List(context.Context) ([]UnitStatus, error) {
	return nil, nil
}
