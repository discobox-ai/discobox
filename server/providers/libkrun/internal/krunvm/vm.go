package krunvm

import (
	"errors"
	"fmt"
	"runtime"
)

// ErrUnsupported reports that this build cannot run a libkrun microVM.
//
// It is the same failure ergonomics as relay.ErrNotBuilt: the provider is
// registered everywhere, because a user configuring one should be told why it
// cannot run rather than not be shown it at all, and it refuses at the point a
// pool would start.
var ErrUnsupported = errors.New("krunvm: libkrun microVMs need linux/amd64 with KVM")

// ErrKVMUnavailable reports a host whose /dev/kvm cannot be opened or does not
// speak the KVM API this launcher expects. CheckKVM wraps it with the cause.
var ErrKVMUnavailable = errors.New("krunvm: KVM is not available")

// Supported reports whether this build can start a libkrun microVM.
//
// It is a platform gate and nothing more. Configuring a provider must not open
// /dev/kvm: KVM is a property of the machine at the moment a VM starts, not of
// the server's configuration, and checking it here would make a provider
// unconstructible on a host that will have it by the time a pool is created.
// The launcher checks it immediately before starting a VM, and its error
// arrives through the driver (ADR 0013).
func Supported() error {
	if !hostSupported() {
		return ErrUnsupported
	}
	return nil
}

// hostSupported is the platform gate. libkrun itself runs wherever KVM does,
// but the kernel it boots is a published artifact and amd64 is the only
// architecture that artifact is built for.
func hostSupported() bool {
	return runtime.GOOS == "linux" && runtime.GOARCH == "amd64"
}

// CheckKVM reports whether this host can start a libkrun microVM right now:
// the platform gate, then a usable /dev/kvm. It is what a server asks before it
// installs libkrun as a machine's default provider (ADR 0148 §2) — the one
// point at which KVM is a property of the decision rather than of a pool that
// has not started yet (see Supported).
func CheckKVM() error {
	if !hostSupported() {
		return ErrUnsupported
	}
	if err := checkKVM(); err != nil {
		return fmt.Errorf("%w: %w", ErrKVMUnavailable, err)
	}
	return nil
}
