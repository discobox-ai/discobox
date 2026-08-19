package libkrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/endpoint"
)

func TestProviderIdentity(t *testing.T) {
	if ProviderType != "libkrun" {
		t.Fatalf("ProviderType = %q, want libkrun", ProviderType)
	}
	if got := Definition().Name; got != "libkrun" {
		t.Fatalf("provider name = %q, want libkrun", got)
	}
}

func TestNewDriverDefersKVMValidationToLauncher(t *testing.T) {
	requireLinuxHost(t)
	root := filepath.Join(t.TempDir(), "root.qcow2")
	kernel := filepath.Join(t.TempDir(), "vmlinux")
	for _, path := range []string{root, kernel} {
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	driver, err := NewDriver(DriverConfig{
		RootImage:          root,
		KernelImage:        kernel,
		StateDir:           filepath.Join(t.TempDir(), "state"),
		RuntimeDir:         filepath.Join(t.TempDir(), "run"),
		ControlPlaneSocket: filepath.Join(t.TempDir(), "server.sock"),
		LauncherPath:       executable,
		MkfsPath:           executable,
	})
	if err != nil {
		t.Fatalf("initialize driver without opening /dev/kvm: %v", err)
	}
	if err := driver.Close(); err != nil {
		t.Fatalf("close driver: %v", err)
	}
}

func TestStorageDirectoriesUseLibkrunNamespaceAndAdoptLegacyState(t *testing.T) {
	dataHome := filepath.Join(t.TempDir(), "data")
	runtimeHome := filepath.Join(t.TempDir(), "run")
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeHome)

	if got, want := defaultStateDir(), filepath.Join(dataHome, "discobox", "libkrun"); got != want {
		t.Fatalf("default state directory = %q, want %q", got, want)
	}
	if got, want := defaultRuntimeDir(), filepath.Join(runtimeHome, "discobox", "libkrun"); got != want {
		t.Fatalf("default runtime directory = %q, want %q", got, want)
	}
	for _, path := range []string{legacyStateDir(), legacyRuntimeDir()} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if got := effectiveStateDir(""); got != legacyStateDir() {
		t.Fatalf("effective state directory = %q, want legacy %q", got, legacyStateDir())
	}
	if got := effectiveRuntimeDir(""); got != legacyRuntimeDir() {
		t.Fatalf("effective runtime directory = %q, want legacy %q", got, legacyRuntimeDir())
	}
}

func TestValidateRequiresAbsoluteRootImage(t *testing.T) {
	if err := Validate(json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "rootImage") {
		t.Fatalf("validate error = %v, want rootImage requirement", err)
	}
	if err := Validate(json.RawMessage(`{"rootImage":"relative.qcow2"}`)); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("validate error = %v, want absolute path requirement", err)
	}
}

func TestValidateRequiresAbsoluteKernelImage(t *testing.T) {
	requireLinuxHost(t)
	if err := Validate(json.RawMessage(`{"rootImage":"/images/root.qcow2"}`)); err == nil || !strings.Contains(err.Error(), "kernelImage") {
		t.Fatalf("validate error = %v, want kernelImage requirement", err)
	}
	if err := Validate(json.RawMessage(`{"rootImage":"/images/root.qcow2","kernelImage":"vmlinux"}`)); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("validate error = %v, want absolute path requirement", err)
	}
}

func TestValidateAcceptsUnixControlPlaneSocket(t *testing.T) {
	requireLinuxHost(t)
	root := filepath.Join(t.TempDir(), "root.qcow2")
	kernel := filepath.Join(t.TempDir(), "vmlinux")
	data, err := json.Marshal(Config{
		RootImage:          root,
		KernelImage:        kernel,
		ControlPlaneSocket: "unix://" + filepath.Join(t.TempDir(), "server.sock"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(data); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestValidateRejectsInvalidSizing(t *testing.T) {
	for _, config := range []Config{
		{RootImage: "/images/root.qcow2", KernelImage: "/images/vmlinux", VCPUs: 256},
		{RootImage: "/images/root.qcow2", KernelImage: "/images/vmlinux", MemoryMiB: 128},
		{RootImage: "/images/root.qcow2", KernelImage: "/images/vmlinux", DataDiskGiB: 4097},
		{RootImage: "/images/root.qcow2", KernelImage: "/images/vmlinux", CacheDiskGiB: 4097},
	} {
		data, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := Validate(data); err == nil {
			t.Fatalf("Validate(%s) succeeded", data)
		}
	}
}

func TestDriverConfigUsesVSOCKAndStorageDefaults(t *testing.T) {
	cfg := driverConfig(Config{RootImage: "/images/root.qcow2", KernelImage: "/images/vmlinux"})
	if cfg.ControlPlaneSocket == "" {
		t.Fatal("control plane socket default is empty")
	}
	if parsed, err := endpoint.Parse(endpoint.DefaultEndpoint()); err != nil || cfg.ControlPlaneSocket != parsed.Value {
		t.Fatalf("control plane socket = %q, parsed default = %#v, err = %v", cfg.ControlPlaneSocket, parsed, err)
	}
	if cfg.VCPUs != defaultVCPUs || cfg.MemoryMiB != defaultMemoryMiB {
		t.Fatalf("compute defaults = %d vCPUs, %d MiB", cfg.VCPUs, cfg.MemoryMiB)
	}
	if cfg.DataDiskGiB != defaultDataDiskGiB || cfg.CacheDiskGiB != defaultCacheDiskGiB {
		t.Fatalf("disk defaults = %d/%d GiB", cfg.DataDiskGiB, cfg.CacheDiskGiB)
	}
}

func TestMACAddressIsStableLocalUnicast(t *testing.T) {
	first := macAddress("pool_123")
	if first != macAddress("pool_123") {
		t.Fatal("MAC address is not stable")
	}
	octet, err := strconv.ParseUint(strings.Split(first, ":")[0], 16, 8)
	if err != nil || octet&0x03 != 0x02 {
		t.Fatalf("MAC address %q is not locally administered unicast", first)
	}
}

func TestValidatePoolIDRejectsPaths(t *testing.T) {
	for _, value := range []string{"", "..", "pool/other", "pool..other"} {
		if err := validatePoolID(value); err == nil {
			t.Fatalf("validatePoolID(%q) succeeded", value)
		}
	}
	if err := validatePoolID("pool_9ade63td40g87ddm"); err != nil {
		t.Fatalf("valid pool ID rejected: %v", err)
	}
}

// requireLinuxHost skips a test whose subject is Linux-only. The libkrun driver
// refuses to initialize anywhere but x86-64 Linux, and its configuration is
// validated as POSIX absolute paths -- "/images/root.qcow2" is not absolute to
// a Windows filepath, and a Windows path cannot be spelled inside a unix:// URL
// at all. There is nothing to assert about this provider off Linux.
func requireLinuxHost(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("libkrun is x86-64 Linux only")
	}
}
