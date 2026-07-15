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

func (m *fakeUnitManager) Stop(context.Context, string) error {
	return nil
}

// recordingAudit is an in-memory AuditRecorder that persists exec records like
// the SQLite store, for testing metadata durability/hydration.
type recordingAudit struct {
	records map[string]Exec
}

func newRecordingAudit() *recordingAudit { return &recordingAudit{records: map[string]Exec{}} }

func (a *recordingAudit) RecordExecEvent(context.Context, string, string, string, map[string]any) error {
	return nil
}
func (a *recordingAudit) ObserveExec(context.Context, Exec) error { return nil }
func (a *recordingAudit) SaveExecRecord(_ context.Context, exec Exec) error {
	if _, ok := a.records[exec.ID]; ok {
		return nil // immutable
	}
	a.records[exec.ID] = cloneExec(exec)
	return nil
}
func (a *recordingAudit) LoadExecRecords(context.Context) ([]Exec, error) {
	out := make([]Exec, 0, len(a.records))
	for _, e := range a.records {
		out = append(out, cloneExec(e))
	}
	return out, nil
}

// A shim runtime write drops the metadata field; the durable record must still
// restore harnessId/primary on read so the exec keeps its identity.
func TestManagerHydratesMetadataAfterRuntimeClobber(t *testing.T) {
	audit := newRecordingAudit()
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       &fakeUnitManager{},
		Audit:       audit,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{
		Command:  []string{"codex"},
		Metadata: map[string]string{"harnessId": "codex", "primary": "true"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Simulate the shim overwriting the runtime file without metadata.
	clobbered := created
	clobbered.Metadata = nil
	if err := writeRuntime(created.RuntimePath, clobbered); err != nil {
		t.Fatalf("clobber runtime: %v", err)
	}

	got, ok := manager.Get(created.ID)
	if !ok {
		t.Fatalf("exec not found after clobber")
	}
	if got.Metadata["harnessId"] != "codex" || got.Metadata["primary"] != "true" {
		t.Fatalf("metadata not restored from record: %#v", got.Metadata)
	}
	// And it is still enumerated with its metadata via List.
	found := false
	for _, e := range manager.List() {
		if e.ID == created.ID && e.Metadata["harnessId"] == "codex" {
			found = true
		}
	}
	if !found {
		t.Fatalf("clobbered exec missing metadata in List")
	}
}

// A durable record does not contain manager-local runtime paths. If the tmpfs
// runtime file disappears, reads must restore those paths and reconcile stale
// active state instead of returning an attachable-looking exec with no socket.
func TestManagerReconcilesDurableRecordWithoutRuntimeFile(t *testing.T) {
	audit := newRecordingAudit()
	runtimeDir := t.TempDir()
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  runtimeDir,
		Units:       &fakeUnitManager{},
		Audit:       audit,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{
		Command:  []string{"claude"},
		Metadata: map[string]string{"harnessId": "claude-code", "primary": "true"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := os.Remove(created.RuntimePath); err != nil {
		t.Fatalf("remove runtime: %v", err)
	}
	// Match the SQLite durable record, which deliberately excludes local paths.
	record := audit.records[created.ID]
	record.RuntimePath = ""
	record.SocketPath = ""
	audit.records[created.ID] = record

	listed := manager.List()
	if len(listed) != 1 || listed[0].SocketPath == "" || listed[0].Status != StatusLost {
		t.Fatalf("listed execs = %#v", listed)
	}
	// List persists the reconciled status back to the derived runtime path;
	// remove it again to exercise Get's durable-record fallback independently.
	if err := os.Remove(created.RuntimePath); err != nil {
		t.Fatalf("remove reconciled runtime: %v", err)
	}
	got, ok := manager.Get(created.ID)
	if !ok {
		t.Fatal("durable exec not found")
	}
	if got.RuntimePath != filepath.Join(runtimeDir, safeName(created.ID)+".json") {
		t.Fatalf("runtime path = %q", got.RuntimePath)
	}
	if got.SocketPath != filepath.Join(runtimeDir, safeName(created.ID)+".sock") {
		t.Fatalf("socket path = %q", got.SocketPath)
	}
	if got.Status != StatusLost {
		t.Fatalf("status = %q, want lost", got.Status)
	}
}

func (m *fakeUnitManager) Status(context.Context, string) (UnitStatus, error) {
	return UnitStatus{}, errors.New("not found")
}

func (m *fakeUnitManager) List(context.Context) ([]UnitStatus, error) {
	return nil, nil
}
