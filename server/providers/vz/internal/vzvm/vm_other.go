//go:build !darwin

package vzvm

import (
	"net"
	"time"
)

// VM is the non-darwin stand-in. It exists so the driver, its configuration,
// and its tests compile and run everywhere, exactly as wslcsession does for
// Windows. Nothing constructs one: the provider is registered on darwin only.
type VM struct{}

// Supported reports that this build cannot run VMs.
func Supported() error { return ErrUnsupported }

// CreateDiskImage is unavailable off darwin.
func CreateDiskImage(string, int64) error { return ErrUnsupported }

// Start is unavailable off darwin.
func Start(Options) (*VM, error) { return nil, ErrUnsupported }

func (*VM) Connect(uint32) (net.Conn, error) { return nil, ErrUnsupported }

func (*VM) Listen(uint32) (net.Listener, error) { return nil, ErrUnsupported }

func (*VM) Running() bool { return false }

func (*VM) RequestStop() error { return ErrUnsupported }

func (*VM) WaitStopped(time.Duration) bool { return true }

func (*VM) Close() error { return nil }
