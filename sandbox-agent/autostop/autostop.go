// Package autostop powers the sandbox off once nothing has used it for the
// idle timeout (ADR 0108, ADR 0124, ADR 26-10-16-424).
//
// The decision is made here, inside the sandbox, because every fact it needs
// is first-hand here and second-hand everywhere else: the shims hold what each
// exec's terminal shows and who its clients are, this process serves its own
// TCP tunnels, and a keepalive lease is a file only a process in the sandbox can write.
//
// Who carries the decision out depends on the pool. Until it hands the
// sandbox's power to the control plane (SetControlPlanePower), the policy
// stops the sandbox itself with the platform's power-off — systemd's
// poweroff.target on Linux, shutdown(8) on darwin — the one part of the policy
// that is per platform (ADR 0145 §4). After, it only reports: idle, a pending
// sleep request, and the next wake, and the control plane powers the sandbox
// down once the sandbox confirms, fresh, what it reported (Confirm; ADR
// 26-10-16-337). Either way the next sandbox-directed request starts it again
// (ADR 0017 §§10, 12).
//
// Sleep and wake are files, as leases are: a file in the sleep directory asks
// to be powered down now, whatever else is going on, and a file in the wake
// directory names, by its mtime, when the control plane should start the
// sandbox again. Names beginning with "." are ignored in both, so a wake can be
// written and dated before it is put in place.
package autostop

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/discobox-ai/discobox/programstatus"
	"github.com/discobox-ai/discobox/sandbox-agent/execs"
)

const (
	// DefaultIdleTimeout is how long a sandbox runs with nothing happening in
	// it when its pool's provider sets no idle timeout of its own
	// (ADR 0108 §3). A sandbox that must outlast it holds a lease.
	DefaultIdleTimeout = 30 * time.Minute
	// DefaultInterval is how often the policy looks. It bounds how late a stop
	// can be, not how early.
	DefaultInterval = 30 * time.Second
	// DefaultLeaseDir holds the keepalive leases: every regular file in it is
	// one, and its mtime counts as activity (ADR 0108 §4). It is on /run, so a
	// lease dies with the boot that took it.
	DefaultLeaseDir = "/run/discobox/keepalive"
	// DefaultSleepDir holds the sleep requests: every regular file in it asks
	// to power the sandbox down now (ADR 26-10-16-337 §1). It is on /run, so a
	// request a shutdown never consumed dies with the boot.
	DefaultSleepDir = "/run/discobox/sleep"
	// WakeDoneDir is where a wake goes once a boot has handed it to the
	// harness, inside the wake directory: out of scheduling, its note still
	// readable (ADR 26-10-16-337 §7). Its leading "." keeps it from being read
	// as a wake.
	WakeDoneDir = ".done"
	// DefaultControlPlaneMarker exists while the control plane holds the
	// sandbox's power, so a process in the sandbox — discobox-power above all
	// — can tell whether a wake will be acted on (ADR 26-10-16-337 §8). It is
	// on /run, so a boot starts without it until the pool says again.
	DefaultControlPlaneMarker = "/run/discobox/control-plane-power"
	// DefaultHandoverMarker records that this boot has handed its due wakes
	// over (TakeDueWakes): the agent restarting within a boot must not take
	// them again for a launch that is not coming. On /run, it ends with the
	// boot.
	DefaultHandoverMarker = "/run/discobox/wakes-handed-over"
)

// ErrNotConfirmed refuses a power-down the sandbox does not see a reason for
// now: it is no longer idle, or no sleep request is pending.
var ErrNotConfirmed = errors.New("the sandbox does not confirm the power-down")

// Reason is why the control plane is powering the sandbox down.
type Reason string

const (
	ReasonIdle  Reason = "idle"
	ReasonSleep Reason = "sleep"
)

// Owner is who the wake directory's handed-over files belong to: the run user,
// who wrote them and whose harness reads them.
type Owner struct {
	UID, GID int
}

// Config wires a Policy to the sandbox it watches.
type Config struct {
	// Execs lists every exec the sandbox has. Its shim-reported fields — when
	// what it shows last changed, who is attached, when a client last acted —
	// are the activity the policy reads.
	Execs func() []execs.Exec
	// LeaseDir defaults to DefaultLeaseDir.
	LeaseDir string
	// SleepDir defaults to DefaultSleepDir.
	SleepDir string
	// WakeDir is ~/.discobox/wake in the run user's home. Empty reads no
	// wakes: a sandbox whose run user does not resolve still stops.
	WakeDir string
	// WakeOwner owns what a boot moves into WakeDir's WakeDoneDir.
	WakeOwner *Owner
	// ControlPlaneMarker defaults to DefaultControlPlaneMarker.
	ControlPlaneMarker string
	// HandoverMarker defaults to DefaultHandoverMarker.
	HandoverMarker string
	// IdleTimeout defaults to DefaultIdleTimeout.
	IdleTimeout time.Duration
	// Interval defaults to DefaultInterval.
	Interval time.Duration
	// PowerOff stops the sandbox. It defaults to this platform's power-off,
	// and tests replace it: nothing else about the policy needs a real
	// machine.
	PowerOff func(context.Context) error
	// Now defaults to time.Now.
	Now    func() time.Time
	Logger *slog.Logger
}

