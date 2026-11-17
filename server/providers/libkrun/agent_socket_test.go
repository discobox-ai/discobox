package libkrun

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/x/shorttmp"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// A pool VM that is running but has not bound its agent's socket yet is
// booting, as every pool's VM is right after a server start: a start that
// arrives then must wait for it, not fail with a 500 (#893). Once the socket
// is bound the lease reaches it.
func TestAgentClientWaitsForABootingVMsSocket(t *testing.T) {
	dir := shorttmp.Dir(t)
	guest := &guestVM{runtimeDir: dir, exited: make(chan struct{})}
	driver := &Driver{vms: map[string]*guestVM{"pool_1": guest}}

	_, err := driver.AcquirePoolAgentClient(t.Context(), "pool_1")
	if !errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Fatalf("AcquirePoolAgentClient before the socket is bound = %v, want ErrPoolNotReachable", err)
	}

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", filepath.Join(dir, agentSocketName))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	lease, err := driver.AcquirePoolAgentClient(t.Context(), "pool_1")
	if err != nil {
		t.Fatalf("AcquirePoolAgentClient once the socket is bound: %v", err)
	}
	lease.Release()
}

// The Docker socket keeps its plain error: what a daemon that never comes up
// means is the engine's readiness wait's to say, and a pool reconcile reading
// it as a host coming up would never repair the VM.
func TestDockerClientDoesNotWaitForAnUnboundSocket(t *testing.T) {
	guest := &guestVM{runtimeDir: shorttmp.Dir(t), exited: make(chan struct{})}
	driver := &Driver{vms: map[string]*guestVM{"pool_1": guest}}
	_, err := driver.AcquireDockerClient(t.Context(), "pool_1")
	if err == nil || errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Fatalf("AcquireDockerClient before the socket is bound = %v, want a plain error", err)
	}
}

// Every connection to the agent's socket is a new VSOCK connection into the
// guest, so the server keeps its connections alive across leases: sequential
// requests, each on its own lease as every pool-agent call is, reuse one
// connection rather than dialing the guest for each (#1335).
func TestAgentClientReusesConnectionsAcrossLeases(t *testing.T) {
	dir := shorttmp.Dir(t)
	guest := &guestVM{runtimeDir: dir, exited: make(chan struct{})}
	driver := &Driver{vms: map[string]*guestVM{"pool_1": guest}}

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", filepath.Join(dir, agentSocketName))
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }),
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				accepted.Add(1)
			}
		},
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	const requests = 10
	for range requests {
		lease, err := driver.AcquirePoolAgentClient(t.Context(), "pool_1")
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, lease.BaseURL+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := lease.Client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		lease.Release()
	}
	if got := accepted.Load(); got != 1 {
		t.Fatalf("%d sequential pool-agent requests opened %d connections to the guest, want 1", requests, got)
	}
}

// The keep-alive client belongs to the VM: closing the VM closes the
// connections it kept, and the VM that replaces it dials with a client of its
// own rather than one holding connections to a guest that is gone (#1335).
func TestAgentClientClosesWithItsVM(t *testing.T) {
	dir := shorttmp.Dir(t)
	_, watchdog, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	guest := &guestVM{runtimeDir: dir, cmd: &exec.Cmd{}, watchdog: watchdog, exited: make(chan struct{})}
	driver := &Driver{vms: map[string]*guestVM{"pool_1": guest}}

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", filepath.Join(dir, agentSocketName))
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{}, 1)
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }),
		ConnState: func(_ net.Conn, state http.ConnState) {
			if state == http.StateClosed {
				closed <- struct{}{}
			}
		},
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	lease, err := driver.AcquirePoolAgentClient(t.Context(), "pool_1")
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, lease.BaseURL+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := lease.Client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	lease.Release()

	guest.close()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the VM left its kept-alive connection to the agent open")
	}

	// A lease that races the close, before the launcher is seen to exit, gets
	// no client rather than one nothing will close.
	if _, err := driver.AcquirePoolAgentClient(t.Context(), "pool_1"); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("AcquirePoolAgentClient on a closed VM = %v, want ErrNotFound", err)
	}

	replacement := &guestVM{runtimeDir: dir, exited: make(chan struct{})}
	driver.vms["pool_1"] = replacement
	next, err := driver.AcquirePoolAgentClient(t.Context(), "pool_1")
	if err != nil {
		t.Fatal(err)
	}
	if next.Client == lease.Client {
		t.Fatal("the replacement VM leased the stopped VM's client")
	}
}
