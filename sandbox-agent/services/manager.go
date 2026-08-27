package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

// Metadata keys tagging the exec that runs a service. The id is the join —
// everything addressing a service resolves through it — and the name rides
// along so a client reading only the exec listing can title a session without
// a second request (ADR 0070 §7).
const (
	MetadataServiceID   = "serviceId"
	MetadataServiceName = "serviceName"
)

// Status is a service's state, derived from the exec running it. It is not
// execs.Status: a service that has never run and one that was stopped are both
// simply stopped here, and "lost" is a fact about a systemd unit that a
// service's reader has no use for.
type Status string

const (
	// StatusStopped is a service that is not running and was not left that way
	// by failing: never started, or stopped on request.
	StatusStopped Status = "stopped"
	// StatusStarting is an exec created but whose process has not launched yet.
	StatusStarting Status = "starting"
	StatusRunning  Status = "running"
	// StatusExited is a service whose process ended by itself, successfully.
	// Nothing restarts it (ADR 0070 §4).
	StatusExited Status = "exited"
	// StatusFailed is a service whose process ended with a non-zero status, or
	// that could not be started at all.
	StatusFailed Status = "failed"
)

// Service is a declaration together with the run of it, which is what every
// caller of this package actually wants.
type Service struct {
	Definition

	Status Status
	// ExecID is the exec running this service, empty when none ever has. It is
	// durable across restarts (ADR 0038), so a client keyed on it — the
	// workspace's tabs — keeps its place when a service is restarted.
	ExecID    string
	PID       int64
	ExitCode  *int64
	StartedAt *time.Time
	ExitedAt  *time.Time
	// Error is why the last run failed, empty when it did not.
	Error string
}

// ManagerConfig wires the service layer to the exec primitive it runs on.
type ManagerConfig struct {
	Execs *execs.Manager
	// Root is the repository root services are declared in and run in: the
	// sandbox's primary source directory, which DirName is resolved under and
	// which is also where an exec that names no workdir starts.
	Root string
}

// Manager runs the sandbox's declared services.
//
// It holds no service state of its own. Declarations are re-read from disk on
// every listing, so a file added or edited while the sandbox is up is seen
// immediately (ADR 0070 §5), and run state is the exec record — which means
// two clients, or a client and the boot flow, cannot hold different ideas of
// what is running.
type Manager struct {
	execs *execs.Manager
	root  string

	// lifecycle serializes start/stop/restart per service id. Deciding whether
	// to create an exec or relaunch the existing one is a check-then-act over
	// records nothing else serializes (execs.Manager keeps no in-process lock),
	// and boot's autostart overlaps a client's first `services start` by
	// construction. Two unserialized starts give one service two execs, only
	// one of which any later stop can find.
	mu        sync.Mutex
	lifecycle map[string]*sync.Mutex
}

var ErrNotFound = errors.New("service not found")

func NewManager(cfg ManagerConfig) (*Manager, error) {
	if cfg.Execs == nil {
		return nil, errors.New("shared exec manager is required")
	}
	if strings.TrimSpace(cfg.Root) == "" {
		return nil, errors.New("service root is required")
	}
	return &Manager{execs: cfg.Execs, root: cfg.Root, lifecycle: map[string]*sync.Mutex{}}, nil
}

// List is every declared service with the state of its run, in declaration
// order. It takes no context because it cancels nothing: discovery is a
// directory read and run state is already-collected exec records.
func (m *Manager) List() ([]Service, error) {
	defs, err := Discover(m.root)
	if err != nil {
		return nil, err
	}
	byService := m.execsByService()
	out := make([]Service, 0, len(defs))
	for _, def := range defs {
		out = append(out, project(def, byService[def.ID]))
	}
	return out, nil
}

// DeclaredPorts is every port the repository's declarations name, deduplicated
// and in declaration order. It is what the listening-port watcher folds into
// its snapshot so a declared port is reported whatever procfs shows (ADR 0076),
// and it reads the declarations rather than the exec records deliberately: a
// port is declared by a file, not by a run, and the script that published it
// has usually exited by the time the port matters.
//
// A declaration that cannot run still contributes its ports. The file says the
// port exists; a missing executable bit says nothing about that either way.
func (m *Manager) DeclaredPorts() ([]int, error) {
	defs, err := Discover(m.root)
	if err != nil {
		return nil, err
	}
	var out []int
	seen := map[int]struct{}{}
	for _, def := range defs {
		for _, port := range def.Ports {
			if _, duplicate := seen[port]; duplicate {
				continue
			}
			seen[port] = struct{}{}
			out = append(out, port)
		}
	}
	return out, nil
}