// Policy decides when the sandbox has been idle long enough to stop.
//
// Exec activity is read from the shims on every evaluation, and leases from the
// directory. Two things are kept as well, because nothing else records them:
// the client connections this process is serving, and the latest activity it
// has ever seen. The second is not a cache. An exec that ends takes its shim's
// record of access with it, so a client that spent forty minutes on a command
// that has just finished would otherwise count for nothing, and the sandbox
// would stop at the next tick. Leases are the exception, and are never
// remembered: removing one has to release it.
type Policy struct {
	execs     func() []execs.Exec
	leaseDir  string
	sleepDir  string
	wakeDir   string
	wakeOwner *Owner
	// controlPlaneMarker and handoverMarker are DefaultControlPlaneMarker and
	// DefaultHandoverMarker unless a test moves them.
	controlPlaneMarker string
	handoverMarker     string
	interval           time.Duration
	powerOff           func(context.Context) error
	now                func() time.Time
	logger             *slog.Logger
	startedAt          time.Time

	// running is set while Run is: the policy's view is only reported while
	// the policy is actually in force.
	running atomic.Bool
	// controlPlane is set once the pool hands the sandbox's power to the
	// control plane: from then on the policy reports rather than powers off.
	controlPlane atomic.Bool

	mu sync.Mutex
	// idleTimeout is the timeout in force. It changes when the pool delivers a
	// new one (SetIdleTimeout), which takes effect at the next evaluation.
	idleTimeout time.Duration
	// holds are the client connections this process is serving, by the order
	// they arrived, each with what it is for the log line a stop leaves.
	holds    map[uint64]string
	nextHold uint64
	// seen is the latest non-lease activity any evaluation has observed,
	// starting from this process's start, and connections' departures are
	// written straight into it.
	seen activity
}

// State is the policy's view of the sandbox at one moment.
type State struct {
	IdleTimeout time.Duration
	// LastActivityAt is the latest activity of any kind, which is never
	// earlier than this process's start. A connected client makes it now; a
	// lease dated in the future makes it that date.
	LastActivityAt time.Time
	// LastActivity says what LastActivityAt was, for the log line a stop
	// leaves behind and for anyone asking why a sandbox is still up.
	LastActivity string
	// StopsAt is LastActivityAt plus the idle timeout: the earliest the
	// sandbox stops if nothing else happens.
	StopsAt time.Time
	// LeaseUntil is the latest lease's mtime when it is in the future, and
	// zero when no lease is.
	LeaseUntil time.Time
	// ControlPlanePower says the pool has handed the sandbox's power to the
	// control plane. Idle, Sleep and NextWake are reported only while it is
	// set (ADR 26-10-16-337 §8).
	ControlPlanePower bool
	// Idle is now at or past StopsAt: the point at which the policy used to
	// power the sandbox off.
	Idle bool
	// Sleep is the latest pending sleep request, zero when none is.
	Sleep Request
	// NextWake is the earliest wake in the future, zero when none is.
	NextWake Request
}

// Request is one sleep or wake file: its mtime and its name.
type Request struct {
	At   time.Time
	Name string
}

// IsZero reports whether there is no such request.
func (r Request) IsZero() bool { return r.At.IsZero() }

