//go:build windows

package wslcsession

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"sync"
)

// Session owns a dedicated wslc VM. It is NOT persistent: the underlying VM
// is torn down when the last reference to it is released, which happens
// automatically when this process exits or crashes (COM releases all of a
// dead process's outstanding references), or explicitly via Close.
//
// All COM calls for a Session run on one dedicated, runtime.LockOSThread'd
// goroutine (started in NewSession, serialized via reqCh). This isn't
// optional: COM apartment state is per-OS-thread, but Go goroutines are free
// to migrate between OS threads whenever they block or get preempted. Without
// pinning every call for a given COM object to the same OS thread, a call
// made shortly after NewSession returns can land on a different thread than
// the one that initialized COM, and fail with CO_E_NOTINITIALIZED
// (0x800401F0) - which is exactly what happened during development here
// before this was added.
//
// Session itself never touches raw COM pointers or vtable slots - all of
// that lives behind the sessionManager/wslcSession/wslcProcess interfaces in
// comapi.go. This type is just the public API and lifecycle/threading glue
// on top of them.
type Session struct {
	reqCh  chan func()
	closed chan struct{}

	// closeMu/stopped guard against a real race: something outside this
	// package's control (e.g. net/http's Transport closing an idle
	// connection from its own background goroutine) can call into a method
	// that calls do() at the same moment Close() runs. Without this guard,
	// do() sending on reqCh right as Close() closes it panics with "send on
	// closed channel" - reproduced during development via exactly that
	// net/http idle-connection-cleanup path. Close() takes the write lock
	// before closing reqCh, so it cannot proceed while any do() call is
	// mid-send, and any do() call arriving after stopped=true is set simply
	// no-ops instead of racing the close.
	closeMu   sync.RWMutex
	stopped   bool
	closeOnce sync.Once

	// procMu/procs track the guest-process references this session has handed
	// out but not yet taken back. Teardown is the reason they are tracked at
	// all: a conn that is never closed, or closed after Close, would otherwise
	// hold an IWSLCProcess reference forever, and the VM lives until its last
	// reference is released. See the drain in comThread.
	procMu sync.Mutex
	procs  map[wslcProcess]struct{}

	mgr  sessionManager
	sess wslcSession

	bridgeDir  string
	bridgeDone bool
}

// ErrSessionExists reports that a VM of the requested name is already running.
//
// wslc keys sessions by display name and refuses a duplicate. Because a session
// belongs to the process that created it and dies with it, this means an
// earlier process is still alive or has not finished exiting -- not that
// anything is misconfigured. Callers can match it to retry rather than treat
// the pool as failed.
var ErrSessionExists = errors.New("wslcsession: session already exists")

// createSessionError explains a CreateSession failure.
//
// A bare HRESULT says nothing, and the one callers actually hit is not a
// misconfiguration: 0x800700B7 means a VM of this name is already running,
// left by a process that exited without closing it. There is no API to
// enumerate or reattach to that session -- the session manager exposes only
// CreateSession -- and the VM is a Host Compute Service one, so it does not
// appear in `wsl --list` either. hcsdiag is what can see and end it, from an
// elevated prompt. Every other failure is returned unchanged, so a real fault
// is not mistaken for a collision.
func createSessionError(err error, displayName string) error {
	if !isHRESULT(err, hrErrorAlreadyExists) {
		return err
	}
	return fmt.Errorf("%w: a VM named %q is already running, left by a process that did not shut "+
		"down cleanly; end it with `hcsdiag kill <id>` using the id `hcsdiag list` reports for "+
		"that name, both from an elevated prompt: %w", ErrSessionExists, displayName, err)
}

// activateManager is how comThread reaches the session manager, indirected so a
// test can stand in a fake for it -- the real one reaches a Windows service and
// creates an actual VM, which no test can assert the lifetime of.
var activateManager = activateSessionManager

// NewSession creates a brand-new dedicated wslc VM. This can take a while
// the first time (VM boot).
func NewSession(opts Options) (*Session, error) {
	if len(opts.Volumes) > 0 && opts.StoragePath == "" {
		return nil, fmt.Errorf("wslcsession: Options.Volumes requires Options.StoragePath to be set " +
			"(VHD-backed volumes live under <StoragePath>/volumes/)")
	}

	s := &Session{
		reqCh:  make(chan func()),
		closed: make(chan struct{}),
	}

	initErr := make(chan error, 1)
	go s.comThread(opts, initErr)

	if err := <-initErr; err != nil {
		return nil, err
	}

	runtime.SetFinalizer(s, (*Session).Close)
	return s, nil
}