// Get is one declared service by id.
func (m *Manager) Get(id string) (Service, error) {
	def, err := m.definition(id)
	if err != nil {
		return Service{}, err
	}
	return project(def, m.execsByService()[def.ID]), nil
}

// Start brings a service up, and is a no-op on one that is already running.
//
// A service that has run before is relaunched under its existing exec id
// rather than given a new one, so its identity — and the transcript and the
// tab keyed on it — survives being stopped and started again.
func (m *Manager) Start(ctx context.Context, id string) (Service, error) {
	def, err := m.definition(id)
	if err != nil {
		return Service{}, err
	}
	if !def.Runnable() {
		return Service{}, fmt.Errorf("service %s cannot run: %s", def.ID, def.Problem)
	}
	unlock := m.lock(def.ID)
	defer unlock()
	exec, err := m.startLocked(ctx, def)
	if err != nil {
		return Service{}, err
	}
	return project(def, &exec), nil
}

// Stop ends a service's run and keeps its record, so what it printed is still
// readable and starting it again resumes the same identity.
func (m *Manager) Stop(ctx context.Context, id string) (Service, error) {
	def, err := m.definition(id)
	if err != nil {
		return Service{}, err
	}
	unlock := m.lock(def.ID)
	defer unlock()
	exec, ok := m.execFor(def.ID)
	if !ok {
		// Nothing has ever run it, which is the state stopping asks for.
		return project(def, nil), nil
	}
	stopped, err := m.execs.Stop(ctx, exec.ID)
	if err != nil {
		return Service{}, err
	}
	return project(def, &stopped), nil
}

// Restart stops a running service and starts it again under the same exec id.
// A service that is not running is simply started, so restart is the verb that
// always ends with it up.
func (m *Manager) Restart(ctx context.Context, id string) (Service, error) {
	def, err := m.definition(id)
	if err != nil {
		return Service{}, err
	}
	if !def.Runnable() {
		return Service{}, fmt.Errorf("service %s cannot run: %s", def.ID, def.Problem)
	}
	unlock := m.lock(def.ID)
	defer unlock()
	if exec, ok := m.execFor(def.ID); ok && live(exec) {
		if _, err := m.execs.Stop(ctx, exec.ID); err != nil {
			return Service{}, err
		}
	}
	exec, err := m.startLocked(ctx, def)
	if err != nil {
		return Service{}, err
	}
	return project(def, &exec), nil
}

// Logs is the transcript of a service's current or last run. A service that has
// never run has no transcript, which is not an error: it is an empty one.
func (m *Manager) Logs(ctx context.Context, id string) ([]execs.LogEntry, error) {
	def, err := m.definition(id)
	if err != nil {
		return nil, err
	}
	exec, ok := m.execFor(def.ID)
	if !ok {
		return nil, nil
	}
	return m.execs.Logs(ctx, exec.ID)
}

// EnsureStarted starts every runnable declared service, once, and is what the
// sandbox calls at boot.
//
// The launches are sequenced but the services are not ordered: each is started
// without waiting for the one before it to be ready, so a declaration order is
// a listing order and never a dependency (ADR 0070 §1). A service that fails to
// start is logged and the rest still come up — one broken script must not cost
// you the others.
func (m *Manager) EnsureStarted(ctx context.Context, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	defs, err := Discover(m.root)
	if err != nil {
		return err
	}
	for _, def := range defs {
		if !def.Runnable() {
			logger.Warn("skipping sandbox service declaration", "service", def.ID, "file", def.FileName, "problem", def.Problem)
			continue
		}
		if _, err := m.Start(ctx, def.ID); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			logger.Error("start sandbox service", "service", def.ID, "error", err)
		}
	}
	return nil
}

