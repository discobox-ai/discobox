package wslc

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/layout"
)

// A pool must never fall back to wslc's own default storage, which is RAM-backed
// tmpfs. It is far too small to build or hold pool images, and the failure it
// produces is an opaque "no space left on device" part-way through an image
// build rather than anything naming storage.
func TestStorageDirDefaultsToARealLocation(t *testing.T) {
	got := effectiveStorageDir("")
	if got == "" {
		t.Fatal("storage directory defaulted to empty, which selects ephemeral tmpfs")
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("default storage directory %q is not absolute", got)
	}
	if !strings.Contains(strings.ToLower(got), "discobox") {
		t.Fatalf("default storage directory %q should be discobox-scoped", got)
	}
}

func TestStorageDirHonorsConfiguredValue(t *testing.T) {
	const configured = `C:\pools`
	if got := effectiveStorageDir(configured); got != configured {
		t.Fatalf("effectiveStorageDir(%q) = %q, want the configured value", configured, got)
	}
	if got := effectiveStorageDir("  " + configured + "  "); got != configured {
		t.Fatalf("effectiveStorageDir did not trim surrounding whitespace: %q", got)
	}
}

// The driver is the last line of defense: a directly constructed driver must
// also get real storage rather than silently running ephemeral.
func TestDriverConfigCarriesDefaultedStorage(t *testing.T) {
	cfg := driverConfig(Config{}, nil)
	if cfg.StorageDir == "" {
		t.Fatal("driver config has no storage directory; pools would run on tmpfs")
	}
	if cfg.RelayStagingDir == "" {
		t.Fatal("driver config has no relay staging directory")
	}
}

// wslc persists only /var/lib/docker, so pool state must be placed inside that
// tree. Without this the state lands on the guest's ephemeral root and every
// sandbox is lost when the VM stops.
func TestGuestStateRootIsOnThePersistedDisk(t *testing.T) {
	if !strings.HasPrefix(GuestStateRoot, "/var/lib/docker/") {
		t.Fatalf("GuestStateRoot = %q, want it inside the persisted /var/lib/docker tree", GuestStateRoot)
	}
}

// The container's view must not move with it: only the daemon-side location
// changes, which is what lets the agent stay backend-agnostic.
func TestGuestStateRootRelocatesOnlyTheDaemonView(t *testing.T) {
	mapping := layout.NewHostMapping(GuestStateRoot)
	containerPath := layout.PoolData("prj", "pool")
	got := mapping.HostPath(containerPath)

	if got == containerPath {
		t.Fatal("pool data was not relocated onto the persisted disk")
	}
	if !strings.HasPrefix(got, GuestStateRoot+"/") {
		t.Fatalf("relocated path = %q, want it under %q", got, GuestStateRoot)
	}
	if !strings.HasPrefix(containerPath, layout.ContainerRoot+"/") {
		t.Fatalf("container path = %q, want it under the invariant root", containerPath)
	}
	// A developer's own directory is already a daemon path and must survive.
	if outside := "/home/darren/project"; mapping.HostPath(outside) != outside {
		t.Fatal("a local source bind path was rewritten")
	}
}

// The relay dials the agent at a fixed guest address, so the pool container has
// to publish the agent port at that fixed number. Docker's other option — the
// local driver's loopback-ephemeral binding — leaves nothing listening where
// the relay connects, and every control-plane request to the agent then fails
// with a bare EOF while agent-initiated traffic keeps working, so the pool
// still reports itself ready and the breakage looks like a network fault.
func TestEngineConfigPublishesTheAgentPortTheRelayDials(t *testing.T) {
	cfg := engineConfig(Config{}, nil)
	if !cfg.PublicAgentPort {
		t.Fatal("engine config does not publish the agent port at a fixed number; the relay would dial a closed port")
	}
	if cfg.AgentPort != defaultAgentPort {
		t.Fatalf("agent port = %d, want the default %d the driver dials", cfg.AgentPort, defaultAgentPort)
	}
}

// A configured port has to reach both sides, or the relay and the container
// disagree about where the agent is.
func TestEngineConfigAndDriverAgreeOnTheAgentPort(t *testing.T) {
	const port = 4310
	engine := engineConfig(Config{AgentPort: port}, nil)
	driver := driverConfig(Config{AgentPort: port}, nil)
	if engine.AgentPort != port || driver.AgentPort != port {
		t.Fatalf("agent port: engine %d, driver %d, want %d on both", engine.AgentPort, driver.AgentPort, port)
	}
}

// The control plane must stay off TCP: a host listener is what triggers the
// Windows firewall prompt this provider exists to avoid.
func TestEngineConfigKeepsTheControlPlaneOffTCP(t *testing.T) {
	cfg := engineConfig(Config{}, nil)
	if !strings.HasPrefix(cfg.ControlPlaneURL, "unix://") {
		t.Fatalf("control plane URL = %q, want a unix socket so no host TCP port is opened", cfg.ControlPlaneURL)
	}
}
