package libkrun

import (
	"errors"
	"net"
	"path/filepath"
	"testing"

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