// startLocked creates or relaunches the service's exec. The caller holds the
// service's lifecycle lock.
func (m *Manager) startLocked(ctx context.Context, def Definition) (execs.Exec, error) {
	if existing, ok := m.execFor(def.ID); ok {
		if live(existing) {
			return existing, nil
		}
		// Relaunch re-runs the record's own command under a fresh unit
		// generation, keeping the exec id. Env and user are deliberately left
		// to the manager to re-resolve, so a service picks up the sandbox's
		// current secrets rather than the ones its first run was given.
		//
		// Relaunch prepares the run; Start is what tells the new shim to run
		// it. A relaunch that is never started leaves a service sitting in
		// `starting` with a process that never began.
		relaunched, err := m.execs.Relaunch(ctx, execs.RelaunchRequest{ID: existing.ID})
		if err != nil {
			return relaunched, err
		}
		return m.execs.Start(ctx, relaunched.ID)
	}
	// The script runs through the run user's login shell, so it gets the PATH,
	// the profile and the direnv a person typing the same command would get —
	// and so a script with a shebang is executed by it rather than by us
	// deciding what interpreter it meant.
	created, err := m.execs.Create(ctx, execs.CreateRequest{
		Shell:            true,
		ShellCommandLine: execs.QuoteShellArg(def.Path),
		// A service runs in the repository root it was declared in, named
		// rather than left to the exec default. The two resolve to the same
		// directory today — discovery is rooted at that default — but a
		// service script reads its own repository through relative paths, and
		// which directory those are relative to must be a property of the
		// declaration rather than a coincidence of two derivations agreeing.
		// It is recorded on the exec, so a restart resumes in the same place.
		Workdir: m.root,
		// Pipes, not a PTY: a service's output is read after the fact, and
		// stdout and stderr are worth keeping apart (ADR 0070 §3).
		TTY: false,
		Metadata: map[string]string{
			MetadataServiceID:   def.ID,
			MetadataServiceName: def.Name,
		},
	})
	if err != nil {
		return created, err
	}
	return m.execs.Start(ctx, created.ID)
}

func (m *Manager) definition(id string) (Definition, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Definition{}, ErrNotFound
	}
	defs, err := Discover(m.root)
	if err != nil {
		return Definition{}, err
	}
	for _, def := range defs {
		if def.ID == id {
			return def, nil
		}
	}
	return Definition{}, ErrNotFound
}

// execsByService indexes the exec listing by the service each exec runs. A
// service has at most one exec — relaunch reuses the id — but a listing is
// taken from disk and a stale record could still double up, so the newest
// wins rather than whichever was read first.
func (m *Manager) execsByService() map[string]*execs.Exec {
	list := m.execs.List()
	out := map[string]*execs.Exec{}
	for i := range list {
		id := strings.TrimSpace(list[i].Metadata[MetadataServiceID])
		if id == "" {
			continue
		}
		if current, ok := out[id]; ok && current.CreatedAt.After(list[i].CreatedAt) {
			continue
		}
		out[id] = &list[i]
	}
	return out
}

func (m *Manager) execFor(id string) (execs.Exec, bool) {
	exec, ok := m.execsByService()[id]
	if !ok {
		return execs.Exec{}, false
	}
	return *exec, true
}

func (m *Manager) lock(id string) func() {
	m.mu.Lock()
	mu, ok := m.lifecycle[id]
	if !ok {
		mu = &sync.Mutex{}
		m.lifecycle[id] = mu
	}
	m.mu.Unlock()
	mu.Lock()
	return mu.Unlock
}

// live reports whether an exec is running or on its way there.
func live(exec execs.Exec) bool {
	switch exec.Status {
	case execs.StatusStarting, execs.StatusRunning:
		return true
	default:
		return false
	}
}

// project joins a declaration to the exec running it. A declaration with no
// exec is stopped, which is also what a service nobody has started reads as:
// the two are the same state and the listing does not distinguish them.
func project(def Definition, exec *execs.Exec) Service {
	svc := Service{Definition: def, Status: StatusStopped}
	if exec == nil {
		return svc
	}
	svc.ExecID = exec.ID
	svc.PID = exec.PID
	svc.ExitCode = exec.ExitCode
	svc.StartedAt = exec.StartedAt
	svc.ExitedAt = exec.ExitedAt
	svc.Error = exec.Error
	switch exec.Status {
	case execs.StatusStarting:
		svc.Status = StatusStarting
	case execs.StatusRunning:
		svc.Status = StatusRunning
	default:
		svc.Status = endedStatus(*exec)
	}
	if svc.Status == StatusStopped {
		// Stopping is not a failure, so the reason the last run gave for
		// ending is not one either.
		svc.Error = ""
	}
	return svc
}

// endedStatus reads a run that is over. Stopped is asked first and is the exec
// record's own answer rather than a guess from the exit status: a stopped
// process is killed by a signal, and a signal is indistinguishable after the
// fact from one the service did not ask for.
func endedStatus(exec execs.Exec) Status {
	switch {
	case exec.Stopped:
		return StatusStopped
	case exec.Error != "":
		return StatusFailed
	case exec.ExitCode != nil && *exec.ExitCode != 0:
		return StatusFailed
	case exec.ExitCode != nil:
		return StatusExited
	default:
		// Ended with nothing recorded about how. That is not success, and
		// calling it exited would report a service that vanished as one that
		// finished its work.
		return StatusFailed
	}
}
