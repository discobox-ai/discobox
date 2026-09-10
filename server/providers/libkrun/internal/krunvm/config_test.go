package krunvm

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func validConfig() Config {
	//nolint:gosec // G101 matches the "passt" in PasstSocket as "pass"; it is a Unix socket path.
	return Config{
		Version:     ConfigVersion,
		PoolID:      "pool_123",
		RuntimeDir:  "/run/user/1000/discobox/libkrun/pool_123",
		KernelImage: "/var/lib/discobox/libkrun/.images/kernel/vmlinux",
		RootDisk:    "/var/lib/discobox/libkrun/.images/guest/root.ext4",
		DataDisk:    "/var/lib/discobox/libkrun/pool_123/data.raw",
		CacheDisk:   "/var/lib/discobox/libkrun/pool_123/cache.raw",
		PasstSocket: "/run/user/1000/discobox/libkrun/pool_123/passt.sock",
		ConsoleLog:  "/run/user/1000/discobox/libkrun/pool_123/console.log",
		VCPUs:       2,
		MemoryMiB:   4096,
		MACAddress:  "02:00:00:00:00:01",
		VSOCK: []VSOCKMapping{
			{Name: "control-plane", Port: 3001, Socket: "/run/user/1000/discobox/server.sock", Direction: GuestConnects},
			{Name: "pool-agent", Port: 3002, Socket: "/run/user/1000/discobox/libkrun/pool_123/pool-agent.sock", Direction: HostConnects},
		},
	}
}

