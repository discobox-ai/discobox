package terminal

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/internal/shorttmp"
	"github.com/discobox-ai/discobox/sandbox-agent/config"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// shimUnits is a fakeUnits whose Start also binds a minimal exec-shim on the
// requested socket, the way a real unit launch does: /start and /status answer
// with a running exec, so Manager.Start and status probes succeed without
// systemd or a real shim process.
type shimUnits struct {
	fakeUnits
	servers []*http.Server
}

func (f *shimUnits) Start(ctx context.Context, req execs.StartRequest) (execs.StartResult, error) {
	result, err := f.fakeUnits.Start(ctx, req)
	if err != nil {
		return result, err
	}
	ln, err := new(net.ListenConfig).Listen(ctx, "unix", req.SocketPath)
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(map[string]string{"id": req.ID, "status": "running"})
	if err != nil {
		return result, err
	}
	mux := http.NewServeMux()
	answer := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}
	mux.HandleFunc("/start", answer)
	mux.HandleFunc("/status", answer)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	f.servers = append(f.servers, server)
	go func() { _ = server.Serve(ln) }()
	return result, nil
}

// Status reports a loaded, active unit — what systemd says while a shim runs —
// so a live terminal reads live instead of lost. Ended runs are pinned by
// their runtime file and never consult this.
func (f *shimUnits) Status(_ context.Context, unit string) (execs.UnitStatus, error) {
	return execs.UnitStatus{Unit: unit, Loaded: true, Active: true, Status: execs.StatusRunning}, nil
}

func (f *shimUnits) Close() {
	for _, server := range f.servers {
		_ = server.Close()
	}
}

type reviveFixture struct {
	svc       *Service
	execs     *execs.Manager
	units     *shimUnits
	installer *noopInstaller
}

func newReviveService(t *testing.T, harness config.Harness) reviveFixture {
	t.Helper()
	dir := shorttmp.Dir(t)
	units := &shimUnits{}
	t.Cleanup(units.Close)
	installer := &noopInstaller{}
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
		Harness:     harness,
		Units:       units,
		Installer:   installer,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return reviveFixture{svc: svc, execs: execManager, units: units, installer: installer}
}

// markExited rewrites a terminal's runtime file as an ended run, the state a
// terminal is in after its harness exits (or its unit is lost to a reboot).
func markExited(t *testing.T, svc *Service, id string) {
	t.Helper()
	exec, ok := svc.Get(id)
	if !ok {
		t.Fatalf("terminal %s not found", id)
	}
	data, err := os.ReadFile(exec.RuntimePath)
	if err != nil {
		t.Fatalf("read runtime: %v", err)
	}
	var current execs.Exec
	if err := json.Unmarshal(data, &current); err != nil {
		t.Fatalf("unmarshal runtime: %v", err)
	}
	now := time.Now().UTC()
	started := now.Add(-time.Minute)
	current.Status = execs.StatusExited
	current.StartedAt = &started
	current.ExitedAt = &now
	out, err := json.Marshal(current)
	if err != nil {
		t.Fatalf("marshal runtime: %v", err)
	}
	if err := os.WriteFile(exec.RuntimePath, out, 0o600); err != nil {
		t.Fatalf("write runtime: %v", err)
	}
	// The shim from the previous run is gone with the run.
	_ = os.Remove(exec.SocketPath)
}

