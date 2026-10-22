//go:build windows

package endpoint

import (
	"fmt"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
)

func Listen(endpoint string) (net.Listener, string, func(), error) {
	parsed, err := Parse(endpoint)
	if err != nil {
		return nil, "", nil, err
	}
	switch parsed.Scheme {
	case "http":
		addr := strings.TrimPrefix(parsed.Value, "http://")
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, "", nil, err
		}
		return listener, parsed.Value, func() {}, nil
	case "npipe":
		listener, err := winio.ListenPipe(parsed.Value, nil)
		if err != nil {
			return nil, "", nil, err
		}
		return listener, parsed.Raw, func() { _ = listener.Close() }, nil
	case "iroh":
		return irohListen(parsed)
	case "https", "unix":
		return nil, "", nil, fmt.Errorf("server listen endpoint %q is not supported on this platform", endpoint)
	default:
		return nil, "", nil, fmt.Errorf("unsupported endpoint scheme %q", parsed.Scheme)
	}
}
