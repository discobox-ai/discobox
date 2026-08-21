//go:build linux

package buildkitagent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/discobox-ai/discobox/layout"
	"github.com/discobox-ai/discobox/proxy/bridge"
	"golang.org/x/sys/unix"
)

// The per-build forwarder is what binds a build step's egress to the sandbox
// that asked for the build.
//
// A build step reaches the pool proxy the same way everything else in Discobox
// does: through a plaintext forwarder that holds an mTLS client certificate.
// What differs is where the forwarder listens. It binds inside that one build
// step's own network namespace, so the address is reachable only from that
// step. Nothing has to be secret — the identifier the mediator puts in the
// build's proxy URL is inert anywhere else, because the listener it names does
// not exist in any other namespace.
//
// That is the property ADR 0020 required before a pool-shared builder could
// exist at all: "a mechanism that binds each build's egress to its owning
// sandbox's client certificate". The OCI spec still carries no tenant
// identity of its own; the mediator supplies it, and this puts it to work.

// forwarderRuntimeDir holds each build's readiness marker and pid file. It is
// under /run, which is tmpfs, so nothing here outlives the pool container.
const forwarderRuntimeDir = "/run/discobox/build-forwarders"

// StartBuildForwarder is the createRuntime hook's work. It spawns the detached
// listener for one build step and waits for it to bind.
//
// It must not return before the listener is up: runc starts the container as
// soon as the hook returns, and a build step that raced ahead of its own proxy
// would see connection refused rather than a working egress path.
func StartBuildForwarder(ctx context.Context, containerID, sandboxID string, pid int) error {
	if containerID == "" || sandboxID == "" || pid <= 0 {
		return fmt.Errorf("build forwarder needs a container, a sandbox and a pid")
	}
	if err := os.MkdirAll(resolve(forwarderRuntimeDir), 0o700); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	//nolint:gosec // Every argument is derived from the OCI state runc handed us.
	cmd := exec.CommandContext(ctx, self, "build-forwarder", "serve",
		containerID, sandboxID, strconv.Itoa(pid))
	// Setsid detaches it from the hook, which runc reaps as soon as it exits.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn build forwarder: %w", err)
	}
	ready := readyPath(containerID)
	for range 300 {
		if _, err := os.Stat(resolve(ready)); err == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("build forwarder for %s did not bind", containerID)
}

// ServeBuildForwarder is the detached half. It enters the build step's network
// namespace, binds loopback there, and forwards to the pool proxy over mTLS
// with the owning sandbox's client certificate.
func ServeBuildForwarder(ctx context.Context, containerID, sandboxID string, pid int) error {
	projectID := os.Getenv(envProjectID)
	poolID := os.Getenv(envPoolID)
	if projectID == "" || poolID == "" {
		return fmt.Errorf("build forwarder does not know its pool")
	}
	certDir := filepath.Join(layout.ProxyCerts(projectID, poolID), "clients", filepath.Clean(sandboxID))

	listener, err := listenInNetns(ctx, pid)
	if err != nil {
		return err
	}
	forwarder, err := bridge.New(ctx, bridge.Config{
		WorkerProxyURL: PoolProxyURL,
		MTLSCAPath:     resolve(filepath.Join(layout.ProxyCerts(projectID, poolID), "mtls-ca.crt")),
		ClientCertPath: resolve(filepath.Join(certDir, "client.crt")),
		ClientKeyPath:  resolve(filepath.Join(certDir, "client.key")),
	})
	if err != nil {
		_ = listener.Close()
		return fmt.Errorf("build forwarder for %s: %w", sandboxID, err)
	}
	defer forwarder.Close()

	if err := os.WriteFile(resolve(pidPath(containerID)), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return err
	}
	// Written last: the create hook waits on this, so it must mean "bound".
	if err := os.WriteFile(resolve(readyPath(containerID)), []byte("ok"), 0o600); err != nil {
		return err
	}
	return forwarder.Serve(listener)
}

// StopBuildForwarder is the poststop hook's work. poststop runs even when a
// container dies badly, which is what makes this a guaranteed teardown rather
// than a best-effort one.
func StopBuildForwarder(containerID string) error {
	if containerID == "" {
		return nil
	}
	defer func() {
		_ = os.Remove(resolve(pidPath(containerID)))
		_ = os.Remove(resolve(readyPath(containerID)))
	}()
	data, err := os.ReadFile(resolve(pidPath(containerID)))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return err
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stop build forwarder %d: %w", pid, err)
	}
	return nil
}

// listenInNetns binds the forwarder's port inside a build step's network
// namespace and returns to the caller's own namespace before handing the
// listener back.
//
// Returning is the whole point. A network namespace is a property of the
// process, not of one socket: a forwarder that stayed in the build's namespace
// could accept the build's connections but could no longer reach the pool
// proxy — every request would fail inside the forwarder, and the build would
// see a proxy answering 500 rather than a missing one. A listening socket keeps
// the namespace it was created in, and accepted connections inherit it from the
// listener, so moving back costs nothing.
//
// The thread is pinned across the whole sequence because setns acts on the
// calling thread, and Go moves goroutines between threads freely. Without the
// pin the listen could run on a thread that never entered the namespace —
// binding in the pool's namespace and silently handing every build one shared
// address — and the restore could leave some other thread behind in a build's
// namespace. The thread is left locked and unusable for anything else after a
// failed restore, which is why that case ends the process rather than
// continuing with an unknown namespace.
func listenInNetns(ctx context.Context, pid int) (net.Listener, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	own, err := os.Open(resolve("/proc/thread-self/ns/net"))
	if err != nil {
		return nil, fmt.Errorf("open own netns: %w", err)
	}
	defer own.Close()

	target, err := os.Open(resolve(fmt.Sprintf("/proc/%d/ns/net", pid)))
	if err != nil {
		return nil, fmt.Errorf("open build netns: %w", err)
	}
	defer target.Close()

	if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
		return nil, fmt.Errorf("enter build netns: %w", err)
	}
	var config net.ListenConfig
	listener, listenErr := config.Listen(ctx, "tcp", BuildProxyAddress())
	if err := unix.Setns(int(own.Fd()), unix.CLONE_NEWNET); err != nil {
		// This thread is now in a namespace that is about to disappear, and
		// there is no way to put it back. Carrying on would mean forwarding a
		// build's traffic from inside that build, which is exactly the
		// confusion this function exists to prevent.
		if listener != nil {
			_ = listener.Close()
		}
		return nil, fmt.Errorf("return from build netns: %w", err)
	}
	if listenErr != nil {
		return nil, fmt.Errorf("bind inside build netns: %w", listenErr)
	}
	return listener, nil
}

func pidPath(containerID string) string {
	return filepath.Join(forwarderRuntimeDir, filepath.Clean(containerID)+".pid")
}

func readyPath(containerID string) string {
	return filepath.Join(forwarderRuntimeDir, filepath.Clean(containerID)+".ready")
}
