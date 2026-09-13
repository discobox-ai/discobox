package wslc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/layout"
	"github.com/discobox-ai/discobox/pool-agent/cpmux"
	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/wslc/internal/wslcsession"
	"github.com/discobox-ai/discobox/server/providers/wslc/relay"
)

// GuestSocketDir is the guest directory the relay creates its control-plane
// socket in. It is bind-mounted into the pool-agent container at the same path
// — both live inside the guest, so this is a plain same-kernel bind and the
// socket works. The agent then reaches the control plane with an ordinary
// unix:// URL and needs no wslc-specific code.
const (
	GuestSocketDir  = "/run/discobox"
	GuestSocketPath = GuestSocketDir + "/cp.sock"
)

// ControlPlaneURL is what the in-guest agent is configured with.
const ControlPlaneURL = "unix://" + GuestSocketPath

// GuestStateRoot is where the guest's Docker daemon keeps pool state.
//
// wslc persists only /var/lib/docker — everything else is on an ephemeral root
// that is discarded when the VM stops — so state lives inside that tree,
// alongside Docker's own volumes, which is the only durable location the
// backend offers. Containers still address it as layout.ContainerRoot.
const GuestStateRoot = "/var/lib/docker/discobox"

// StreamSink receives control-plane connections opened by a guest. The driver
// hands each one to the server, which serves the ordinary control-plane handler
// over it; see server/internal/transport/carrierhub.
type StreamSink interface {
	Push(conn net.Conn, cancel <-chan struct{}) error
}

// relaySession is one guest's control-plane relay: the guest process, the
// multiplexed session over its stdio, and the loop that feeds guest-opened
// streams to the control plane.
type relaySession struct {
	session *cpmux.Session
	conn    net.Conn

	stopOnce sync.Once
	stopped  chan struct{}
}

