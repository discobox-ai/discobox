// Package vsock provides the guest-side AF_VSOCK transport used by local
// libkrun and vz pools. HTTP remains the application protocol; this package only
// supplies net.Listener and net.Conn implementations.
//
// Linux guests use mdlayher/vsock. A macOS guest has AF_VSOCK too, but that
// library implements Linux only, so darwin has its own (transport_darwin.go).
package vsock

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"
)

const (
	// HostCID is the standard guest-visible VSOCK context ID for the host
	// (VMADDR_CID_HOST), the same on Linux and macOS guests.
	HostCID uint32 = 2
	// AnyCID binds on the guest's assigned CID without consulting /dev/vsock.
	// This matters for the pool-agent container: AF_VSOCK is available through
	// the shared guest kernel even when the character device is not mounted
	// into the container.
	AnyCID = ^uint32(0)

	// EnvControlPlanePort is inherited by the pool proxy systemd unit so all
	// guest-to-control-plane HTTP uses the same VSOCK transport.
	EnvControlPlanePort = "DISCOBOX_CONTROL_PLANE_VSOCK_PORT"
)

// Listen binds a guest AF_VSOCK listener on port.
func Listen(port uint32) (net.Listener, error) {
	if port < 1024 {
		return nil, fmt.Errorf("vsock listener port %d must be at least 1024", port)
	}
	return listen(AnyCID, port)
}

// DialHostContext dials a fixed host VSOCK port. The network and address
// arguments are intentionally ignored so it can be installed directly as an
// HTTP Transport DialContext.
func DialHostContext(port uint32) func(context.Context, string, string) (net.Conn, error) {
	return DialContextCID(HostCID, port)
}

// DialContextCID dials a fixed VSOCK context ID and port. The network and
// address arguments are intentionally ignored so it can be installed directly
// as an HTTP Transport DialContext.
func DialContextCID(cid, port uint32) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		if port < 1024 {
			return nil, fmt.Errorf("vsock port %d must be at least 1024", port)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := dial(cid, port)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return conn, nil
	}
}

// HTTPClient returns an HTTP client whose every connection crosses VSOCK.
func HTTPClient(port uint32, timeout time.Duration) *http.Client {
	return &http.Client{
		Transport: &http.Transport{DialContext: DialHostContext(port)},
		Timeout:   timeout,
	}
}