// New builds a policy. It does nothing until Run.
func New(cfg Config) *Policy {
	if cfg.LeaseDir == "" {
		cfg.LeaseDir = DefaultLeaseDir
	}
	if cfg.SleepDir == "" {
		cfg.SleepDir = DefaultSleepDir
	}
	if cfg.ControlPlaneMarker == "" {
		cfg.ControlPlaneMarker = DefaultControlPlaneMarker
	}
	if cfg.HandoverMarker == "" {
		cfg.HandoverMarker = DefaultHandoverMarker
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.PowerOff == nil {
		cfg.PowerOff = platformPowerOff
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Execs == nil {
		cfg.Execs = func() []execs.Exec { return nil }
	}
	start := cfg.Now().UTC()
	return &Policy{
		execs:              cfg.Execs,
		leaseDir:           cfg.LeaseDir,
		sleepDir:           cfg.SleepDir,
		wakeDir:            cfg.WakeDir,
		wakeOwner:          cfg.WakeOwner,
		controlPlaneMarker: cfg.ControlPlaneMarker,
		handoverMarker:     cfg.HandoverMarker,
		idleTimeout:        cfg.IdleTimeout,
		interval:           cfg.Interval,
		powerOff:           cfg.PowerOff,
		now:                cfg.Now,
		logger:             cfg.Logger,
		startedAt:          start,
		holds:              map[uint64]string{},
		seen:               activity{at: start, what: "sandbox agent start"},
	}
}

// SetIdleTimeout replaces the timeout in force, from the next evaluation on:
// the pool delivers its idle timeout in the sandbox's runtime config, and a
// changed one applies to the running sandbox rather than at its next start
// (ADR 26-10-08-127 §5). Zero or less restores DefaultIdleTimeout. Activity
// already seen is kept, so a shorter timeout can stop a sandbox at the next
// tick when it has already been idle that long.
func (p *Policy) SetIdleTimeout(timeout time.Duration) {
	if p == nil {
		return
	}
	if timeout <= 0 {
		timeout = DefaultIdleTimeout
	}
	p.mu.Lock()
	p.idleTimeout = timeout
	p.mu.Unlock()
}

// SetControlPlanePower hands the sandbox's power to the control plane, or
// takes it back: the pool says which in the runtime config it delivers
// (ADR 26-10-16-337 §8). While it is handed over the policy reports idle,
// sleep and wake and never powers the sandbox off itself.
//
// The marker file follows it, for a process in the sandbox to read.
func (p *Policy) SetControlPlanePower(on bool) {
	if p == nil {
		return
	}
	p.controlPlane.Store(on)
	var err error
	if on {
		// /run is fresh at every boot, and this runs before anything else
		// may have made the directory.
		if err = os.MkdirAll(filepath.Dir(p.controlPlaneMarker), 0o755); err == nil {
			err = os.WriteFile(p.controlPlaneMarker, nil, 0o644)
		}
	} else {
		err = os.Remove(p.controlPlaneMarker)
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	}
	if err != nil {
		p.logger.Warn("autostop control-plane marker", "path", p.controlPlaneMarker, "error", err)
	}
}

// Hold marks a client connection this process serves — an exec attach, a TCP
// tunnel — as open; what says which, for the log line a stop leaves. The
// sandbox is active until release is called, and the release is activity too,
// so the idle clock starts when the client left.
//
// A shim reports its own attachers, but only for as long as its exec lasts:
// holding here is what keeps a client that has just left counting after the
// command it ran has ended.
func (p *Policy) Hold(what string) (release func()) {
	if p == nil {
		return func() {}
	}
	p.mu.Lock()
	p.nextHold++
	id := p.nextHold
	p.holds[id] = what
	p.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			delete(p.holds, id)
			// At least as new as anything seen, since it is happening now,
			// and it replaces an equally recent "connected" so the log says
			// the client left rather than that it is still here.
			if left := p.now().UTC(); !left.Before(p.seen.at) {
				p.seen = activity{at: left, what: "client left: " + what}
			}
			p.mu.Unlock()
		})
	}
}

// Status reports the policy's view while it is running, and nothing when it is
// not — a configure-mode sandbox, which never runs it, does not claim a stop
// time it will never reach.
func (p *Policy) Status() (State, bool) {
	if p == nil || !p.running.Load() {
		return State{}, false
	}
	return p.Evaluate(p.now().UTC()), true
}

