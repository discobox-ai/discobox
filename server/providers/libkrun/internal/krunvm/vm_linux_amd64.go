package krunvm

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// kvmGetAPIVersion and kvmAPIVersion are KVM's handshake. The ioctl is the only
// way to tell a usable /dev/kvm from one that merely exists, which is the
// difference between a VM that starts and a pool that fails at boot with
// nothing on its console.
const (
	kvmGetAPIVersion = 0xAE00
	kvmAPIVersion    = 12
)

func checkKVM() error {
	const path = "/dev/kvm"
	device, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w; this host has no KVM, or this user is not in the group that owns it", path, err)
	}
	defer func() { _ = device.Close() }()
	version, _, errno := syscall.Syscall(syscall.SYS_IOCTL, device.Fd(), kvmGetAPIVersion, 0)
	if errno != 0 {
		return fmt.Errorf("query the KVM API version from %s: %w", path, errno)
	}
	if version != kvmAPIVersion {
		return fmt.Errorf("KVM at %s reports API version %d, want %d", path, version, kvmAPIVersion)
	}
	return nil
}

// passtStartTimeout is how long passt gets to create its socket. It forks,
// binds, and writes the socket; anything slower than this is a failure that has
// not reported itself yet.
const passtStartTimeout = 5 * time.Second

// Run boots the VM this manifest describes and does not return while it lives.
//
// This runs in the launcher child, never in the server. The child is killed by
// its watchdog when the server exits, which is what makes "the VM dies with the
// server" true here as it is by construction on vz and wslc (ADR 0062 §9).
func Run(cfg Config) error {
	if !hostSupported() {
		return ErrUnsupported
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := checkKVM(); err != nil {
		return err
	}
	if err := cfg.ValidateDiskFiles(); err != nil {
		return err
	}
	if err := prepareRuntimeDir(cfg); err != nil {
		return err
	}

	lib, err := openLibrary(cfg.LibraryPath)
	if err != nil {
		return err
	}

	passt, err := startPasst(cfg)
	if err != nil {
		return err
	}
	supervisePasst(passt)

	// The vCPU threads libkrun creates belong to this goroutine's thread, and
	// the call never returns while the guest runs. Locking it keeps the Go
	// scheduler from moving the goroutine somewhere else mid-call.
	runtime.LockOSThread()
	return boot(cfg, lib)
}

// prepareRuntimeDir makes the pool's runtime directory private and clears
// sockets a previous VM left behind. libkrun refuses to bind a path that
// exists, and after an unclean exit these are exactly the paths that do.
func prepareRuntimeDir(cfg Config) error {
	if err := os.MkdirAll(cfg.RuntimeDir, 0o700); err != nil {
		return fmt.Errorf("create runtime directory %s: %w", cfg.RuntimeDir, err)
	}
	info, err := os.Lstat(cfg.RuntimeDir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("runtime directory %s must be a real directory", cfg.RuntimeDir)
	}
	if err := os.Chmod(cfg.RuntimeDir, 0o700); err != nil {
		return err
	}
	// The driver has already cleared these, because its readiness check depends
	// on having done so. Doing it again costs nothing and keeps a launcher
	// started by hand from failing on a path libkrun will not rebind.
	return RemoveOwnedSockets(cfg)
}

// passtProcess is passt plus the one channel its exit is reported on. Nothing
// may call Wait on the command itself: a second Wait returns an error rather
// than the status, so the exit has exactly one reader and everything that wants
// it reads this.
type passtProcess struct {
	cmd    *exec.Cmd
	exited chan error
}

// startPasst brings up the guest's only route off the machine.
//
// passt is unprivileged user-mode networking: it needs no TAP, TUN, veth, or
// bridge on the host and creates no interface an operator has to reason about
// (ADR 0013). Every pool gets the same fixed private addressing because the
// guests share nothing — each passt process serves exactly one VM over one Unix
// socket, so two pools using 192.168.127.2 cannot collide.
//
// The guest takes that addressing from passt's DHCP server rather than being
// configured by hand, which is what lets it boot the same image a
// Virtualization.framework guest does.
func startPasst(cfg Config) (*passtProcess, error) {
	nameserver, err := hostIPv4Nameserver()
	if err != nil {
		return nil, err
	}
	logPath := filepath.Join(cfg.RuntimeDir, "passt.log")
	// context.Background, deliberately: passt lives as long as this process
	// does, and this process is the VM. Its lifetime is arranged by
	// PR_SET_PDEATHSIG below, not by a context.
	//nolint:gosec // Every argument is this process's own manifest, written by the driver.
	cmd := exec.CommandContext(context.Background(), cfg.PasstExecutable(),
		"--foreground",
		"--one-off",
		"--socket", cfg.PasstSocket,
		"--tcp-ports", "none",
		"--udp-ports", "none",
		"--address", guestAddress,
		"--netmask", guestNetmask,
		"--gateway", guestGateway,
		"--dns", guestResolver,
		"--dns-forward", guestResolver,
		"--dns-host", nameserver,
		"--map-host-loopback", "none",
		"--map-guest-addr", "none",
		"--no-map-gw",
		"--ipv4-only",
		"--log-file", logPath,
		"--log-size", "1048576",
	)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	// passt dies with the launcher, which dies with the server. Nothing
	// reconciles a passt that outlived its VM, so nothing may.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start passt %s: %w", cfg.PasstExecutable(), err)
	}
	passt := &passtProcess{cmd: cmd, exited: make(chan error, 1)}
	go func() { passt.exited <- cmd.Wait() }()

	deadline := time.Now().Add(passtStartTimeout)
	for {
		if info, err := os.Lstat(cfg.PasstSocket); err == nil && info.Mode()&os.ModeSocket != 0 {
			return passt, nil
		}
		// Checked before the deadline, so a passt that fails at once — a bad
		// argument, a missing binary's interpreter, an address already in use —
		// is reported as what happened rather than as a five-second timeout.
		select {
		case err := <-passt.exited:
			return nil, fmt.Errorf("passt exited before creating %s; see %s: %w", cfg.PasstSocket, logPath, err)
		default:
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			<-passt.exited
			return nil, fmt.Errorf("passt did not create %s within %s; see %s", cfg.PasstSocket, passtStartTimeout, logPath)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// supervisePasst ends the VM when its network does. A guest whose passt died
// keeps running with a link that silently drops everything, which looks like a
// hung pool rather than a failed one.
func supervisePasst(passt *passtProcess) {
	go func() {
		fmt.Fprintf(os.Stderr, "discobox launcher: passt exited: %v\n", <-passt.exited)
		os.Exit(1)
	}()
}

// The guest's fixed private network. passt hands these out over DHCP, so they
// appear in exactly one place: here.
const (
	guestAddress  = "192.168.127.2"
	guestNetmask  = "255.255.255.0"
	guestGateway  = "192.168.127.1"
	guestResolver = "192.168.127.53"
)

// hostIPv4Nameserver is where passt forwards the guest's DNS. The guest is
// given guestResolver, which exists only inside passt; passt then asks whatever
// this host asks.
func hostIPv4Nameserver() (string, error) {
	const path = "/etc/resolv.conf"
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the host resolver configuration %s: %w", path, err)
	}
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		if address := net.ParseIP(fields[1]); address != nil && address.To4() != nil {
			return address.String(), nil
		}
	}
	return "", fmt.Errorf("the host resolver configuration %s names no IPv4 nameserver", path)
}
