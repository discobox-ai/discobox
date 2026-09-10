//go:build linux && amd64

package libkrun

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/providers/dockerworker"
	"github.com/discobox-ai/discobox/server/providers/guestimage"
	"github.com/discobox-ai/discobox/server/providers/libkrun/internal/krunvm"
)

// The launcher is this binary re-executed, so the thing most worth proving is
// that the re-exec lands somewhere that understands the argv — and that when
// the child cannot start a VM, what the driver reports is the child's own
// reason rather than "it exited".
//
// It needs KVM and nothing else: the child gets as far as loading libkrun,
// which is deliberately pointed at a library that is not there.
func TestLauncherFailureIsReportedWithTheChildsReason(t *testing.T) {
	requireKVM(t)
	root := t.TempDir()
	driver := newTestDriver(t, root, filepath.Join(root, "nonexistent-libkrun.so"))
	// The runtime directory outlives a server process and libkrun does not
	// unlink its sockets when it is killed, so the second start on a machine
	// finds all four already there. If those satisfied readiness, this failing
	// launcher would be reported as a running pool.
	seedStaleSockets(t, driver.poolRuntimeDir("pool_1"))

	_, err := driver.EnsureVM(t.Context(), "pool_1", dockerworker.VMSpec{})
	if err == nil {
		t.Fatal("EnsureVM succeeded with no libkrun installed")
	}
	if !strings.Contains(err.Error(), "nonexistent-libkrun.so") {
		t.Fatalf("error = %v, want the launcher's own reason", err)
	}
	// Not "unknown command", not "flag provided but not defined": a re-exec that
	// was not recognized would fail with the host binary's own argument error.
	if strings.Contains(err.Error(), "unknown command") || strings.Contains(err.Error(), "flag provided") {
		t.Fatalf("the launcher argv was not recognized by the re-executed binary: %v", err)
	}
}

// Both halves of "the VM dies with the server" are spawn parameters, and losing
// either is silent: a VM would keep running after its server exited, reachable
// by nothing, holding a pool's disks open.
//
// PR_SET_PDEATHSIG covers everything after the child is running. The pipe covers
// the window before it arms one — the read end is the child's fd 3, the write
// end is held here and nowhere else, so it closes when this process exits
// whether or not the child ever got that far.
func TestTheLauncherIsSpawnedWithBothLifetimeMechanisms(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "run", "pool_1")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	driver := newTestDriver(t, root, "")
	manifest := krunvm.Config{RuntimeDir: runtimeDir, PasstSocket: filepath.Join(runtimeDir, passtSocketName)}
	guest, err := driver.startLauncher(manifest, filepath.Join(runtimeDir, manifestName))
	if err != nil {
		t.Fatalf("startLauncher: %v", err)
	}
	t.Cleanup(guest.close)

	if guest.cmd.SysProcAttr == nil || guest.cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatalf("SysProcAttr = %#v, want Pdeathsig SIGKILL", guest.cmd.SysProcAttr)
	}
	if guest.cmd.SysProcAttr.Setsid {
		t.Fatal("the launcher was put in its own session, which detaches it from the server's fate")
	}
	if len(guest.cmd.ExtraFiles) != 1 {
		t.Fatalf("ExtraFiles = %v, want the watchdog pipe's read end at fd 3", guest.cmd.ExtraFiles)
	}
	if guest.watchdog == nil {
		t.Fatal("the driver did not keep the watchdog pipe's write end")
	}

	// The child was handed a manifest that does not exist, so it exits at once;
	// what matters is that closing releases the pipe rather than leaking it.
	guest.close()
	if err := guest.watchdog.Close(); err == nil {
		t.Fatal("closing the driver left the watchdog pipe open")
	}
	if !guest.waitExited(10 * time.Second) {
		t.Fatal("the launcher outlived the driver that started it")
	}
}

func newTestDriver(t *testing.T, root, libraryPath string) *Driver {
	t.Helper()
	artifacts := filepath.Join(root, "artifacts")
	if err := os.MkdirAll(artifacts, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{rootArtifact, kernelArtifact} {
		if err := os.WriteFile(filepath.Join(artifacts, name), []byte("not a real artifact"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	resolver := func(artifact string) *guestimage.Resolver {
		r, err := guestimage.New(guestimage.Config{
			OverrideDir: artifacts,
			Artifacts:   []guestimage.Artifact{{Name: artifact}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	driver, err := NewDriver(DriverConfig{
		Guest:              resolver(rootArtifact),
		Kernel:             resolver(kernelArtifact),
		StateDir:           filepath.Join(root, "state"),
		RuntimeDir:         filepath.Join(root, "run"),
		ControlPlaneSocket: filepath.Join(root, "server.sock"),
		LibraryPath:        libraryPath,
		VCPUs:              1,
		MemoryMiB:          512,
		DataDiskGiB:        1,
		CacheDiskGiB:       1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = driver.Close() })
	return driver
}

// seedStaleSockets leaves behind what a SIGKILLed launcher leaves behind:
// bound Unix sockets with nothing listening on them.
func seedStaleSockets(t *testing.T, runtimeDir string) {
	t.Helper()
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{passtSocketName, agentSocketName, lifecycleSocketName, dockerSocketName} {
		path := filepath.Join(runtimeDir, name)
		var listen net.ListenConfig
		listener, err := listen.Listen(t.Context(), "unix", path)
		if err != nil {
			t.Fatal(err)
		}
		listener.(*net.UnixListener).SetUnlinkOnClose(false)
		_ = listener.Close()
		if !isUnixSocket(path) {
			t.Fatalf("%s is not a socket", path)
		}
	}
}

func requireKVM(t *testing.T) {
	t.Helper()
	device, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("this host has no usable /dev/kvm: %v", err)
	}
	_ = device.Close()
}
