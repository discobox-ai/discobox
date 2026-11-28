// Package handover is how a pool's agent and its proxy give way to new
// processes beside them without cutting what they serve (ADR 26-11-26-536
// §§3–4).
//
// systemd holds every listening socket (the pool bundle's socket units), and
// passes the same sockets to each process that serves them (Listeners). Of
// the processes holding them, exactly one is active (Active): it accepts, and
// runs everything that must run once per pool. A new process starts as a
// standby, and becomes active once the old one has stopped accepting and let
// go of the active lock. The old one drains (DrainRequested): it finishes what
// it holds, up to DrainBound, and exits.
//
// Nothing here decides whether a pool hands over or restarts. That is the
// pool's units' (poolunits), which start the idle slot beside the running one
// and signal the old one to drain, or restart the running one as before.
//
// Its locks, the active lock and a sandbox's power lock and boot mark
// (sandboxruntime) among them, exclude processes of a pool from each other,
// and are held within one too. A lock is an open file description lock
// (fcntl(2) F_OFD_SETLK) on a file of the pool's own: each open of the file
// is its own holder, in one process or several. A lock is asked about
// without taking it (Held), so probes never see each other.
package handover

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// DrainBound is how long a draining process carries what it holds before it
// cuts what is left and exits (ADR 26-11-26-536 §3). It is fixed, not a
// provider's setting.
const DrainBound = 30 * time.Minute

// ErrDrained is what a process that drained returns, which its main exits
// with DrainedExitCode for: the one exit its unit does not restart
// (RestartPreventExitStatus=), since the process that took over serves. Any
// other exit, a clean one included, is restarted.
var ErrDrained = errors.New("drained: the process that took over serves")

// DrainedExitCode is the exit status of a process that drained, which the
// slots' units name (RestartPreventExitStatus=, SuccessExitStatus=).
const DrainedExitCode = 75

// The STATUS= a process tells systemd it is in, which poolunits reads to know
// a new process has taken over (systemctl show -P StatusText).
const (
	StatusStandby  = "standby"
	StatusActive   = "active"
	StatusDraining = "draining"
)

// The active locks under /run/discobox: one for the agent, one for the proxy.
// Each is held by the one process of its kind that accepts.
const (
	AgentLock = "/run/discobox/handover/pool-agent.active"
	ProxyLock = "/run/discobox/handover/pool-proxy.active"
)

// The draining marks: a process that drains holds its kind's (Mark) from
// before it lets go of the active lock until it exits. The process that took
// over starts nothing that sweeps the pool's shared state until it is free
// (AwaitFree): a sweep judges what to remove by what its own process is doing,
// and would remove what the draining one still is.
const (
	AgentDraining = "/run/discobox/handover/pool-agent.draining"
	ProxyDraining = "/run/discobox/handover/pool-proxy.draining"
)

// lockPoll is how often a wait for a lock held elsewhere looks again. A
// handover waits on it once, for the moment the old process lets go; a
// power operation waits on it for as long as the operation before it runs.
const lockPoll = 50 * time.Millisecond

// Lock is one held file lock: an open file description lock (fcntl(2)
// F_OFD_SETLK), so locks of one file exclude each other between processes and
// within one, since each is its own open file description.
type Lock struct {
	file *os.File
}

// Acquire waits, until ctx ends, to hold the lock at path alone, making the
// file and its directory as root's alone if they are not there yet.
func Acquire(ctx context.Context, path string) (*Lock, error) {
	return acquire(ctx, path, true)
}

// TryAcquire holds the lock at path alone if nobody holds it now, and
// reports whether it does.
func TryAcquire(path string) (*Lock, bool, error) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lock, err := acquire(ctx, path, true)
	if errors.Is(err, context.Canceled) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return lock, true, nil
}

// Mark holds the lock at path shared with any other marks of it, for as long
// as some work runs: Held sees it from any process, and AwaitFree waits it
// out. It waits only while the file is locked alone (Acquire), so a file a
// mark is taken on must be one nothing locks alone, or the mark's taker waits
// on it as long as ctx lets it.
func Mark(ctx context.Context, path string) (*Lock, error) {
	return acquire(ctx, path, false)
}

// acquire waits to hold the lock at path. A lock is held on the file the
// path names once it is held: one whose file was removed meanwhile (Remove)
// is let go of, and the file the path names now is locked instead.
func acquire(ctx context.Context, path string, exclusive bool) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	ticker := time.NewTicker(lockPoll)
	defer ticker.Stop()
	for {
		file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: a lock path of the pool's own
		if err != nil && !openPending(err) {
			return nil, err
		}
		if err != nil {
			// Waited out within ctx; past it, the open's own error, so a
			// try (TryAcquire) fails with it rather than reading it as held.
			select {
			case <-ctx.Done():
				return nil, err
			case <-ticker.C:
			}
			continue
		}
		held, err := tryLock(file, exclusive)
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		if held && named(path, file) {
			settle(file)
			return &Lock{file: file}, nil
		}
		if held {
			unlock(file)
			_ = file.Close()
			continue
		}
		_ = file.Close()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// named reports whether path still names file.
func named(path string, file *os.File) bool {
	byPath, err := os.Stat(path)
	if err != nil {
		return false
	}
	byFile, err := file.Stat()
	return err == nil && os.SameFile(byPath, byFile)
}

// Remove removes the lock file at path, which the caller holds (Acquire):
// a waiter that opened it before finds it gone once it holds it, and locks
// the file the path names then.
func Remove(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Release lets go of the lock.
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	unlock(l.file)
	err := l.file.Close()
	l.file = nil
	if errors.Is(err, os.ErrClosed) {
		// settle closed it already.
		return nil
	}
	return err
}

// Held reports whether any process holds the lock at path now, alone or as a
// mark. It asks who holds it without taking it, so probes never see each
// other, nor delay a mark. A file that is not there is a lock nobody has
// held.
func Held(path string) (bool, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // G304: a lock path of the pool's own
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Close() }()
	return lockHeld(file)
}

// AwaitFree waits, until ctx ends, for nobody to hold the lock at path.
func AwaitFree(ctx context.Context, path string) error {
	ticker := time.NewTicker(lockPoll)
	defer ticker.Stop()
	for {
		held, err := Held(path)
		if err != nil || !held {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// drainMarkWait bounds how long a drain waits to take its mark.
const drainMarkWait = 5 * time.Second

// Drain is a drain this process was asked for and holds the draining mark of
// (DrainRequested).
type Drain struct {
	requested chan struct{}
	mark      *Lock
}

// Requested is closed once the drain is asked for and marked.
func (d *Drain) Requested() <-chan struct{} { return d.requested }

// Release lets go of the draining mark, which the process otherwise holds
// until it exits.
func (d *Drain) Release() {
	select {
	case <-d.requested:
		_ = d.mark.Release()
	default:
	}
}
