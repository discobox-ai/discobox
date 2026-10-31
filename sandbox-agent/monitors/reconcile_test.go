package monitors

import (
	"context"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/discobox-ai/discobox/sandbox-agent/execs/execstest"
	"github.com/discobox-ai/x/shorttmp"
)

func newTestReconciler(t *testing.T) (*Reconciler, *execstest.Units) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the exec runtime resolves guest paths; a Windows host has none")
	}
	root := t.TempDir()
	units := execstest.NewUnits(t)
	manager, err := execs.NewManagerWithConfig(execs.ManagerConfig{
		WorkingRoot: root,
		RuntimeDir:  filepath.Join(shorttmp.Dir(t), "rt"),
		Env:         map[string]string{"PATH": "/usr/bin", "SHELL": "/bin/sh"},
		Units:       units,
	})
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	return &Reconciler{
		Execs:      manager,
		Dirs:       HomeDirs(home),
		Executable: "/opt/discobox/current/discobox-sandbox-agent",
		Home:       home,
	}, units
}

func monitorExecs(r *Reconciler) []execs.Exec {
	var out []execs.Exec
	for _, ex := range r.Execs.List() {
		if ex.Metadata[MetadataMonitorName] != "" {
			out = append(out, ex)
		}
	}
	return out
}

func TestReconcilerRunsOneRunnerPerEnabledInstance(t *testing.T) {
	r, units := newTestReconciler(t)
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	if err := r.ensureEnabledDir(); err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	if _, err := Enable(r.Dirs, Instance{Name: "pr-412", Monitor: "github-repo", Workdir: workdir}); err != nil {
		t.Fatal(err)
	}
	r.Sync(ctx, logger)
	r.Sync(ctx, logger)
	got := monitorExecs(r)
	if len(got) != 1 || len(units.Starts) != 1 {
		t.Fatalf("execs %+v after %d starts; want one, started once", got, len(units.Starts))
	}
	start := units.Starts[0]
	if start.Workdir != workdir || start.TTY {
		t.Fatalf("runner started in %q, tty %v", start.Workdir, start.TTY)
	}
	command := strings.Join(start.Command, " ")
	if !strings.Contains(command, "discobox-sandbox-agent") || !strings.Contains(command, RunnerCommand+" 'pr-412'") {
		t.Fatalf("runner command = %q", command)
	}
	if got[0].Metadata[MetadataMonitorID] != "github-repo" {
		t.Fatalf("metadata = %v", got[0].Metadata)
	}

	// Disabled, its runner is stopped and its exec removed.
	if err := Disable(r.Dirs, "pr-412"); err != nil {
		t.Fatal(err)
	}
	r.Sync(ctx, logger)
	if got := monitorExecs(r); len(got) != 0 || len(units.Stops) != 1 {
		t.Fatalf("after disable: execs %+v, %d stops", got, len(units.Stops))
	}
}

func TestReconcilerRelaunchesARunnerThatEnded(t *testing.T) {
	r, units := newTestReconciler(t)
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	code := int64(1)
	units.Exit = &code
	if _, err := Enable(r.Dirs, Instance{Name: "ci", Monitor: "github-repo"}); err != nil {
		t.Fatal(err)
	}
	r.Sync(ctx, logger)
	first := monitorExecs(r)
	units.Exit = nil
	r.Sync(ctx, logger)
	second := monitorExecs(r)
	if len(first) != 1 || len(second) != 1 || first[0].ID != second[0].ID || len(units.Starts) != 2 {
		t.Fatalf("first %+v, second %+v, %d starts; want one exec relaunched under its id", first, second, len(units.Starts))
	}
	// An instance naming no workdir, or one that is gone, runs in home.
	if units.Starts[0].Workdir != r.Home {
		t.Fatalf("workdir = %q, want home", units.Starts[0].Workdir)
	}
}

func TestReconcilerReplacesARunnerThatIsNotTheEnabledInstances(t *testing.T) {
	r, units := newTestReconciler(t)
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	if _, err := Enable(r.Dirs, Instance{Name: "pr", Monitor: "github-repo", Options: map[string]string{"pr": "412"}}); err != nil {
		t.Fatal(err)
	}
	r.Sync(ctx, logger)
	first := monitorExecs(r)

	// Disabled and enabled with other options before any sync saw the gap.
	if err := Disable(r.Dirs, "pr"); err != nil {
		t.Fatal(err)
	}
	if _, err := Enable(r.Dirs, Instance{Name: "pr", Monitor: "github-repo", Options: map[string]string{"pr": "413"}}); err != nil {
		t.Fatal(err)
	}
	r.Sync(ctx, logger)
	second := monitorExecs(r)
	inst, _ := ReadInstance(r.Dirs, "pr")
	if len(second) != 1 || second[0].ID == first[0].ID || second[0].Metadata[MetadataMonitorInstance] != inst.ID {
		t.Fatalf("first %+v, second %+v; want the old runner replaced by one for %s", first, second, inst.ID)
	}

	// An agent that now starts runners from elsewhere — an exec recorded
	// before an upgrade — replaces the record rather than relaunching a
	// command whose binary may be gone.
	r.Executable = "/opt/discobox/current/features/core/discobox-sandbox-agent.new"
	r.Sync(ctx, logger)
	third := monitorExecs(r)
	if len(third) != 1 || third[0].ID == second[0].ID || !strings.Contains(strings.Join(third[0].Command, " "), r.Executable) {
		t.Fatalf("third %+v; want a runner started from %s", third, r.Executable)
	}
	if len(units.Starts) != 3 {
		t.Fatalf("%d starts, want 3", len(units.Starts))
	}
}
