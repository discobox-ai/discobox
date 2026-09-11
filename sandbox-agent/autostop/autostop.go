// Package autostop powers the sandbox off once nothing has used it for the
// idle timeout (ADR 0108).
//
// The decision is made here, inside the sandbox, because every fact it needs
// is first-hand here and second-hand everywhere else: the shims hold each
// exec's title and its clients, this process serves its own TCP tunnels, and a
// keepalive lease is a file only a process in the sandbox can write. Stopping
// is starting systemd's poweroff.target (see systemctlPowerOff for why not
// `systemctl poweroff`). The container exits, the pool agent reports it
// `stopped` like any container that exited, and the next sandbox-directed
// request starts it again (ADR 0017 §§10, 12). Nothing outside the sandbox
// takes part in the decision.
package autostop

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
)

// Config wires a Policy to the sandbox it watches.
type Config struct {
	// Execs lists every exec the sandbox has. Its shim-reported fields — when
	// the title last changed, who is attached, when a client last acted — are
	// the activity the policy reads.
	Execs func() []execs.Exec
	// LeaseDir defaults to DefaultLeaseDir.
	LeaseDir string
	// IdleTimeout defaults to DefaultIdleTimeout.
	IdleTimeout time.Duration
	// Interval defaults to DefaultInterval.
	Interval time.Duration
	// PowerOff stops the sandbox. It defaults to starting systemd's
	// poweroff.target, and tests replace it: nothing else about the policy
	// needs a real machine.
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
	execs       func() []execs.Exec
	leaseDir    string
	idleTimeout time.Duration
	interval    time.Duration
	powerOff    func(context.Context) error
	now         func() time.Time
	logger      *slog.Logger
	startedAt   time.Time

	// running is set while Run is: the policy's view is only reported while
	// the policy is actually in force.
	running atomic.Bool

	mu sync.Mutex
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
}

// New builds a policy. It does nothing until Run.
func New(cfg Config) *Policy {
	if cfg.LeaseDir == "" {
		cfg.LeaseDir = DefaultLeaseDir
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.PowerOff == nil {
		cfg.PowerOff = systemctlPowerOff
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
		execs:       cfg.Execs,
		leaseDir:    cfg.LeaseDir,
		idleTimeout: cfg.IdleTimeout,
		interval:    cfg.Interval,
		powerOff:    cfg.PowerOff,
		now:         cfg.Now,
		logger:      cfg.Logger,
		startedAt:   start,
		holds:       map[uint64]string{},
		seen:        activity{at: start, what: "sandbox agent start"},
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
		if e.TitleChangedAt != nil {
			observed.consider(*e.TitleChangedAt, "title change on exec "+e.ID)
		}
	}
	p.mu.Lock()
	if what, ok := p.oldestHoldLocked(); ok {
		observed.consider(now, "client connected: "+what)
	}
	p.seen.consider(observed.at, observed.what)
	latest := p.seen
	p.mu.Unlock()

	var leaseUntil time.Time
	for _, lease := range p.leases() {
		latest.consider(lease.at, "lease "+lease.what)
		if lease.at.After(now) && lease.at.After(leaseUntil) {
			leaseUntil = lease.at
		}
	}
	return State{
		IdleTimeout:    p.idleTimeout,
		LastActivityAt: latest.at,
		LastActivity:   latest.what,
		StopsAt:        latest.at.Add(p.idleTimeout),
		LeaseUntil:     leaseUntil,
	}
}

// Run evaluates the policy every interval and powers the sandbox off once it
// has been idle for the timeout. It returns when ctx ends or the power-off has
// been accepted; one that fails is logged and tried again on the next tick.
func (p *Policy) Run(ctx context.Context) {
	if p == nil {
		return
	}
	if err := p.prepareLeaseDir(); err != nil {
		// A sandbox with no lease directory still stops when idle; it just
		// cannot be held. Refusing to run would keep it up forever instead.
		p.logger.Warn("autostop lease directory unavailable", "dir", p.leaseDir, "error", err)
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
		now := p.now().UTC()
		state := p.Evaluate(now)
		if now.Before(state.StopsAt) {
			continue
		}
		p.logger.Info("sandbox idle, powering off",
			"idleTimeout", state.IdleTimeout,
			"lastActivity", state.LastActivity,
			"lastActivityAt", state.LastActivityAt)
		if err := p.powerOff(ctx); err != nil {
			p.logger.Error("autostop power off", "error", err)
			continue
		}
		return
	}
}

// prepareLeaseDir makes the lease directory writable by anyone in the sandbox
// and, being sticky, removable only by whoever wrote each lease — so two
// holders can neither shorten nor delete each other's (ADR 0108 §4).
func (p *Policy) prepareLeaseDir() error {
	if err := os.MkdirAll(p.leaseDir, 0o755); err != nil {
		return err
	}
	// Chmod after, because MkdirAll's mode is filtered through the umask.
	return os.Chmod(p.leaseDir, os.ModeSticky|0o777)
}

// leases reads every regular file in the lease directory. Only the mtime is
// read, from the entry itself — a symlink is not a lease and is not followed.
// A directory that does not exist holds no leases.
func (p *Policy) leases() []activity {
	entries, err := os.ReadDir(p.leaseDir)
	if err != nil {
		return nil
	}
	out := make([]activity, 0, len(entries))
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		out = append(out, activity{at: info.ModTime().UTC(), what: filepath.Join(p.leaseDir, entry.Name())})
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

// systemctlPowerOff asks systemd, which is PID 1 in the sandbox, to power it
// off, and returns as soon as the job is queued.
//
// It starts poweroff.target directly rather than running `systemctl poweroff`,
// which is the same job reached a worse way. `poweroff` asks logind first — the
// image has none, so every stop logs its failure before falling back to this —
// and then waits for the job, during which the shutdown it started kills this
// unit and the waiting systemctl with it. Every stop that worked was reported
// as one that failed. --no-block is what returns before that, and
// replace-irreversibly is the job mode `poweroff` itself uses, so nothing
// started afterwards can cancel the shutdown.
func systemctlPowerOff(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "systemctl", "start", "--no-block", "--job-mode=replace-irreversibly", "poweroff.target").CombinedOutput()
	if err != nil {
		return fmt.Errorf("start poweroff.target: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
