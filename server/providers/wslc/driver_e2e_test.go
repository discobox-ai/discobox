package wslc

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/moby/moby/client"

	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/dockerworker"
)

// TestDriverE2E boots a real wslc VM and exercises the VM lifecycle plus the
// Docker client lease against the guest's real dockerd. It is skipped unless
// DISCOBOX_WSLC_E2E=1 and only runs on a wslc-capable Windows machine.
//
//	$env:DISCOBOX_WSLC_E2E="1"; go test -run TestDriverE2E -v ./providers/wslc/
func TestDriverE2E(t *testing.T) {
	if os.Getenv("DISCOBOX_WSLC_E2E") != "1" {
		t.Skip("set DISCOBOX_WSLC_E2E=1 to run the real wslc VM e2e test")
	}

	storage := t.TempDir()
	driver, err := NewDriver(DriverConfig{
		StorageDir:    filepath.Join(storage, "vms"),
		CPUCount:      2,
		MemoryMiB:     2048,
		MaxStorageMiB: 16384,
		AgentPort:     defaultAgentPort,
	})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })

	const poolID = "e2e-pool"
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	info, err := driver.EnsureVM(ctx, poolID, dockerworker.VMSpec{})
	if err != nil {
		t.Fatalf("EnsureVM: %v", err)
	}
	if info.Status != sandbox.StatusRunning {
		t.Fatalf("EnsureVM status = %v, want running", info.Status)
	}

	// EnsureVM is idempotent: a second call reuses the same session.
	if _, err := driver.EnsureVM(ctx, poolID, dockerworker.VMSpec{}); err != nil {
		t.Fatalf("second EnsureVM: %v", err)
	}

	if info, err := driver.InspectVM(ctx, poolID); err != nil || info.Status != sandbox.StatusRunning {
		t.Fatalf("InspectVM = (%v, %v), want running/nil", info, err)
	}

	lease, err := driver.AcquireDockerClient(ctx, poolID)
	if err != nil {
		t.Fatalf("AcquireDockerClient: %v", err)
	}
	defer lease.Release()

	ping, err := lease.Client.Ping(ctx, client.PingOptions{})
	if err != nil {
		t.Fatalf("docker Ping over wslc bridge: %v", err)
	}
	t.Logf("dockerd reachable over wslc: APIVersion=%s OSType=%s", ping.APIVersion, ping.OSType)

	if err := driver.StopVM(ctx, poolID); err != nil {
		t.Fatalf("StopVM: %v", err)
	}
	if _, err := driver.InspectVM(ctx, poolID); err == nil {
		t.Fatalf("InspectVM after StopVM should report not found")
	}
}

// TestRelayMuxCarriesGuestTrafficE2E proves the multiplexed control-plane
// session is a working data path over the real wslc transport, not merely an
// established handshake: it dials the guest's Docker socket *through the relay*
// and speaks HTTP over the resulting stream.
//
// The pool-agent API rides this exact mechanism, so a mux that connects but
// cannot carry a request would otherwise only surface once a pool tried to
// register.
func TestRelayMuxCarriesGuestTrafficE2E(t *testing.T) {
	if os.Getenv("DISCOBOX_WSLC_E2E") != "1" {
		t.Skip("set DISCOBOX_WSLC_E2E=1 to run the real wslc VM e2e test")
	}
	driver, err := NewDriver(DriverConfig{CPUCount: 2, MemoryMiB: 2048})
	if err != nil {
		t.Fatalf("NewDriver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })

	const poolID = "relay-mux-e2e"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if _, err := driver.EnsureVM(ctx, poolID, dockerworker.VMSpec{}); err != nil {
		t.Fatalf("EnsureVM: %v", err)
	}

	relay, err := driver.relay(poolID)
	if err != nil {
		t.Fatalf("relay: %v", err)
	}

	// Every request opens its own stream, so this also exercises the mux under
	// concurrent streams rather than a single happy-path connection.
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return relay.dial(ctx, "unix:"+guestDockerSocket)
			},
		},
		Timeout: 60 * time.Second,
	}
	for i := 0; i < 3; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/_ping", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d over relay mux: %v", i, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i, resp.StatusCode)
		}
	}
	t.Log("HTTP round trips succeeded over the multiplexed guest relay")

	// A closed session must be reported unhealthy so the engine replaces the VM
	// instead of leaving a pool with no control-plane path.
	relay.close()
	if info, err := driver.InspectVM(ctx, poolID); err != nil || info.Status == sandbox.StatusRunning {
		t.Fatalf("InspectVM after relay loss = (%v, %v), want a non-running status", info, err)
	}
}