func TestValidManifestIsAccepted(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// The manifest crosses a process boundary as JSON, so the wire names are part
// of the contract rather than an implementation detail of the struct.
func TestManifestRoundTripsThroughJSON(t *testing.T) {
	data, err := json.Marshal(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"memoryMiB"`) {
		t.Fatalf("manifest = %s, want a memoryMiB key", data)
	}
	var decoded Config
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("a manifest this package wrote is one it rejects: %v", err)
	}
}

// A stale manifest left in a runtime directory by an older server must fail
// rather than be read with the wrong meaning.
func TestManifestVersionIsChecked(t *testing.T) {
	cfg := validConfig()
	cfg.Version = ConfigVersion - 1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("validate = %v, want a version rejection", err)
	}
}

func TestDuplicateVSOCKPortsAreRejected(t *testing.T) {
	cfg := validConfig()
	cfg.VSOCK[1].Port = cfg.VSOCK[0].Port
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate vsock port") {
		t.Fatalf("validate = %v, want a duplicate port rejection", err)
	}
}

// libkrun creates the host-listening sockets, and they are how anything reaches
// this pool's Docker and pool agent. Putting one outside the pool's private
// runtime directory would publish it to every other process on the machine.
func TestHostListenerMustBePrivateToTheRuntimeDirectory(t *testing.T) {
	cfg := validConfig()
	cfg.VSOCK[1].Socket = "/tmp/pool-agent.sock"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "beneath runtimeDir") {
		t.Fatalf("validate = %v, want a runtime directory requirement", err)
	}
}

// The guest's outbound control-plane port terminates at the server's own
// listening socket, which is deliberately not the pool's.
func TestGuestInitiatedSocketMayLiveOutsideTheRuntimeDirectory(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestUnknownVSOCKDirectionIsRejected(t *testing.T) {
	cfg := validConfig()
	cfg.VSOCK[0].Direction = "either"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "direction") {
		t.Fatalf("validate = %v, want a direction rejection", err)
	}
}

func TestPrivilegedVSOCKPortsAreRejected(t *testing.T) {
	cfg := validConfig()
	cfg.VSOCK[0].Port = 22
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "privileged") {
		t.Fatalf("validate = %v, want a privileged port rejection", err)
	}
}

func TestMACMustBeLocalUnicast(t *testing.T) {
	cfg := validConfig()
	cfg.MACAddress = "01:00:00:00:00:01"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "unicast") {
		t.Fatalf("validate = %v, want a unicast rejection", err)
	}
	cfg.MACAddress = "00:00:00:00:00:01"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "locally administered") {
		t.Fatalf("validate = %v, want a locally administered rejection", err)
	}
}

func TestRelativeAndUncleanPathsAreRejected(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"relative": func(c *Config) { c.RootDisk = "root.ext4" },
		"unclean":  func(c *Config) { c.KernelImage = "/images/../images/vmlinux" },
	} {
		cfg := validConfig()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("%s path accepted", name)
		}
	}
}

func TestSizingBoundsAreEnforced(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"no vcpus":    func(c *Config) { c.VCPUs = 0 },
		"too many":    func(c *Config) { c.VCPUs = 256 },
		"tiny memory": func(c *Config) { c.MemoryMiB = 128 },
	} {
		cfg := validConfig()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// A pool VM is sized from the machine it runs on, so the defaults have to be
// values a VM can actually boot with.
func TestDefaultHostResourcesAreBootable(t *testing.T) {
	resources := DefaultHostResources()
	if resources.CPUCount < 1 || resources.CPUCount > maxVCPUs {
		t.Fatalf("cpu count = %d", resources.CPUCount)
	}
	if resources.MemoryBytes < minMemoryMiB*1024*1024 {
		t.Fatalf("memory = %d bytes, below the %d MiB floor", resources.MemoryBytes, minMemoryMiB)
	}
}

// The sockets a launcher owns are the ones it creates. The control plane's is
// not one of them: it is the server's own listening socket, and a caller that
// treated it as the launcher's would delete the thing every pool on the machine
// talks to.
func TestOwnedSocketsExcludeTheServersOwnSocket(t *testing.T) {
	cfg := validConfig()
	owned := cfg.OwnedSockets()
	for _, mapping := range cfg.VSOCK {
		if mapping.Direction != GuestConnects {
			continue
		}
		if slices.Contains(owned, mapping.Socket) {
			t.Fatalf("owned sockets %v include the server's own socket %s", owned, mapping.Socket)
		}
	}
	if !slices.Contains(owned, cfg.PasstSocket) {
		t.Fatalf("owned sockets %v omit passt's", owned)
	}
	for _, mapping := range cfg.VSOCK {
		if mapping.Direction == HostConnects && !slices.Contains(owned, mapping.Socket) {
			t.Fatalf("owned sockets %v omit the host-listening %s", owned, mapping.Name)
		}
	}
}

// A previous VM's sockets are cleared before a launcher starts, because their
// reappearance is what the driver reads as the VM being up. A socket that
// survived would make the next start look finished the instant it began.
func TestRemoveOwnedSocketsClearsWhatALauncherLeftBehind(t *testing.T) {
	dir := t.TempDir()
	cfg := validConfig()
	cfg.RuntimeDir = dir
	cfg.PasstSocket = filepath.Join(dir, "passt.sock")
	cfg.VSOCK = []VSOCKMapping{
		{Name: "control-plane", Port: 3001, Socket: filepath.Join(t.TempDir(), "server.sock"), Direction: GuestConnects},
		{Name: "docker", Port: 3004, Socket: filepath.Join(dir, "docker.sock"), Direction: HostConnects},
	}
	cfg.ConsoleLog = filepath.Join(dir, "console.log")
	for _, path := range append(cfg.OwnedSockets(), cfg.VSOCK[0].Socket) {
		var listen net.ListenConfig
		listener, err := listen.Listen(t.Context(), "unix", path)
		if err != nil {
			t.Fatal(err)
		}
		// Closed without unlinking, which is what a killed libkrun leaves.
		if file, err := listener.(*net.UnixListener).File(); err == nil {
			_ = file.Close()
		}
		listener.(*net.UnixListener).SetUnlinkOnClose(false)
		_ = listener.Close()
	}

	if err := RemoveOwnedSockets(cfg); err != nil {
		t.Fatalf("RemoveOwnedSockets: %v", err)
	}
	for _, path := range cfg.OwnedSockets() {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived: %v", path, err)
		}
	}
	if _, err := os.Lstat(cfg.VSOCK[0].Socket); err != nil {
		t.Fatalf("the server's own socket was removed: %v", err)
	}
}

// A path that is not a socket is not this pool's to delete.
func TestRemoveOwnedSocketsRefusesANonSocket(t *testing.T) {
	dir := t.TempDir()
	cfg := validConfig()
	cfg.RuntimeDir = dir
	cfg.PasstSocket = filepath.Join(dir, "passt.sock")
	cfg.VSOCK = nil
	cfg.ConsoleLog = filepath.Join(dir, "console.log")
	if err := os.WriteFile(cfg.PasstSocket, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveOwnedSockets(cfg); err == nil {
		t.Fatal("RemoveOwnedSockets deleted a regular file")
	}
}
