package execs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// supervisorDirName is the directory, beside the exec runtime files, a
// Supervisor keeps its units in. It is a subdirectory so the runtime
// directory's own readers — the glob for exec records, the watcher for the
// shim's writes — never see a unit's files.
const supervisorDirName = "units"

// lockSuffix names a unit's lifetime file, and logSuffix the file its shim's
// own output goes to.
const (
	lockSuffix = ".lock"
	logSuffix  = ".log"
)

// defaultSupervisorStopTimeout is how long Stop waits for a shim it asked to
// end before it kills it: the longest the shim's own stop takes — the grace it
// gives its command, the wait after the kill, and closing its server — and a
// margin, so a shim doing exactly what it should is never the one killed.
const defaultSupervisorStopTimeout = shimStopGrace + shimKillDrain + shimShutdownTimeout + 3*time.Second

// Supervisor is the UnitManager for a sandbox without systemd (ADR 0145 §4).
// The agent starts each shim itself, in a session of its own so it outlives
// the agent, and learns that it ended from a lock rather than from a parent's
// wait.
//
// A unit is a file, <unit>.lock, that the shim holds an exclusive flock on for
// as long as it runs. The supervisor takes the lock before the shim starts and
// hands the shim the descriptor; the kernel releases it when the last holder
// exits, however it exits. So the lock answers the one question a unit manager
// is authoritative for — is the run still there — with no pid to go stale,
// nothing to poll, and nothing that has to be the shim's parent: an agent that
// restarted finds the same lock still held by a shim it never started. A file
// whose lock nobody holds is a shim that is gone, and it is collected — removed
// — the way systemd collects a transient unit.
//
// Only the shim ever holds the lock exclusively. Everything in this process
// that asks about it — the probe behind Status and Stop, the goroutine waiting
// for the shim to end — takes it shared, so the supervisor's own questions can
// never read as a shim that is still there: a shared lock is refused only
// while an exclusive one is held.
//
// It is never a source of exit status. That is the shim's own runtime write
// (ADR 0115 §1), which lands before the shim exits and so before its lock is
// released.
type Supervisor struct {
	dir         string
	stopTimeout time.Duration

	mu sync.Mutex
	// waiters holds, per unit, a channel closed when that unit's lock is
	// released. One goroutine waits on each lock; Stop and Watch share it.
	waiters     map[string]chan struct{}
	subscribers map[*unitSubscriber]struct{}
}

// unitRecordSize is the size of every record a unit's lifetime file holds
// (writeUnitState): a pid, an identity, a time and a runtime path, with room to
// spare.
const unitRecordSize = 4096

// unitState is what a unit's lifetime file holds: the shim's pid and kernel
// identity, which Stop signals only while the lock is held and the identity
// still matches, and the exec's runtime file, which says what the shim's
// command was when the shim went.
type unitState struct {
	PID         int       `json:"pid"`
	Identity    string    `json:"identity,omitempty"`
	StartedAt   time.Time `json:"startedAt"`
	RuntimePath string    `json:"runtimePath,omitempty"`
}

// NewSupervisor returns a supervisor that keeps its units in dir.
func NewSupervisor(dir string) *Supervisor {
	return &Supervisor{
		dir:         filepath.Clean(dir),
		stopTimeout: defaultSupervisorStopTimeout,
		waiters:     map[string]chan struct{}{},
		subscribers: map[*unitSubscriber]struct{}{},
	}
}

// HoldLifetime is what a shim does first with the descriptor a Supervisor
// passed it: keeps it, and keeps it from the command it starts. A command that
// inherited it would hold the lock past the shim's own exit, and the shim
// would read as running for as long as anything it started did.
func HoldLifetime(fd uintptr) error {
	return holdLifetime(fd)
}

func (s *Supervisor) lockPath(unit string) string {
	return filepath.Join(s.dir, safeName(unit)+lockSuffix)
}

func (s *Supervisor) logPath(unit string) string {
	return filepath.Join(s.dir, safeName(unit)+logSuffix)
}

