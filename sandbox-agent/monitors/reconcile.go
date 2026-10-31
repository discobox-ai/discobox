package monitors

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
	"github.com/fsnotify/fsnotify"
)

// Metadata keys tagging the exec that runs an instance's runner.
const (
	MetadataMonitorID   = "monitorId"
	MetadataMonitorName = "monitorName"
	// MetadataMonitorInstance is the Instance.ID the exec was started for.
	MetadataMonitorInstance = "monitorInstanceId"
)

// RunnerCommand is the agent's subcommand an instance's exec runs, with the
// instance's name after it.
const RunnerCommand = "monitor-run"

// resync is how often the reconciler converges whether or not the directory
// told it something changed.
const resync = 30 * time.Second

// Owner is who the run user is, for the directories the agent makes in its
// home. Nil fields leave ownership alone.
type Owner struct {
	UID, GID *int64
}

// Reconciler keeps one exec running each enabled instance's runner, and none
// for an instance that is not enabled (ADR 26-10-31-073 §4). It is the
// agent's whole part in monitors: it reads the enabled files and acts on
// execs, and never reads a monitor's output or the queue.
type Reconciler struct {
	Execs *execs.Manager
	Dirs  Dirs
	Owner Owner
	// Executable is the agent binary the runner is started from. It is
	// recorded in the exec and run again on every relaunch, so it must be a
	// path that outlives an upgrade (agentlayer.AgentPath), never the
	// layer os.Executable resolves to.
	Executable string
	// Home is where a runner whose instance names no workdir starts.
	Home   string
	Logger *slog.Logger
}

// Run converges until ctx ends: at once, then whenever the enabled directory
// changes, and every resync regardless.
func (r *Reconciler) Run(ctx context.Context) {
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if err := r.ensureEnabledDir(); err != nil {
		logger.Warn("monitors: make the enabled directory", "dir", r.Dirs.Enabled, "error", err)
	}
	var changes <-chan fsnotify.Event
	if watcher, err := fsnotify.NewWatcher(); err == nil {
		defer watcher.Close()
		if err := watcher.Add(r.Dirs.Enabled); err == nil {
			changes = watcher.Events
		}
	}
	ticker := time.NewTicker(resync)
	defer ticker.Stop()
	for {
		r.Sync(ctx, logger)
		select {
		case <-ctx.Done():
			return
		case <-changes:
			// A burst of writes is one change: an enable is a temp file and a
			// rename.
			if sleep(ctx, 200*time.Millisecond) != nil {
				return
			}
		case <-ticker.C:
		}
	}
}

// Sync is one convergence: start or relaunch the runner of each enabled
// instance that is not running, and stop and remove the exec of each one
// that is no longer enabled.
func (r *Reconciler) Sync(ctx context.Context, logger *slog.Logger) {
	instances, problems := Enabled(r.Dirs)
	for _, problem := range problems {
		logger.Warn("monitors: unreadable instance", "error", problem)
	}
	enabled := map[string]Instance{}
	for _, inst := range instances {
		enabled[inst.Name] = inst
	}
	// An instance has one exec — a restart relaunches it under its id — but
	// should a stale record double up, the newest is the instance's and the
	// rest are removed.
	byName := map[string]execs.Exec{}
	for _, ex := range r.Execs.List() {
		name := strings.TrimSpace(ex.Metadata[MetadataMonitorName])
		if name == "" {
			continue
		}
		current, ok := byName[name]
		switch {
		case !ok:
			byName[name] = ex
		case current.CreatedAt.After(ex.CreatedAt):
			r.remove(ctx, logger, ex)
		default:
			r.remove(ctx, logger, current)
			byName[name] = ex
		}
	}
	for name, ex := range byName {
		if _, ok := enabled[name]; !ok {
			r.remove(ctx, logger, ex)
		}
	}
	for name, inst := range enabled {
		ex, ok := byName[name]
		if ok && !r.runs(ex, inst) {
			// Another instance under the name — disabled and enabled again
			// before a sync saw the gap — or a runner recorded with a
			// command this agent would no longer start: replaced, not
			// relaunched.
			r.remove(ctx, logger, ex)
			ok = false
		}
		if ok && live(ex) {
			continue
		}
		if err := r.start(ctx, inst, ex, ok); err != nil && ctx.Err() == nil {
			logger.Error("monitors: start runner", "instance", name, "error", err)
		}
	}
}

// runs reports whether ex is the runner this reconciler would start for inst.
func (r *Reconciler) runs(ex execs.Exec, inst Instance) bool {
	return ex.Metadata[MetadataMonitorInstance] == inst.ID &&
		strings.Contains(strings.Join(ex.Command, " "), r.runnerCommand(inst.Name))
}

func (r *Reconciler) runnerCommand(name string) string {
	return execs.QuoteShellArg(r.Executable) + " " + RunnerCommand + " " + execs.QuoteShellArg(name)
}

func (r *Reconciler) start(ctx context.Context, inst Instance, existing execs.Exec, exists bool) error {
	if exists {
		// The same exec id across restarts, as a service keeps (ADR 0038).
		relaunched, err := r.Execs.Relaunch(ctx, execs.RelaunchRequest{ID: existing.ID})
		if err != nil {
			return err
		}
		_, err = r.Execs.Start(ctx, relaunched.ID)
		return err
	}
	workdir := inst.Workdir
	if workdir == "" || !isDir(workdir) {
		workdir = r.Home
	}
	created, err := r.Execs.Create(ctx, execs.CreateRequest{
		// Through the run user's login shell, as a service is, so the
		// monitor has the PATH and profile a person's command would.
		Shell:            true,
		ShellCommandLine: r.runnerCommand(inst.Name),
		Workdir:          workdir,
		// Pipes, not a PTY: nobody attaches to a runner, and with no screen
		// it never reads as activity to the idle stop (ADR 26-10-31-073 §8).
		TTY: false,
		Metadata: map[string]string{
			MetadataMonitorID:       inst.Monitor,
			MetadataMonitorName:     inst.Name,
			MetadataMonitorInstance: inst.ID,
		},
	})
	if err != nil {
		return err
	}
	_, err = r.Execs.Start(ctx, created.ID)
	return err
}

// remove deletes an exec, which stops its unit first: an instance that is
// gone leaves nothing running and nothing in the exec listing.
func (r *Reconciler) remove(ctx context.Context, logger *slog.Logger, ex execs.Exec) {
	if err := r.Execs.Delete(ctx, ex.ID); err != nil {
		logger.Warn("monitors: remove runner", "exec", ex.ID, "error", err)
	}
}

// ensureEnabledDir makes the enabled directory, owned by the run user all the
// way down from the first directory it had to make, so the CLI — running as
// that user — can write in it.
func (r *Reconciler) ensureEnabledDir() error {
	var made []string
	for dir := r.Dirs.Enabled; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(dir); err == nil {
			break
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		made = append(made, dir)
		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}
	if err := os.MkdirAll(r.Dirs.Enabled, 0o755); err != nil {
		return err
	}
	if r.Owner.UID == nil || r.Owner.GID == nil {
		return nil
	}
	for _, dir := range made {
		if err := os.Lchown(dir, int(*r.Owner.UID), int(*r.Owner.GID)); err != nil {
			return err
		}
	}
	return nil
}

func live(ex execs.Exec) bool {
	switch ex.Status {
	case execs.StatusStarting, execs.StatusRunning:
		return true
	}
	return false
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