// Evaluate computes the policy's view at now from the execs, the open
// connections, and the leases as they are at this moment, and from the latest
// activity it has seen before.
func (p *Policy) Evaluate(now time.Time) State {
	var observed activity
	for _, e := range p.execs() {
		if e.AttacherCount > 0 {
			observed.consider(now, "client attached to exec "+e.ID)
		}
		if e.LastAccessedAt != nil {
			observed.consider(*e.LastAccessedAt, "client on exec "+e.ID)
		}
		if e.ProgramStatus != nil {
			observed.consider(e.ProgramStatus.ReportedAt, "status report on exec "+e.ID)
		}
		switch {
		case e.ProgramStatus != nil && programstatus.Live(e.ProgramStatus.State):
			// A program that says what it is doing replaces guessing it from
			// its screen (ADR 26-10-16-424 §4): working is activity for as
			// long as it lasts, and idle and blocked on a person are idle
			// from the report that said so, however its screen moves.
			if e.ProgramStatus.State == programstatus.Working {
				observed.consider(now, "exec "+e.ID+" reports working")
			}
		case e.ScreenChangedAt != nil:
			// No report, or only a result one (done, error) that outlives
			// the program that reported it: whatever runs in the terminal
			// now says nothing of itself, so its screen does.
			observed.consider(*e.ScreenChangedAt, "screen change on exec "+e.ID)
		}
	}
	p.mu.Lock()
	if what, ok := p.oldestHoldLocked(); ok {
		observed.consider(now, "client connected: "+what)
	}
	p.seen.consider(observed.at, observed.what)
	latest := p.seen
	idleTimeout := p.idleTimeout
	p.mu.Unlock()

	var leaseUntil time.Time
	for _, lease := range p.leases() {
		latest.consider(lease.at, "lease "+lease.what)
		if lease.at.After(now) && lease.at.After(leaseUntil) {
			leaseUntil = lease.at
		}
	}
	state := State{
		IdleTimeout:       idleTimeout,
		LastActivityAt:    latest.at,
		LastActivity:      latest.what,
		StopsAt:           latest.at.Add(idleTimeout),
		LeaseUntil:        leaseUntil,
		ControlPlanePower: p.controlPlane.Load(),
	}
	state.Idle = !now.Before(state.StopsAt)
	for _, request := range ReadRequests(p.sleepDir) {
		if request.At.After(state.Sleep.At) {
			state.Sleep = request
		}
	}
	for _, wake := range p.wakes() {
		if wake.At.After(now) && (state.NextWake.IsZero() || wake.At.Before(state.NextWake.At)) {
			state.NextWake = wake
		}
	}
	return state
}