func (s *Supervisor) Start(ctx context.Context, req StartRequest) (StartResult, error) {
	if len(req.Command) == 0 || strings.TrimSpace(req.Command[0]) == "" {
		return StartResult{}, errors.New("exec command is required")
	}
	unit := unitBaseName(req.Unit)
	if unit == "" {
		return StartResult{}, errors.New("unit is required")
	}
	if err := ctx.Err(); err != nil {
		return StartResult{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return StartResult{}, err
	}
	argv, err := shimArgv(exe, req)
	if err != nil {
		return StartResult{}, err
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return StartResult{}, err
	}
	path := s.lockPath(unit)
	held, err := lockHeld(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return StartResult{}, fmt.Errorf("start %s: %w", unit, err)
	case held:
		return StartResult{}, fmt.Errorf("start %s: unit already exists", unit)
	default:
		// A unit of this name ended and is not collected yet. Its collection
		// removes the file by name, so it has to be over before a new one takes
		// that name.
		select {
		case <-s.gone(unit):
		case <-ctx.Done():
			return StartResult{}, ctx.Err()
		}
	}
	// The lock is taken on a file no reader can find yet, and the file is put
	// in place still locked, before the shim starts: there is no moment at
	// which the unit exists and its lock is free — which would read as a shim
	// that already ended — and none at which a shim runs under a unit nothing
	// can find, which an agent dying between the two would otherwise leave.
	lifetime, err := os.CreateTemp(s.dir, ".start-*")
	if err != nil {
		return StartResult{}, err
	}
	defer lifetime.Close()
	abandon := func() { _ = os.Remove(lifetime.Name()) }
	if ok, err := lockFile(lifetime, true, false); err != nil || !ok {
		abandon()
		if err == nil {
			err = errors.New("lifetime lock is held")
		}
		return StartResult{}, fmt.Errorf("start %s: %w", unit, err)
	}
	state := unitState{StartedAt: time.Now().UTC(), RuntimePath: req.RuntimePath}
	if err := writeUnitState(int(lifetime.Fd()), state); err != nil {
		abandon()
		return StartResult{}, fmt.Errorf("start %s: %w", unit, err)
	}
	if err := os.Rename(lifetime.Name(), path); err != nil {
		abandon()
		return StartResult{}, fmt.Errorf("start %s: %w", unit, err)
	}
	abandon = func() { _ = os.Remove(path) }
	logFile, err := os.OpenFile(s.logPath(unit), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		abandon()
		return StartResult{}, err
	}
	// Not the caller's context: a shim outlives the call that started it, and
	// the agent too. Stop is what ends it.
	cmd := exec.CommandContext(context.Background(), argv[0], argv[1:]...) //nolint:gosec // this binary, as its own exec shim.
	cmd.Dir = strings.TrimSpace(req.Workdir)
	cmd.Env = shimEnvironment(req.Env)
	// The shim's own diagnostics, which systemd would have sent to the journal.
	// Never this process's stderr: a shim outlives the agent, and a Go program
	// writing to a pipe whose reader is gone is killed by SIGPIPE.
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = shimSysProcAttr()
	cmd.Args = append(cmd.Args, "--lifetime", inheritLifetime(cmd, lifetime))
	if err := cmd.Start(); err != nil {
		logFile.Close()
		abandon()
		return StartResult{}, fmt.Errorf("start %s: %w", unit, err)
	}
	logFile.Close()
	pid := cmd.Process.Pid
	// The shim records its own pid and identity as it starts (HoldLifetime),
	// in case this process does not live to; written here as well, it is in
	// the file by the time Start returns.
	state.PID = pid
	state.Identity, _ = processIdentity(pid)
	if err := writeUnitState(int(lifetime.Fd()), state); err != nil {
		// A unit naming no shim could not be stopped, so the shim is ended
		// here — through its handle, while it is still this process's
		// unreaped child — rather than left to run unsupervised.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		abandon()
		return StartResult{}, fmt.Errorf("start %s: %w", unit, err)
	}
	// Reap it. The shim is this process's child until this process exits, and
	// a zombie holds no lock but would still answer a signal.
	go func() { _ = cmd.Wait() }()
	s.gone(unit)
	return StartResult{Unit: unit, PID: int64(pid)}, nil
}

// shimEnvironment is a shim's environment: the exec's, as a systemd unit's is
// its Environment property, and never nil, which os/exec would read as
// "inherit this process's". What the exec does not set, the shim does not get —
// except PATH, which systemd gives every service it starts and the shim needs
// to find the command at all; the agent's own is the nearest equivalent.
func shimEnvironment(env map[string]string) []string {
	out := append([]string{}, unitEnvironment(env)...)
	if _, ok := env["PATH"]; !ok {
		if path, ok := os.LookupEnv("PATH"); ok {
			out = append(out, "PATH="+path)
		}
	}
	return out
}

// Stop asks the unit's shim to end, which ends its command's process group,
// and kills the shim if it has not gone within the stop timeout. What it
// cannot do is what systemd's control group does: a process the command moved
// into a session of its own is not the shim's to end.
func (s *Supervisor) Stop(ctx context.Context, unit string) error {
	unit = unitBaseName(unit)
	if unit == "" {
		return nil
	}
	if _, err := s.readState(unit); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	gone := s.gone(unit)
	pid, err := s.shim(ctx, unit)
	if err != nil {
		return fmt.Errorf("stop %s: %w", unit, err)
	}
	if pid > 0 {
		if err := terminateProcess(pid); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("stop %s: %w", unit, err)
		}
		timer := time.NewTimer(s.stopTimeout)
		defer timer.Stop()
		select {
		case <-gone:
			return nil
		case <-timer.C:
			// Asked again, because the shim may have gone and its number been
			// taken while it was given its time.
			if pid, err := s.shim(ctx, unit); err == nil && pid > 0 {
				_ = killProcess(pid)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case <-gone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// shimPIDWait bounds how long shim waits for a held unit to name its shim. A
// unit is put in place before its shim starts, and the shim's pid is written
// once it has — by the supervisor, or by the shim itself — so a held unit
// naming none is a shim in its first moments.
const shimPIDWait = 2 * time.Second

// shim is the pid of the unit's running shim, or zero when it has none: the
// lock is free, or the process holding that pid is no longer the shim the unit
// recorded. Nothing signals a unit's shim without asking this first — a lock
// held a moment ago, or a pid read from a file, says nothing about what that
// number is now.
func (s *Supervisor) shim(ctx context.Context, unit string) (int, error) {
	deadline := time.Now().Add(shimPIDWait)
	for {
		state, err := s.readState(unit)
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		held, err := lockHeld(s.lockPath(unit))
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		if !held {
			return 0, nil
		}
		if state.PID > 0 {
			// A process that is gone has no identity, which matches none.
			if current, _ := processIdentity(state.PID); state.Identity != "" && current != state.Identity {
				return 0, nil
			}
			return state.PID, nil
		}
		if time.Now().After(deadline) {
			return 0, errors.New("unit names no shim")
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

// Status reports a unit as running while its lock is held, and as unloaded
// once it is not: a shim that is gone has nothing to come back to, which is
// exactly what systemd says of a collected transient unit.
func (s *Supervisor) Status(_ context.Context, unit string) (UnitStatus, error) {
	unit = unitBaseName(unit)
	if unit == "" {
		return UnitStatus{}, errors.New("unit is required")
	}
	return s.status(unit)
}

func (s *Supervisor) status(unit string) (UnitStatus, error) {
	gone := UnitStatus{Unit: unit, Status: StatusLost}
	state, err := s.readState(unit)
	if errors.Is(err, fs.ErrNotExist) {
		return gone, nil
	}
	if err != nil {
		return UnitStatus{}, err
	}
	held, err := lockHeld(s.lockPath(unit))
	if errors.Is(err, fs.ErrNotExist) {
		return gone, nil
	}
	if err != nil {
		return UnitStatus{}, err
	}
	// Either way something is waiting on the lock from here on: a unit this
	// process did not start — one from before an agent restart — is watched
	// from the first time anything asks about it.
	s.gone(unit)
	if !held {
		return gone, nil
	}
	started := state.StartedAt
	return UnitStatus{
		Unit:      unit,
		Loaded:    true,
		Active:    true,
		Status:    StatusRunning,
		PID:       int64(state.PID),
		StartedAt: &started,
	}, nil
}

// List reports every unit whose shim is still running.
func (s *Supervisor) List(context.Context) ([]UnitStatus, error) {
	units, err := s.units()
	if err != nil {
		return nil, err
	}
	out := make([]UnitStatus, 0, len(units))
	for _, unit := range units {
		status, err := s.status(unit)
		if err != nil || !status.Loaded {
			continue
		}
		out = append(out, status)
	}
	return out, nil
}

// Watch delivers the name of every unit whose shim ended, until ctx does.
// Every unit already on disk is waited on from here, so a watcher started by
// an agent that restarted hears about shims the agent before it started. It
// never reports lost changes: nothing is dropped, because a name already
// queued for a subscriber is queued once however often it changes.
func (s *Supervisor) Watch(ctx context.Context) (<-chan string, error) {
	sub := &unitSubscriber{pending: map[string]struct{}{}, wake: make(chan struct{}, 1)}
	s.mu.Lock()
	s.subscribers[sub] = struct{}{}
	s.mu.Unlock()
	units, err := s.units()
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.unsubscribe(sub)
		return nil, err
	}
	for _, unit := range units {
		s.gone(unit)
	}
	out := make(chan string)
	go func() {
		defer close(out)
		defer s.unsubscribe(sub)
		for {
			select {
			case <-ctx.Done():
				return
			case <-sub.wake:
			}
			for _, unit := range sub.take() {
				select {
				case out <- unit:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func (s *Supervisor) unsubscribe(sub *unitSubscriber) {
	s.mu.Lock()
	delete(s.subscribers, sub)
	s.mu.Unlock()
}

// units names every unit on disk.
func (s *Supervisor) units() ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(s.dir, "*"+lockSuffix))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(matches))
	for _, path := range matches {
		out = append(out, strings.TrimSuffix(filepath.Base(path), lockSuffix))
	}
	return out, nil
}

func (s *Supervisor) readState(unit string) (unitState, error) {
	data, err := os.ReadFile(s.lockPath(unit))
	if err != nil {
		return unitState{}, err
	}
	var state unitState
	if err := json.Unmarshal(data, &state); err != nil {
		return unitState{}, fmt.Errorf("read unit %s: %w", unit, err)
	}
	return state, nil
}

// gone returns a channel closed once the unit's shim has ended, starting the
// one goroutine that waits for it if nothing is yet.
func (s *Supervisor) gone(unit string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.waiters[unit]; ok {
		return ch
	}
	ch := make(chan struct{})
	s.waiters[unit] = ch
	go s.await(unit, ch)
	return ch
}

// await blocks on the unit's lock until its shim releases it — by exiting,
// which is the only way it does — then collects the unit and tells every
// subscriber. A lock that cannot be waited on is not a shim that ended, so
// that failure closes nothing and reports nothing: declaring the unit gone
// would have its exec relaunched over a shim that is still running.
func (s *Supervisor) await(unit string, ch chan struct{}) {
	ended := false
	if file, err := os.Open(s.lockPath(unit)); errors.Is(err, fs.ErrNotExist) {
		ended = true
	} else if err == nil {
		if ok, err := lockFile(file, false, true); err == nil && ok {
			if state, err := s.readState(unit); err == nil {
				endOrphanedCommand(state)
			}
			_ = os.Remove(s.lockPath(unit))
			removeIfEmpty(s.logPath(unit))
			ended = true
		}
		file.Close()
	}
	s.mu.Lock()
	if s.waiters[unit] == ch {
		delete(s.waiters, unit)
	}
	subscribers := make([]*unitSubscriber, 0, len(s.subscribers))
	for sub := range s.subscribers {
		subscribers = append(subscribers, sub)
	}
	s.mu.Unlock()
	if !ended {
		return
	}
	close(ch)
	for _, sub := range subscribers {
		sub.add(unit)
	}
}

// endOrphanedCommand ends the command of a shim that went without recording
// that its command did. A shim killed outright — SIGKILL, out of memory, or
// Stop's last resort against a shim that would not end — cannot end its command
// on the way out, and nothing else would: systemd's control group does this
// for a unit, and here there is no control group, so the command's session is
// what is ended. A command that exited is left alone, because its pid may
// already be someone else's.
func endOrphanedCommand(state unitState) {
	if state.RuntimePath == "" {
		return
	}
	exec, err := readRuntime(state.RuntimePath)
	if err != nil || settled(exec) || exec.PID <= 0 {
		return
	}
	// Only while the command itself is still alive and is that command. The
	// shim may have gone long ago — while the agent was down, or before a
	// reboot that left this file behind — so a session whose leader is gone is
	// left alone here, unlike in the shim's own stop: by now it may be another
	// exec's, whose command started a server and exited.
	if pid := int(exec.PID); isCommand(pid, exec.ProcessIdentity) {
		_ = endSession(pid, exec.ProcessIdentity)
	}
}

// removeIfEmpty removes a shim's log once the shim has gone, unless it said
// something. A shim writes there only when it fails — a bad argument, a socket
// it could not bind, a panic — and that is exactly the output worth keeping
// after it has gone, as systemd's journal would have.
func removeIfEmpty(path string) {
	if info, err := os.Stat(path); err == nil && info.Size() == 0 {
		_ = os.Remove(path)
	}
}

// lockHeld reports whether a shim holds the lock on path, without waiting.
func lockHeld(path string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	acquired, err := lockFile(file, false, false)
	if err != nil {
		return false, err
	}
	// Taking it was only the question; closing the file gives it back.
	return !acquired, nil
}

// unitSubscriber is one Watch call's queue of units that ended.
type unitSubscriber struct {
	mu      sync.Mutex
	pending map[string]struct{}
	order   []string
	wake    chan struct{}
}

func (u *unitSubscriber) add(unit string) {
	u.mu.Lock()
	if _, ok := u.pending[unit]; !ok {
		u.pending[unit] = struct{}{}
		u.order = append(u.order, unit)
	}
	u.mu.Unlock()
	select {
	case u.wake <- struct{}{}:
	default:
	}
}

func (u *unitSubscriber) take() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := u.order
	u.order = nil
	clear(u.pending)
	return out
}
