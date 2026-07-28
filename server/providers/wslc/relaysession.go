package wslc

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"

	"github.com/obot-platform/discobox/layout"
	"github.com/obot-platform/discobox/pool-agent/cpmux"
	"github.com/obot-platform/discobox/server/providers/dockerworker"
	"github.com/obot-platform/discobox/server/providers/wslc/internal/wslcsession"
	"github.com/obot-platform/discobox/server/providers/wslc/relay"
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

// startRelay mounts the embedded relay into the guest, runs it, and brings up
// the multiplexed control-plane session over its stdio.
func startRelay(ctx context.Context, vm *wslcsession.Session, poolID, stagingDir string, sink StreamSink) (*relaySession, error) {
	if _, err := relay.Extract(stagingDir); err != nil {
		return nil, err
	}
	if err := vm.MountFolder(stagingDir, relay.GuestPath, true); err != nil {
		return nil, fmt.Errorf("mount guest relay: %w", err)
	}

	executable := relay.GuestPath + "/" + relay.BinaryName
	// The mount is read-only and arrives over 9p, which carries no POSIX
	// permission bits, so the binary is executed through the loader rather than
	// relying on its executable bit surviving the crossing.
	conn, err := vm.StartProcess("/bin/sh", []string{
		"/bin/sh", "-c",
		fmt.Sprintf("install -m 0755 %s /tmp/%s && exec /tmp/%s --socket %s",
			executable, relay.BinaryName, relay.BinaryName, GuestSocketPath),
	})
	if err != nil {
		return nil, err
	}

	session, err := cpmux.Client(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	if err := prepareGuestDirs(vm); err != nil {
		_ = session.Close()
		_ = conn.Close()
		return nil, err
	}

	r := &relaySession{session: session, conn: conn, stopped: make(chan struct{})}
	go r.serveControlPlane(ctx, poolID, sink)
	slog.InfoContext(ctx, "started pool control-plane relay",
		"pool_id", poolID, "relay_digest", relay.Digest())
	return r, nil
}

// prepareGuestDirs creates the directories the engine bind-mounts into the
// pool-agent container. A wslc guest boots from a stock Microsoft image that has
// none of them, and Docker refuses a bind whose source is missing rather than
// creating it, so the container create would fail with a path error that names
// no cause.
func prepareGuestDirs(vm *wslcsession.Session) error {
	hostState := layout.NewHostMapping(GuestStateRoot)
	dirs := []string{GuestSocketDir}
	for _, tree := range dockerworker.RequiredHostDirs() {
		dirs = append(dirs, hostState.HostPath(tree))
	}
	command := "mkdir -p " + strings.Join(dirs, " ")
	conn, err := vm.StartProcess("/bin/sh", []string{"/bin/sh", "-c", command})
	if err != nil {
		return fmt.Errorf("prepare guest directories: %w", err)
	}
	defer func() { _ = conn.Close() }()
	// Reading to EOF waits for the command to finish, so a later bind mount
	// cannot race directory creation.
	if _, err := io.Copy(io.Discard, conn); err != nil {
		return fmt.Errorf("prepare guest directories: %w", err)
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