// startRelay installs the embedded relay in the guest, runs it, and brings up
// the multiplexed control-plane session over its stdio.
func startRelay(ctx context.Context, vm *wslcsession.Session, poolID string, sink StreamSink) (*relaySession, error) {
	binary, err := relay.Binary()
	if err != nil {
		return nil, err
	}
	if err := installGuestBinary(ctx, vm, relay.GuestPath, binary); err != nil {
		return nil, fmt.Errorf("install guest relay: %w", err)
	}

	conn, err := vm.StartProcess(relay.GuestPath, []string{relay.GuestPath, "--socket", GuestSocketPath})
	if err != nil {
		return nil, err
	}

	session, err := cpmux.Client(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	if err := prepareGuestDirs(ctx, vm); err != nil {
		_ = session.Close()
		_ = conn.Close()
		return nil, err
	}
	// Not fatal: nothing a pool runs needs the host's /dev/shm, only the admin
	// console does, and a pool that cannot open a console is far better than
	// no pool. The console's own error names /dev/shm, and this names why.
	if err := mountGuestSharedMemory(ctx, vm); err != nil {
		slog.WarnContext(ctx, "guest has no /dev/shm; the pool console will not start",
			"pool_id", poolID, "error", err)
	}

	r := &relaySession{session: session, conn: conn, stopped: make(chan struct{})}
	go r.serveControlPlane(ctx, poolID, sink)
	slog.InfoContext(ctx, "started pool control-plane relay",
		"pool_id", poolID, "relay_digest", relay.Digest())
	return r, nil
}

// guestProcessStarter is the one thing installGuestBinary needs from a session:
// a guest process whose stdio it can talk to. Naming it here rather than taking
// a *wslcsession.Session keeps the install testable off Windows, where no
// session can exist at all.
type guestProcessStarter interface {
	StartProcess(executable string, argv []string) (net.Conn, error)
}

// guestCommandTimeout bounds one short-lived exchange with a guest process.
//
// The exchanges it covers are a directory create and a ~2.5 MB transfer over a
// relayed socket, both of which finish in well under a second on a guest that
// is answering at all; a minute is two orders of magnitude of headroom, and
// what it is really measuring is "this guest has stopped answering".
const guestCommandTimeout = time.Minute

// runGuestCommand starts a guest process and hands its connection to exchange,
// giving up if the exchange does not finish in time.
//
// The timeout is not a nicety. A guestConn's deadlines are deliberate no-ops
// (it is a synchronous relay over a vsock-backed handle, not a socket the OS
// will time out for us), so a guest process that starts and then wedges would
// block its caller forever - and every caller here runs inside EnsureVM, which
// holds the driver mutex for its whole body. One wedged guest would take the
// whole driver with it, for every pool, with no error anywhere.
//
// Closing the connection is what unblocks a stuck Read or Write: it closes both
// socket handles and releases the process reference, so the exchange goroutine
// comes back with an error rather than being left running forever. It is closed
// on the way out regardless, which is also what ends the guest process.
//
// Only stdin and stdout cross the relay, so the script runs with its stderr
// folded into stdout for its whole body. A guest shell that stops under set -e
// writes its reason to stderr, and that reason is the only diagnosis the host
// ever gets; a redirect appended to the script would bind to its last command
// alone.
func runGuestCommand(ctx context.Context, vm guestProcessStarter, script string, exchange func(net.Conn) error) error {
	conn, err := vm.StartProcess("/bin/sh", []string{"/bin/sh", "-c", "exec 2>&1; " + script})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithTimeout(ctx, guestCommandTimeout)
	defer cancel()

	// Buffered, so the goroutine can finish and exit even after this function
	// has stopped waiting for it.
	done := make(chan error, 1)
	go func() { done <- exchange(conn) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = conn.Close()
		return ctx.Err()
	}
}

// installGuestBinary writes a program into the guest by streaming it to a shell
// reading its own stdin, and makes it executable.
//
// A guest process's stdin is the only way in: wslc's supported way to share a
// Windows folder reaches a container, not the VM's own namespace, and the
// private call that reached the namespace mounts over 9p, which this WSL's
// guest init can no longer do (docs/adr/0120). Nothing
// is lost by the change - a mounted binary had to be copied and chmod'd anyway,
// since a Windows folder carries no executable bit.
//
// The guest echoes back the digest of what it received, and this refuses to run
// anything whose bytes did not survive: a truncated relay would otherwise fail
// as a mux handshake timeout, which says nothing about why.
func installGuestBinary(ctx context.Context, vm guestProcessStarter, guestPath string, binary []byte) error {
	// Written to a temporary name and renamed, so a crash mid-write cannot
	// leave a half-written binary at a path the next call finds and runs.
	staging := guestPath + ".tmp"
	script := fmt.Sprintf("set -e; mkdir -p %s; cat > %s; chmod 0755 %s; mv %s %s; sha256sum %s | cut -d' ' -f1",
		path.Dir(guestPath), staging, staging, staging, guestPath, guestPath)

	err := runGuestCommand(ctx, vm, script, func(conn net.Conn) error {
		if _, err := conn.Write(binary); err != nil {
			return fmt.Errorf("write %s to guest: %w", guestPath, err)
		}
		// The shell's `cat` ends at EOF and nowhere else, so the write half has
		// to close for the install to finish at all.
		closer, ok := conn.(interface{ CloseWrite() error })
		if !ok {
			return fmt.Errorf("guest connection cannot half-close, so %s can never be written", guestPath)
		}
		if err := closer.CloseWrite(); err != nil {
			return fmt.Errorf("finish writing %s: %w", guestPath, err)
		}

		out, err := io.ReadAll(conn)
		if err != nil {
			return fmt.Errorf("install %s in guest: %w", guestPath, err)
		}
		want := sha256.Sum256(binary)
		got := strings.TrimSpace(string(out))
		if got != hex.EncodeToString(want[:]) {
			return fmt.Errorf("guest did not receive %s intact (%d bytes sent); it answered %q",
				guestPath, len(binary), got)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("install %s in guest: %w", guestPath, err)
	}
	return nil
}

// dialGuest opens a connection to an address inside the guest ("unix:/path",
// "tcp:host:port") by running the relay in its one-shot --dial mode, with the
// guest process's stdio as the connection.
//
// This is deliberately a process per connection rather than another stream on
// the relay's mux: it carries Docker traffic - image loads, build contexts -
// which would head-of-line block the pool agent behind it.
func dialGuest(vm *wslcsession.Session, target string) (net.Conn, error) {
	return vm.StartProcess(relay.GuestPath, []string{relay.GuestPath, "--dial", target})
}

// prepareGuestDirs creates the directories the engine bind-mounts into the
// pool-agent container. A wslc guest boots from a stock Microsoft image that has
// none of them, and Docker refuses a bind whose source is missing rather than
// creating it, so the container create would fail with a path error that names
// no cause.
func prepareGuestDirs(ctx context.Context, vm guestProcessStarter) error {
	hostState := layout.NewHostMapping(GuestStateRoot)
	dirs := []string{GuestSocketDir}
	for _, tree := range dockerworker.RequiredHostDirs() {
		dirs = append(dirs, hostState.HostPath(tree))
	}
	command := "mkdir -p " + strings.Join(dirs, " ")
	err := runGuestCommand(ctx, vm, command, func(conn net.Conn) error {
		// Reading to EOF waits for the command to finish, so a later bind mount
		// cannot race directory creation.
		_, err := io.Copy(io.Discard, conn)
		return err
	})
	if err != nil {
		return fmt.Errorf("prepare guest directories: %w", err)
	}
	return nil
}

// guestSharedMemoryScript mounts a tmpfs at /dev/shm unless something already
// has, and says so on success; see mountGuestSharedMemory.
const guestSharedMemoryScript = "set -e; mkdir -p /dev/shm; " +
	"mountpoint -q /dev/shm || mount -t tmpfs -o rw,nosuid,nodev,mode=1777 shm /dev/shm; " +
	"echo mounted"

// mountGuestSharedMemory gives the guest the /dev/shm every other Linux host
// has.
//
// The wslc guest boots from a stock Microsoft image with no /dev/shm at all -
// not unmounted, absent - and Docker refuses to start a container in the host's
// IPC namespace without one ("/dev/shm is not mounted, but must be for
// --ipc=host"). The pool console asks for exactly that, as it does on every
// backend, so the missing piece is the guest's rather than the console's to
// supply. Containers in their own IPC namespace are unaffected: Docker gives
// each of those a private /dev/shm of its own.
//
// Mounts do not survive a reboot, and neither does the VM this runs in, so it
// runs each time EnsureVM starts a VM. It checks first, so a WSL release that starts
// shipping the mount is left alone.
func mountGuestSharedMemory(ctx context.Context, vm guestProcessStarter) error {
	var out []byte
	err := runGuestCommand(ctx, vm, guestSharedMemoryScript, func(conn net.Conn) error {
		var readErr error
		out, readErr = io.ReadAll(conn)
		return readErr
	})
	if err != nil {
		return fmt.Errorf("mount /dev/shm in guest: %w", err)
	}
	// The shell's exit status does not cross the stdio relay, so success is the
	// line it prints last and anything else is the reason it stopped.
	if answer := strings.TrimSpace(string(out)); answer != "mounted" {
		return fmt.Errorf("mount /dev/shm in guest: %s", answer)
	}
	return nil
}

// serveControlPlane hands every stream the guest opens to the control plane.
// The guest can only ever mean one thing by opening a stream, so a stream that
// names anything else is dropped rather than dialed.
func (r *relaySession) serveControlPlane(ctx context.Context, poolID string, sink StreamSink) {
	for {
		conn, target, err := r.session.Accept()
		if err != nil {
			select {
			case <-r.stopped:
			default:
				slog.DebugContext(ctx, "pool control-plane relay session ended", "pool_id", poolID, "error", err)
			}
			return
		}
		if target != cpmux.TargetControlPlane {
			slog.WarnContext(ctx, "guest opened a stream to an unexpected target",
				"pool_id", poolID, "target", target)
			_ = conn.Close()
			continue
		}
		if sink == nil {
			_ = conn.Close()
			continue
		}
		if err := sink.Push(conn, r.stopped); err != nil {
			_ = conn.Close()
		}
	}
}

// dial opens a stream the guest relay connects to an address inside the guest.
func (r *relaySession) dial(ctx context.Context, target string) (net.Conn, error) {
	return r.session.Dial(ctx, target)
}

// healthy reports whether the session is still usable, so a pool whose relay
// died is repaired rather than silently unreachable.
func (r *relaySession) healthy() bool {
	return r != nil && r.session != nil && !r.session.Closed()
}

func (r *relaySession) close() {
	r.stopOnce.Do(func() {
		close(r.stopped)
		if r.session != nil {
			_ = r.session.Close()
		}
		if r.conn != nil {
			_ = r.conn.Close()
		}
	})
}
