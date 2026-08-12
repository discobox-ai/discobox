package execs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/harness"
	"github.com/obot-platform/discobox/sandbox-agent/nestedbridge"
	"github.com/obot-platform/discobox/sandbox-agent/runuser"
	"github.com/obot-platform/discobox/sandboxconfig"
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

// pool-agent cannot know a sandbox's own directly-connected networks (Docker
// allocates them), so it leaves sandboxconfig.LocalSubnetsToken in NO_PROXY
// for sandbox-agent to resolve. Every exec must see the real list, not the
// literal placeholder, and resolved fresh -- not once at manager construction
// -- since the nested-Docker bridge and any user-created networks only appear
// after the sandbox has booted.
func TestManagerResolvesLocalSubnetsTokenPerExec(t *testing.T) {
	runner := &fakeUnitManager{}
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Env:         map[string]string{"NO_PROXY": "127.0.0.1,localhost,::1," + sandboxconfig.LocalSubnetsToken},
		Units:       runner,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if _, err := manager.Create(context.Background(), CreateRequest{Command: []string{"echo", "ok"}}); err != nil {
		t.Fatalf("create exec: %v", err)
	}

	noProxy := runner.starts[0].Env["NO_PROXY"]
	if strings.Contains(noProxy, sandboxconfig.LocalSubnetsToken) {
		t.Fatalf("token left unresolved: %q", noProxy)
	}
	if !strings.Contains(noProxy, "127.0.0.1") {
		t.Fatalf("literal exemptions were lost: %q", noProxy)
	}
	for _, cidr := range nestedbridge.LocalSubnets() {
		if !strings.Contains(noProxy, cidr) {
			t.Fatalf("local subnet %s missing from %q", cidr, noProxy)
		}
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
		RuntimeDir: t.TempDir(),
		Env:        testEffectiveEnv(),
		Units:      runner,
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
		RuntimeDir: t.TempDir(),
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

// testEffectiveEnv mirrors what pool-agent's sandboxconfig.Effective call
// already produces before sandbox-agent ever sees it: image env defaults
// merged in with %HOME% expanded against the resolved sandbox user.
func testEffectiveEnv() map[string]string {
	return map[string]string{
		"NPM_CONFIG_PREFIX": "/home/darren/.npm-global",
		"PATH":              "/home/darren/.npm-global/bin:/home/darren/.cargo/bin:/home/darren/.nix-profile/bin:/home/darren/.local/bin:/nix/var/nix/profiles/default/bin:/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
}

func TestManagerExecRequestOverridesDefaultUserAndWorkdir(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
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
			Name:          "override",
			UID:           &requestUID,
			GID:           &requestUID,
			HomeDirectory: "/home/override",
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

// Attaching an exec whose command ended and whose shim is gone must fail fast
// with ErrSessionGone rather than burning the dial timeout and reporting a bare
// socket ENOENT, which says nothing about what happened to the command.
func TestManagerAttachEndedExecWithoutShim(t *testing.T) {
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       &fakeUnitManager{},
		Audit:       newRecordingAudit(),
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{Command: []string{"codex"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	exited := created
	exited.Status = StatusExited
	code := int64(0)
	exited.ExitCode = &code
	if err := writeRuntime(created.RuntimePath, exited); err != nil {
		t.Fatalf("write runtime: %v", err)
	}

	err = manager.Attach(context.Background(), nil, nil, created.ID, true)
	if !errors.Is(err, ErrSessionGone) {
		t.Fatalf("attach ended exec err = %v, want ErrSessionGone", err)
	}
	if _, err := manager.ConnectOneShot(context.Background(), created.ID); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("one-shot attach ended exec err = %v, want ErrSessionGone", err)
	}

	// A live shim socket still means replayable output, so the attach proceeds.
	socket, err := os.Create(exited.SocketPath)
	if err != nil {
		t.Fatalf("create socket: %v", err)
	}
	_ = socket.Close()
	if err := checkAttachable(exited); err != nil {
		t.Fatalf("attach with shim socket present err = %v, want nil", err)
	}
}

func (m *fakeUnitManager) Status(context.Context, string) (UnitStatus, error) {
	return UnitStatus{}, errors.New("not found")
}

// unloadedUnitManager reports what systemd actually reports for a unit it has
// never heard of: no error, an unloaded and inactive unit. A transient exec unit
// lost to a sandbox reboot reads exactly like this.
type unloadedUnitManager struct {
	fakeUnitManager
}

func (m *unloadedUnitManager) Status(_ context.Context, unit string) (UnitStatus, error) {
	return UnitStatus{Unit: unit, Loaded: false, Status: StatusExited}, nil
}

// A never-started exec whose unit vanished (the sandbox rebooted before its
// command launched) must reconcile to lost. systemd reports the missing unit
// without error, so nothing else demotes it, and a primary terminal left at
// starting forever makes EnsurePrimary skip the relaunch while every attach
// dials a socket that will never exist.
func TestManagerReconcilesNeverStartedExecWithVanishedUnit(t *testing.T) {
	audit := newRecordingAudit()
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       &unloadedUnitManager{},
		Audit:       audit,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{
		Command:  []string{"claude", "--continue"},
		TTY:      true,
		Metadata: map[string]string{"primary": "true"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.Status != StatusStarting || created.StartedAt != nil {
		t.Fatalf("created exec = %#v, want starting and unstarted", created)
	}
	// The reboot: the tmpfs runtime file is gone, only the durable record is left.
	if err := os.Remove(created.RuntimePath); err != nil {
		t.Fatalf("remove runtime: %v", err)
	}

	got, ok := manager.Get(created.ID)
	if !ok {
		t.Fatal("durable exec not found")
	}
	if got.Status != StatusLost {
		t.Fatalf("status = %q, want lost", got.Status)
	}
	// The status must stay put once the runtime file has been rewritten, so a
	// later read cannot report the unknown unit's "inactive" as a clean exit.
	again, ok := manager.Get(created.ID)
	if !ok || again.Status != StatusLost {
		t.Fatalf("second read status = %q (found %v), want lost", again.Status, ok)
	}
	// Lost is terminal, so an attach says the session is gone instead of dialing
	// a socket that will never appear.
	if err := manager.Attach(context.Background(), nil, nil, created.ID, true); !errors.Is(err, ErrSessionGone) {
		t.Fatalf("attach err = %v, want ErrSessionGone", err)
	}
}

// A created-but-not-yet-launched exec has no unit for a moment, and must not be
// declared lost while its runtime file says it is still on its way up.
func TestManagerKeepsUnlaunchedExecStartingWhileRuntimePresent(t *testing.T) {
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Units:       &unloadedUnitManager{},
		Audit:       newRecordingAudit(),
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	created, err := manager.Create(context.Background(), CreateRequest{Command: []string{"codex"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, ok := manager.Get(created.ID)
	if !ok || got.Status != StatusStarting {
		t.Fatalf("status = %q (found %v), want starting", got.Status, ok)
	}
}

func (m *fakeUnitManager) List(context.Context) ([]UnitStatus, error) {
	return nil, nil
}

// The sandbox manifest is the sole authority for group membership; a request
// chooses only identity. Naming a user therefore must not strip the sandbox's
// declared groups -- `exec --user dev` used to run with an empty supplementary
// set while the identical default-user exec kept "docker".
func TestManagerKeepsManifestGroupsForAnExplicitlyNamedUser(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	uid := int64(1000)
	gid := int64(1000)
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Env:         testEffectiveEnv(),
		Units:       &fakeUnitManager{},
		DefaultUser: &User{
			Name:             "dev",
			UID:              &uid,
			GID:              &gid,
			AdditionalGroups: []string{"docker"},
		},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	root := int64(0)
	for name, req := range map[string]CreateRequest{
		"same user by name": {Command: []string{"pwd"}, User: &User{Name: "dev"}},
		"another user":      {Command: []string{"pwd"}, User: &User{UID: &root, GID: &root}},
		"no user":           {Command: []string{"pwd"}},
		// A request carrying only groups names no one to run as, so it still
		// falls back to the manifest's identity -- and to its groups, since it
		// asked for none of its own.
		"no groups, no identity": {Command: []string{"pwd"}, User: &User{}},
	} {
		t.Run(name, func(t *testing.T) {
			created, err := manager.Create(context.Background(), req)
			if err != nil {
				t.Fatalf("create exec: %v", err)
			}
			if created.User == nil {
				t.Fatal("exec ran with no user")
			}
			if got := created.User.AdditionalGroups; len(got) != 1 || got[0] != "docker" {
				t.Fatalf("additionalGroups = %v, want [docker] from the manifest", got)
			}
		})
	}
}

// Groups are all-or-nothing, never merged: a request naming any uses exactly
// those, so an exec can run with fewer groups than the sandbox declares. Merging
// would make the manifest a floor no caller could get under.
func TestManagerRequestGroupsReplaceTheManifests(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	uid := int64(1000)
	gid := int64(1000)
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Env:         testEffectiveEnv(),
		Units:       &fakeUnitManager{},
		DefaultUser: &User{
			Name:             "dev",
			UID:              &uid,
			GID:              &gid,
			AdditionalGroups: []string{"docker", "video"},
		},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	for name, req := range map[string]CreateRequest{
		"with an identity": {Command: []string{"pwd"}, User: &User{Name: "dev", AdditionalGroups: []string{"audio", "997"}}},
		// "the usual user, plus these groups": emptyUser ignores groups, so the
		// identity falls back to the manifest's while the groups do not.
		"groups only": {Command: []string{"pwd"}, User: &User{AdditionalGroups: []string{"audio", "997"}}},
	} {
		t.Run(name, func(t *testing.T) {
			created, err := manager.Create(context.Background(), req)
			if err != nil {
				t.Fatalf("create exec: %v", err)
			}
			if created.User == nil {
				t.Fatal("exec ran with no user")
			}
			got := created.User.AdditionalGroups
			if len(got) != 2 || got[0] != "audio" || got[1] != "997" {
				t.Fatalf("additionalGroups = %v, want [audio 997] from the request", got)
			}
		})
	}
}

// A sandbox whose manifest names no user runs as the image's own account, and
// a plain exec expresses that by carrying no user at all: the launch path sets
// no Credential and the child inherits (ADR 0025 §5). A request that names
// groups is the exception. Those groups still have to be honored -- a request
// naming any runs with exactly those, which is how an exec runs with *fewer*
// groups than the sandbox (§2) -- and honoring them means a Credential, which
// cannot carry groups without ids. Dropping them instead fails open, granting
// the ambient set the caller explicitly asked to narrow.
func TestManagerRequestGroupsSurviveAManifestWithNoUser(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Env:         testEffectiveEnv(),
		Units:       &fakeUnitManager{},
		// No DefaultUser: the manifest named nobody.
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	t.Run("a request naming groups keeps them", func(t *testing.T) {
		created, err := manager.Create(context.Background(), CreateRequest{
			Command: []string{"pwd"},
			User:    &User{AdditionalGroups: []string{"docker"}},
		})
		if err != nil {
			t.Fatalf("create exec: %v", err)
		}
		if created.User == nil {
			t.Fatal("exec ran with no user, discarding the groups the request named")
		}
		if got := created.User.AdditionalGroups; len(got) != 1 || got[0] != "docker" {
			t.Fatalf("additionalGroups = %v, want [docker] from the request", got)
		}
		// The ids are the image's own, since that is what the exec would have
		// inherited had it named no groups. They come from the fixture rather
		// than from os.Getuid: asserting a resolver against the same call it
		// makes cannot fail, and cannot tell a uid from a gid on the usual
		// developer account where the two are equal.
		if got := uidOf(created.User); got != 1500 {
			t.Fatalf("uid = %d, want the image's own 1500", got)
		}
		if got := gidOf(created.User); got != 1600 {
			t.Fatalf("gid = %d, want the image's own 1600", got)
		}
	})

	t.Run("a request naming nothing still inherits", func(t *testing.T) {
		created, err := manager.Create(context.Background(), CreateRequest{Command: []string{"pwd"}})
		if err != nil {
			t.Fatalf("create exec: %v", err)
		}
		if created.User != nil {
			t.Fatalf("user = %#v, want none: nothing was asked for, so the exec inherits", created.User)
		}
	})
}

// A request naming only a primary group means "the usual user, but with this
// primary group", the same way a request naming only supplementary groups means
// "the usual user, plus these". emptyUser ignores Group for exactly the reason
// it ignores AdditionalGroups -- a request carrying only a group still names no
// one to run as -- so the group has to be read off the request before the
// identity fallback and applied after it. Otherwise the fallback discards it
// silently, and the exec keeps the manifest user's own primary group instead of
// the one that was asked for: more access than was requested, in the direction
// nobody notices.
func TestManagerRequestPrimaryGroupSurvivesTheIdentityFallback(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	uid := int64(1000)
	gid := int64(2000)

	newManager := func(t *testing.T, defaultUser *User) *Manager {
		t.Helper()
		manager, err := NewManagerWithConfig(ManagerConfig{
			WorkingRoot: "/workspace",
			RuntimeDir:  t.TempDir(),
			Env:         testEffectiveEnv(),
			Units:       &fakeUnitManager{},
			DefaultUser: defaultUser,
		})
		if err != nil {
			t.Fatalf("new manager: %v", err)
		}
		return manager
	}

	// docker is gid 997 in the fixed group database and the manifest user's own
	// primary group is 2000, so the requested group and the inherited one are
	// never confusable -- and neither is confusable with the uid, which is 1000.
	const dockerGID = int64(997)

	t.Run("group only keeps the manifest identity", func(t *testing.T) {
		manager := newManager(t, &User{Name: "dev", UID: &uid, GID: &gid})
		created, err := manager.Create(context.Background(), CreateRequest{
			Command: []string{"pwd"},
			User:    &User{GroupName: "docker"},
		})
		if err != nil {
			t.Fatalf("create exec: %v", err)
		}
		if created.User == nil {
			t.Fatal("exec ran with no user")
		}
		if got := gidOf(created.User); got != dockerGID {
			t.Fatalf("gid = %d, want %d (docker) from the request, not the manifest user's own", got, dockerGID)
		}
		// Only the primary group was chosen; the identity is still the
		// manifest's, since the request named no one to run as.
		if created.User.Name != "dev" || created.User.UID == nil || *created.User.UID != uid {
			t.Fatalf("identity = %+v, want the manifest's dev/%d", created.User, uid)
		}
	})

	// The shape the CLI actually produces: `--group docker,video` with no
	// `--user` sends the first group as a name and the rest as supplementary
	// (applySandboxExecGroups). Honoring the supplementary list while dropping
	// the primary would satisfy half the request and report success.
	t.Run("primary and supplementary groups arrive together", func(t *testing.T) {
		manager := newManager(t, &User{Name: "dev", UID: &uid, GID: &gid})
		created, err := manager.Create(context.Background(), CreateRequest{
			Command: []string{"pwd"},
			User:    &User{GroupName: "docker", AdditionalGroups: []string{"video"}},
		})
		if err != nil {
			t.Fatalf("create exec: %v", err)
		}
		if got := gidOf(created.User); got != dockerGID {
			t.Fatalf("gid = %d, want %d (docker) as the primary group", got, dockerGID)
		}
		if got := created.User.AdditionalGroups; len(got) != 1 || got[0] != "video" {
			t.Fatalf("additionalGroups = %v, want [video]", got)
		}
	})

	t.Run("group only survives a manifest with no user", func(t *testing.T) {
		manager := newManager(t, nil)
		created, err := manager.Create(context.Background(), CreateRequest{
			Command: []string{"pwd"},
			User:    &User{GroupName: "docker"},
		})
		if err != nil {
			t.Fatalf("create exec: %v", err)
		}
		if created.User == nil {
			t.Fatal("exec ran with no user, discarding the group the request named")
		}
		if got := gidOf(created.User); got != dockerGID {
			t.Fatalf("gid = %d, want %d (docker) from the request", got, dockerGID)
		}
		// As with groups, the uid is the image's own: a credential cannot carry
		// a group without one, and that is the id the exec would have inherited
		// anyway.
		if got := uidOf(created.User); got != 1500 {
			t.Fatalf("uid = %d, want the image's own 1500", got)
		}
	})

	t.Run("gid and group together stay mutually exclusive", func(t *testing.T) {
		manager := newManager(t, &User{Name: "dev", UID: &uid, GID: &gid})
		root := int64(0)
		// A request that names its own identity is not a fallback case, so the
		// hoisted group must not quietly overwrite the gid it also named. Two
		// answers to one question is a malformed request, and stays an error.
		if _, err := manager.Create(context.Background(), CreateRequest{
			Command: []string{"pwd"},
			User:    &User{UID: &uid, GID: &root, GroupName: "docker"},
		}); err == nil {
			t.Fatal("a request naming both gid and group was accepted; it must stay mutually exclusive")
		}
	})
}

// TestManagerExpandsTildeWorkdirAgainstUserHome covers the workdir the SSH
// ingress asks for. SSH starts a session in the user's home directory and
// scp/sftp resolve relative paths there, so `~` must land on home rather than
// on the sandbox's default (the primary source directory) — writing uploads
// into the source tree is the bug this expansion exists to prevent.
func TestManagerExpandsTildeWorkdirAgainstUserHome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		workdir string
		want    string
	}{
		{name: "bare tilde", workdir: "~", want: "/home/darren"},
		{name: "tilde subdirectory", workdir: "~/uploads", want: "/home/darren/uploads"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeUnitManager{}
			manager, err := NewManagerWithConfig(ManagerConfig{
				WorkingRoot:    "/workspace",
				DefaultWorkdir: "project",
				DefaultUser:    &User{Name: "darren", HomeDirectory: "/home/darren"},
				RuntimeDir:     t.TempDir(),
				Units:          runner,
			})
			if err != nil {
				t.Fatalf("new manager: %v", err)
			}

			created, err := manager.Create(context.Background(), CreateRequest{
				Command: []string{"pwd"},
				Workdir: tc.workdir,
			})
			if err != nil {
				t.Fatalf("create exec: %v", err)
			}
			if created.Workdir != tc.want {
				t.Fatalf("workdir = %q, want %q", created.Workdir, tc.want)
			}
			if runner.starts[0].Workdir != tc.want {
				t.Fatalf("start workdir = %q, want %q", runner.starts[0].Workdir, tc.want)
			}
		})
	}
}

// TestManagerTildeWorkdirWithoutHomeFails documents the deliberate choice not
// to silently fall back: a caller that asked for home and got the source
// directory instead would only find out when files landed in the wrong place.
func TestManagerTildeWorkdirWithoutHomeFails(t *testing.T) {
	runner := &fakeUnitManager{}
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot:    "/workspace",
		DefaultWorkdir: "project",
		RuntimeDir:     t.TempDir(),
		Units:          runner,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if _, err := manager.Create(context.Background(), CreateRequest{
		Command: []string{"pwd"},
		Workdir: "~",
	}); err == nil {
		t.Fatal("expected ~ with no resolvable home directory to fail")
	}
}

// TestManagerLeavesNonTildeWorkdirsAlone keeps the expansion narrow: only a
// leading `~`/`~/` is special, and a relative path still joins the working
// root as before.
func TestManagerLeavesNonTildeWorkdirsAlone(t *testing.T) {
	runner := &fakeUnitManager{}
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		DefaultUser: &User{Name: "darren", HomeDirectory: "/home/darren"},
		RuntimeDir:  t.TempDir(),
		Units:       runner,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	for _, tc := range []struct{ workdir, want string }{
		{workdir: "sub", want: "/workspace/sub"},
		{workdir: "/etc", want: "/etc"},
		{workdir: "already~tilde", want: "/workspace/already~tilde"},
	} {
		created, err := manager.Create(context.Background(), CreateRequest{
			Command: []string{"pwd"},
			Workdir: tc.workdir,
		})
		if err != nil {
			t.Fatalf("create exec %q: %v", tc.workdir, err)
		}
		if created.Workdir != tc.want {
			t.Fatalf("workdir for %q = %q, want %q", tc.workdir, created.Workdir, tc.want)
		}
	}
}

// TestHomeDirFallsBackToEnvHome covers a run user with no passwd entry (a bare
// UID) whose environment still names a home.
func TestHomeDirFallsBackToEnvHome(t *testing.T) {
	uid := int64(4242)
	got := HomeDir(&User{UID: &uid}, map[string]string{"HOME": "/home/from-env"})
	if got != "/home/from-env" {
		t.Fatalf("HomeDir = %q, want /home/from-env", got)
	}
	if got := HomeDir(nil, nil); got != "" {
		t.Fatalf("HomeDir with nothing to resolve = %q, want empty", got)
	}
}

// gidOf reports a resolved user's gid, or -1 when it has none, so a failing
// assertion prints the id rather than a pointer address.
func gidOf(user *User) int64 {
	if user == nil || user.GID == nil {
		return -1
	}
	return *user.GID
}

// uidOf reports a resolved user's uid, or -1 when it has none, so a failing
// assertion prints the id rather than a pointer address.
func uidOf(user *User) int64 {
	if user == nil || user.UID == nil {
		return -1
	}
	return *user.UID
}

// The pool agent cannot resolve the sandbox user's home when the request did
// not state one -- the account lives in the image -- so it forwards %HOME%
// unexpanded and the sandbox substitutes it, exactly as it does for
// %LOCAL_SUBNETS%. Expanding it outside against a blank would have produced
// real paths pointing at the wrong place (ADR 0032 §5).
func TestManagerExpandsDeferredHomeTokenInEnv(t *testing.T) {
	t.Cleanup(runuser.FixedDatabase())
	runner := &fakeUnitManager{}
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Env:         map[string]string{"CLAUDE_CONFIG_DIR": harness.HomeToken + "/.claude"},
		Units:       runner,
		DefaultUser: &User{Name: "dev"},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if _, err := manager.Create(context.Background(), CreateRequest{Command: []string{"pwd"}}); err != nil {
		t.Fatalf("create exec: %v", err)
	}
	if got := runner.starts[0].Env["CLAUDE_CONFIG_DIR"]; got != "/home/dev/.claude" {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want /home/dev/.claude", got)
	}
}

// With no home resolvable at all the token is left alone. A literal %HOME% is
// visibly wrong; "/.claude" is a real path that silently is not the one anyone
// meant.
func TestManagerLeavesTheHomeTokenWhenNoHomeIsKnown(t *testing.T) {
	t.Cleanup(runuser.FixedEffectiveIDs(4242424, 4242424))
	runner := &fakeUnitManager{}
	manager, err := NewManagerWithConfig(ManagerConfig{
		WorkingRoot: "/workspace",
		RuntimeDir:  t.TempDir(),
		Env:         map[string]string{"CLAUDE_CONFIG_DIR": harness.HomeToken + "/.claude"},
		Units:       runner,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if _, err := manager.Create(context.Background(), CreateRequest{Command: []string{"pwd"}}); err != nil {
		t.Fatalf("create exec: %v", err)
	}
	if got := runner.starts[0].Env["CLAUDE_CONFIG_DIR"]; got != harness.HomeToken+"/.claude" {
		t.Fatalf("CLAUDE_CONFIG_DIR = %q, want the token left in place", got)
	}
}