// comThread owns this session's COM apartment for its entire lifetime: one
// OS thread, CoInitialize'd once, processing every COM call for this session
// off reqCh until Close() closes it, at which point it terminates the VM and
// releases both COM references before exiting.
func (s *Session) comThread(opts Options, initErr chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	fail := func(err error) {
		initErr <- err
		close(s.closed)
	}

	if err := initCOM(); err != nil {
		fail(fmt.Errorf("wslcsession: COM init: %w", err))
		return
	}

	mgr, err := activateManager()
	if err != nil {
		fail(err)
		return
	}

	sess, err := mgr.CreateSession(opts)
	if err != nil {
		mgr.Release()
		fail(createSessionError(err, opts.DisplayName))
		return
	}

	for _, v := range opts.Volumes {
		if err := sess.CreateVolume(v); err != nil {
			// All-or-nothing: don't hand back a session with only some of
			// the requested volumes created.
			_ = sess.Terminate()
			sess.Release()
			mgr.Release()
			fail(fmt.Errorf("wslcsession: create volume %q: %w", v.Name, err))
			return
		}
	}

	s.mgr = mgr
	s.sess = sess
	initErr <- nil

	for req := range s.reqCh {
		req()
	}

	// Release whatever guest-process references are still outstanding, before
	// the session itself goes.
	//
	// This is the last moment they can be released. Close has already set
	// stopped, so do() no-ops from here on and a guestConn closing later --
	// net/http retiring an idle connection on its own schedule is the case
	// that happens -- can no longer reach this thread to release anything. A
	// reference left here is leaked for the life of the process, and because
	// the VM is torn down only when its last reference goes (see the type
	// doc), the VM outlives the Close that was supposed to end it. Every
	// later CreateSession for the same display name then fails with
	// ErrSessionExists, naming a process that did not shut down cleanly --
	// which is this one, still running.
	for _, process := range s.takeProcesses() {
		process.Release()
	}
	_ = s.sess.Terminate() // best-effort
	s.sess.Release()
	s.mgr.Release()
	close(s.closed)
}

// do runs f on the session's COM thread and waits for it to finish. Safe to
// call after Close (f simply never runs).
func (s *Session) do(f func()) {
	s.closeMu.RLock()
	defer s.closeMu.RUnlock()
	if s.stopped {
		return
	}
	done := make(chan struct{})
	s.reqCh <- func() { defer close(done); f() }
	<-done
}

// Close terminates the VM and releases COM references. Safe to call
// multiple times; blocks until teardown completes.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.closeMu.Lock()
		s.stopped = true
		close(s.reqCh)
		s.closeMu.Unlock()

		<-s.closed
		runtime.SetFinalizer(s, nil)
	})
	return nil
}

// DisplayName returns the session's display name.
func (s *Session) DisplayName() (string, error) {
	var name string
	var callErr error
	s.do(func() {
		name, callErr = s.sess.GetDisplayName()
	})
	return name, callErr
}

// DockerConn returns a connection to dockerd's Docker Engine API socket
// inside the guest, relayed entirely over the guest's private vsock control
// channel - no TCP port, public or otherwise, is ever created.
func (s *Session) DockerConn() (net.Conn, error) {
	return s.DialGuestUnix("/var/run/docker.sock")
}

// DialGuestUnix connects to a unix domain socket at the given path inside
// the guest's root namespace.
func (s *Session) DialGuestUnix(path string) (net.Conn, error) {
	return s.dial("unix:" + path)
}

// DialGuestTCP connects to a TCP address as seen from inside the guest's own
// network namespace (e.g. a container-published port bound to
// 127.0.0.1:<port> inside the VM). This is the general-purpose counterpart
// to DockerConn - same mechanism, arbitrary target.
func (s *Session) DialGuestTCP(addr string) (net.Conn, error) {
	return s.dial("tcp:" + addr)
}

func (s *Session) dial(target string) (net.Conn, error) {
	bridgeDir, err := s.ensureBridgeMounted()
	if err != nil {
		return nil, err
	}

	// Exec via /bin/sh explicitly rather than relying on entrypoint.sh's own
	// executable bit: it arrives in the guest through a 9p-mounted Windows
	// folder (MountWindowsFolder), and Windows has no POSIX-mode concept to
	// preserve in the first place - not worth depending on whatever
	// permission bits the 9p layer happens to expose.
	script := bridgeDir + "/entrypoint.sh"

	var process wslcProcess
	var callErr error
	s.do(func() {
		// wslcsession.exe backs this with the exact same
		// Fork(WSLC_FORK::Process) primitive its own DockerHTTPClient uses
		// for its private docker.sock relay (WSLCVirtualMachine.cpp) - this
		// is the supported front door onto that same fork machinery.
		process, callErr = s.sess.CreateRootNamespaceProcess("/bin/sh", []string{"/bin/sh", script, target}, true)
	})
	if callErr != nil {
		return nil, fmt.Errorf("wslcsession: spawn bridge for %q: %w", target, callErr)
	}

	var stdin, stdout socketHandle
	var hErr error
	s.do(func() {
		stdin, hErr = process.GetStdHandle(fdStdin)
		if hErr != nil {
			return
		}
		stdout, hErr = process.GetStdHandle(fdStdout)
	})
	if hErr != nil {
		s.releaseProcess(process)
		return nil, hErr
	}

	return newGuestConn(s, process, stdin, stdout), nil
}

