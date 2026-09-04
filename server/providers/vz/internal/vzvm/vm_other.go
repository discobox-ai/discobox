//go:build !darwin || !cgo

package vzvm

import (
	"fmt"
	"net"
	"runtime"
	"time"
)

// VM is the stand-in for every build that has no Virtualization.framework
// bindings. It exists so the driver, its configuration, and its tests compile
// and run everywhere, exactly as wslcsession does for Windows.
//
// It covers two builds. Off macOS the framework does not exist and the
// provider is never registered, so nothing constructs a VM. On macOS with cgo
// disabled the provider *is* registered — platform_darwin.go is selected by
// GOOS alone — and this stub is what it gets. That build is a mistake rather
// than a platform, so it must not fail silently or at init: every entry point
// reports why, and NewDriver's Supported check turns it into a provider that
// refuses its first use with an actionable message.
type VM struct{}

// The two reasons this build has no bindings. They are values rather than
// errors built where they are returned so that the entry points keep returning
// an opaque error: a function that can only return a concrete non-nil type
// makes every caller's `err != nil` provably true, which staticcheck reports
// on the driver rather than here (SA4023).
var (
	errNotMacOS = fmt.Errorf("%w: the framework is macOS-only", ErrUnsupported)
	errNoCgo    = fmt.Errorf("%w: the bindings are cgo and this binary was built with CGO_ENABLED=0; rebuild it with CGO_ENABLED=1", ErrUnsupported)
)

// unsupported names which of the two builds this is. The fix differs — use a
// Mac, or turn cgo back on — so the message has to say which one applies.
func unsupported() error {
	if runtime.GOOS == "darwin" {
		return errNoCgo
	}
	return errNotMacOS
}

// Supported reports that this build cannot run VMs.
func Supported() error { return unsupported() }

// CreateDiskImage is unavailable without the bindings.
func CreateDiskImage(string, int64) error { return unsupported() }

// Start is unavailable without the bindings.
func Start(Options) (*VM, error) { return nil, unsupported() }

func (*VM) Connect(uint32) (net.Conn, error) { return nil, unsupported() }

func (*VM) Listen(uint32) (net.Listener, error) { return nil, unsupported() }

func (*VM) Running() bool { return false }

func (*VM) RequestStop() error { return unsupported() }

func (*VM) WaitStopped(time.Duration) bool { return true }

func (*VM) Close() error { return nil }