// A terminal's exec id is its durable identity (ADR 0038): reviving an ended
// terminal resumes it under the same id — the harness relaunch command in a
// fresh shell under a new unit generation — instead of minting a sibling.
func TestReviveResumesDeadTerminalInPlace(t *testing.T) {
	fx := newReviveService(t, config.Harness{
		ID:              "codex",
		Command:         []string{"codex"},
		RelaunchCommand: []string{"codex", "resume", "--last"},
	})
	svc, units, installer := fx.svc, fx.units, fx.installer
	created, err := svc.Create(context.Background(), CreateRequest{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	markExited(t, svc, created.ID)

	revived, err := svc.Revive(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("revive: %v", err)
	}
	if revived.ID != created.ID {
		t.Fatalf("revived id = %q, want the same identity %q", revived.ID, created.ID)
	}
	if revived.Status != execs.StatusRunning {
		t.Fatalf("revived status = %q, want running", revived.Status)
	}
	if len(units.starts) != 2 {
		t.Fatalf("unit starts = %d, want the original plus the revival", len(units.starts))
	}
	start := units.starts[1]
	if !strings.HasSuffix(start.Unit, "-g2") {
		t.Fatalf("unit = %q, want a new generation of the same exec", start.Unit)
	}
	if len(start.StartupCommand) != 3 || start.StartupCommand[1] != "resume" {
		t.Fatalf("startupCommand = %v, want the harness relaunch command", start.StartupCommand)
	}
	if start.Env["DISCOBOX_TERMINAL_ID"] != created.ID {
		t.Fatalf("env terminal id = %q, want the stable id", start.Env["DISCOBOX_TERMINAL_ID"])
	}
	// Hook/file setup is re-ensured per run.
	if len(installer.calls) != 2 {
		t.Fatalf("installer calls = %d, want one per run", len(installer.calls))
	}
	if list := svc.List(); len(list) != 1 {
		t.Fatalf("list = %d execs, want exactly one identity", len(list))
	}
	// Reviving a live terminal is a no-op.
	again, err := svc.Revive(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("revive live: %v", err)
	}
	if again.ID != created.ID || len(units.starts) != 2 {
		t.Fatalf("live revive disturbed the terminal: starts = %d", len(units.starts))
	}
}

// A plain (non-terminal) exec is fire-and-forget and must never be revived.
func TestReviveRefusesNonTerminalExec(t *testing.T) {
	fx := newReviveService(t, config.Harness{ID: "codex", Command: []string{"codex"}})
	created, err := fx.execs.Create(context.Background(), execs.CreateRequest{Command: []string{"echo", "ok"}})
	if err != nil {
		t.Fatalf("create plain exec: %v", err)
	}
	if _, err := fx.svc.Revive(context.Background(), created.ID); err == nil {
		t.Fatal("revive of a non-terminal exec must fail")
	}
}

// EnsurePrimary revives the existing primary record on later boots (ADR 0038)
// — same exec id running the relaunch command — instead of launching a sibling
// primary and leaving the dead one behind in the session list.
func TestEnsurePrimaryRevivesDeadPrimaryRecord(t *testing.T) {
	fx := newReviveService(t, config.Harness{
		ID:              "codex",
		Command:         []string{"codex"},
		RelaunchCommand: []string{"codex", "resume", "--last"},
	})
	svc, units := fx.svc, fx.units
	if err := svc.EnsurePrimary(context.Background(), []string{"build the thing"}); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	first, ok := svc.CurrentPrimary()
	if !ok {
		t.Fatal("no primary after first ensure")
	}
	if got := units.starts[0].StartupCommand; len(got) != 2 || got[1] != "build the thing" {
		t.Fatalf("first startupCommand = %v, want the initial prompt", got)
	}
	// A live primary makes EnsurePrimary a no-op.
	if err := svc.EnsurePrimary(context.Background(), nil); err != nil {
		t.Fatalf("ensure with live primary: %v", err)
	}
	if len(units.starts) != 1 {
		t.Fatalf("unit starts = %d, want the live primary left alone", len(units.starts))
	}

	// The restart: the primary's run ended; the next boot revives the record.
	markExited(t, svc, first.ID)
	if err := svc.EnsurePrimary(context.Background(), []string{"build the thing"}); err != nil {
		t.Fatalf("ensure after death: %v", err)
	}
	second, ok := svc.CurrentPrimary()
	if !ok {
		t.Fatal("no primary after revive")
	}
	if second.ID != first.ID {
		t.Fatalf("primary id changed %q → %q, want a stable identity", first.ID, second.ID)
	}
	if len(units.starts) != 2 {
		t.Fatalf("unit starts = %d, want create plus revive", len(units.starts))
	}
	if got := units.starts[1].StartupCommand; len(got) != 3 || got[1] != "resume" {
		t.Fatalf("revive startupCommand = %v, want the relaunch command, never the prompt", got)
	}
	if list := svc.List(); len(list) != 1 {
		t.Fatalf("list = %d execs, want exactly one primary identity", len(list))
	}
}