// MountFolder mounts a Windows directory into the guest. It is how a caller
// delivers its own guest-side program: the guest has no toolchain, so a binary
// cross-compiled on the host is mounted in and executed. Mounting the same
// pair twice is not an error.
func (s *Session) MountFolder(windowsPath, guestPath string, readOnly bool) error {
	var callErr error
	s.do(func() {
		err := s.sess.MountWindowsFolder(windowsPath, guestPath, readOnly)
		if err != nil && !isHRESULT(err, hrErrorAlreadyExists) {
			callErr = fmt.Errorf("wslcsession: MountWindowsFolder: %w", err)
		}
	})
	return callErr
}

// StartProcess runs a program in the guest's root namespace and returns its
// stdin and stdout as one net.Conn.
//
// Unlike DialGuestUnix and DialGuestTCP, which spawn a short-lived bridge per
// connection, this is for a long-lived guest process whose stdio *is* the
// transport — a multiplexer, for instance, which turns that single pipe into
// many logical connections in both directions.
//
// The returned connection owns the process: closing it releases the guest
// process reference, and the process ends when the session does.
func (s *Session) StartProcess(executable string, argv []string) (net.Conn, error) {
	var process wslcProcess
	var callErr error
	s.do(func() {
		process, callErr = s.sess.CreateRootNamespaceProcess(executable, argv, true)
		if callErr == nil {
			// Tracked here rather than after do() returns, so a reference that
			// exists is always one teardown knows about.
			s.trackProcess(process)
		}
	})
	if callErr != nil {
		return nil, fmt.Errorf("wslcsession: start guest process %q: %w", executable, callErr)
	}

	var stdin, stdout socketHandle
	var hErr error
	s.do(func() {
		stdin, hErr = process.GetStdHandle(fdStdin)
		if hErr != nil {
			return
		}
		stdout, hErr = process.GetStdHandle(fdStdout)
	})
	if hErr != nil {
		s.releaseProcess(process)
		return nil, hErr
	}
	return newGuestConn(s, process, stdin, stdout), nil
}

// releaseProcess is used by guestConn.Close to release the IWSLCProcess
// reference on the session's COM thread.
//
// The claim and the release happen together inside the COM thread's own call,
// so this and teardown cannot both release the same reference, and a call that
// arrives too late to run -- do() no-ops once Close has stopped the session --
// claims nothing and leaves the reference for the drain in comThread. Either
// one releases it exactly once; neither can drop it.
func (s *Session) releaseProcess(process wslcProcess) {
	s.do(func() {
		if s.claimProcess(process) {
			process.Release()
		}
	})
}

// trackProcess records a reference handed out to a caller.
func (s *Session) trackProcess(process wslcProcess) {
	s.procMu.Lock()
	defer s.procMu.Unlock()
	if s.procs == nil {
		s.procs = make(map[wslcProcess]struct{})
	}
	s.procs[process] = struct{}{}
}

// claimProcess takes ownership of releasing process, reporting whether this
// caller is the one that got it. A reference already claimed is not released
// twice.
func (s *Session) claimProcess(process wslcProcess) bool {
	s.procMu.Lock()
	defer s.procMu.Unlock()
	if _, ok := s.procs[process]; !ok {
		return false
	}
	delete(s.procs, process)
	return true
}

// takeProcesses claims every reference still outstanding, for teardown.
func (s *Session) takeProcesses() []wslcProcess {
	s.procMu.Lock()
	defer s.procMu.Unlock()
	out := make([]wslcProcess, 0, len(s.procs))
	for process := range s.procs {
		out = append(out, process)
	}
	s.procs = nil
	return out
}

func (s *Session) ensureBridgeMounted() (string, error) {
	var dir string
	var callErr error
	s.do(func() {
		if s.bridgeDone {
			dir = s.bridgeDir
			return
		}

		extractedDir, err := extractBridgeBinary()
		if err != nil {
			callErr = err
			return
		}

		err = s.sess.MountWindowsFolder(extractedDir, guestBridgeMountPath, true)
		if err != nil && !isHRESULT(err, hrErrorAlreadyExists) {
			callErr = fmt.Errorf("wslcsession: MountWindowsFolder: %w", err)
			return
		}

		s.bridgeDir = guestBridgeMountPath
		s.bridgeDone = true
		dir = s.bridgeDir
	})
	return dir, callErr
}