// Confirm answers the control plane, under the pool's power lock, about a
// power-down it is about to carry out (ADR 26-10-16-337 §5). Idle is
// confirmed only if the sandbox is idle now — a client who attached since the
// report is not cut off. Sleep is confirmed only if a request is pending now,
// and confirming it removes every pending request first, so a sandbox frozen
// rather than shut down does not resume straight into another sleep.
//
// A confirmation carries the sandbox's earliest pending wake: one already due
// that no boot has handed over yet, which asks for a start as soon as the
// sandbox is down, or else the earliest in the future. Zero is none.
func (p *Policy) Confirm(reason Reason) (Request, error) {
	if p == nil {
		return Request{}, ErrNotConfirmed
	}
	now := p.now().UTC()
	switch reason {
	case ReasonIdle:
		if !p.Evaluate(now).Idle {
			return Request{}, ErrNotConfirmed
		}
	case ReasonSleep:
		pending := ReadRequests(p.sleepDir)
		if len(pending) == 0 {
			return Request{}, ErrNotConfirmed
		}
		for _, request := range pending {
			if err := os.Remove(filepath.Join(p.sleepDir, request.Name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return Request{}, fmt.Errorf("consume sleep request %s: %w", request.Name, err)
			}
		}
	default:
		return Request{}, fmt.Errorf("unknown power-down reason %q", reason)
	}
	p.logger.Info("sandbox confirms power-down", "reason", reason)
	var pending Request
	for _, wake := range p.wakes() {
		if pending.IsZero() || wake.At.Before(pending.At) {
			pending = wake
		}
	}
	return pending, nil
}

// TakeDueWakes moves every wake whose time has come into WakeDoneDir and
// returns the moved files' paths, oldest first: what a boot hands the harness
// it launches (ADR 26-10-16-337 §7). A boot is the only place a wake is handed
// over, so one that falls due while the sandbox runs stays where it is until
// the boot after the next power-down — and the agent restarting within a boot
// takes nothing (handoverMarker): it would park the wakes for a primary
// launch that is not coming.
func (p *Policy) TakeDueWakes() ([]string, error) {
	if p == nil || p.wakeDir == "" {
		return nil, nil
	}
	if _, err := os.Stat(p.handoverMarker); err == nil {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(p.handoverMarker), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(p.handoverMarker, nil, 0o644); err != nil {
		return nil, err
	}
	now := p.now().UTC()
	var due []Request
	for _, wake := range p.wakes() {
		if !wake.At.After(now) {
			due = append(due, wake)
		}
	}
	if len(due) == 0 {
		return nil, nil
	}
	sort.Slice(due, func(i, j int) bool { return due[i].At.Before(due[j].At) })
	done := filepath.Join(p.wakeDir, WakeDoneDir)
	if err := os.MkdirAll(done, 0o755); err != nil {
		return nil, err
	}
	if p.wakeOwner != nil {
		if err := os.Lchown(done, p.wakeOwner.UID, p.wakeOwner.GID); err != nil {
			return nil, err
		}
	}
	paths := make([]string, 0, len(due))
	for _, wake := range due {
		target := filepath.Join(done, wake.Name)
		if err := os.Rename(filepath.Join(p.wakeDir, wake.Name), target); err != nil {
			return paths, fmt.Errorf("hand over wake %s: %w", wake.Name, err)
		}
		paths = append(paths, target)
	}
	return paths, nil
}

// wakes reads the wake directory, which holds none when it is not configured.
func (p *Policy) wakes() []Request {
	if p.wakeDir == "" {
		return nil
	}
	return ReadRequests(p.wakeDir)
}

// ReadRequests reads the sleep requests or the wakes in dir: every regular
// file whose name does not begin with "." — its mtime and its name. A symlink
// is not a request and is not followed, and a directory that does not exist
// holds none. It is the one reading of those directories; discobox-power
// shows what it returns.
func ReadRequests(dir string) []Request {
	return readFiles(dir, false)
}

// ReadLeases reads the keepalive leases in dir: every regular file, a
// dot-named one included (ADR 0108 §4) — its mtime and its name.
func ReadLeases(dir string) []Request {
	return readFiles(dir, true)
}

func readFiles(dir string, dotNamed bool) []Request {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	out := make([]Request, 0, len(entries))
	for _, entry := range entries {
		if (!dotNamed && strings.HasPrefix(entry.Name(), ".")) || !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		out = append(out, Request{At: info.ModTime().UTC(), Name: entry.Name()})
	}
	return out
}

// Run evaluates the policy every interval and, until the pool hands the
// sandbox's power to the control plane, powers the sandbox off once it has
// been idle for the timeout. It returns when ctx ends or the power-off has been
// accepted; one that fails is logged and tried again on the next tick. While
// the control plane holds the power it powers nothing off: the status reports
// what it sees, and Confirm answers for it.
func (p *Policy) Run(ctx context.Context) {
	if p == nil {
		return
	}
	for _, dir := range []string{p.leaseDir, p.sleepDir} {
		if err := prepareSharedDir(dir); err != nil {
			// A sandbox with no lease directory still stops when idle; it just
			// cannot be held, or asked to sleep. Refusing to run would keep it
			// up forever instead.
			p.logger.Warn("autostop directory unavailable", "dir", dir, "error", err)
		}
	}
	p.running.Store(true)
	defer p.running.Store(false)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if p.controlPlane.Load() {
			continue
		}
		now := p.now().UTC()
		state := p.Evaluate(now)
		if !state.Sleep.IsZero() {
			// Without the control plane a sleep can only be what it always
			// could: a power-off now. It is consumed first, as a confirmed
			// one is.
			if _, err := p.Confirm(ReasonSleep); err != nil {
				p.logger.Error("autostop consume sleep request", "error", err)
				continue
			}
			p.logger.Info("sandbox asked to sleep, powering off", "request", state.Sleep.Name)
		} else if now.Before(state.StopsAt) {
			continue
		} else {
			p.logger.Info("sandbox idle, powering off",
				"idleTimeout", state.IdleTimeout,
				"lastActivity", state.LastActivity,
				"lastActivityAt", state.LastActivityAt)
		}
		if err := p.powerOff(ctx); err != nil {
			p.logger.Error("autostop power off", "error", err)
			continue
		}
		return
	}
}

// prepareSharedDir makes a lease or sleep directory writable by anyone in the
// sandbox and, being sticky, removable only by whoever wrote each file — so two
// holders can neither shorten nor delete each other's leases (ADR 0108 §4).
func prepareSharedDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Chmod after, because MkdirAll's mode is filtered through the umask.
	return os.Chmod(dir, os.ModeSticky|0o777)
}

// leases reads every lease (ReadLeases), as activity.
func (p *Policy) leases() []activity {
	leases := ReadLeases(p.leaseDir)
	out := make([]activity, 0, len(leases))
	for _, lease := range leases {
		out = append(out, activity{at: lease.At, what: filepath.Join(p.leaseDir, lease.Name)})
	}
	return out
}

// oldestHoldLocked names the longest-open connection, if any is open.
func (p *Policy) oldestHoldLocked() (string, bool) {
	var oldest uint64
	for id := range p.holds {
		if oldest == 0 || id < oldest {
			oldest = id
		}
	}
	what, ok := p.holds[oldest]
	return what, ok
}

type activity struct {
	at   time.Time
	what string
}

func (a *activity) consider(at time.Time, what string) {
	if at.After(a.at) {
		a.at, a.what = at, what
	}
}
