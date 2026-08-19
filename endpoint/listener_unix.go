//go:build !windows

package endpoint

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

func Listen(endpoint string) (net.Listener, string, func(), error) {
	parsed, err := Parse(endpoint)
	if err != nil {
		return nil, "", nil, err
	}
	switch parsed.Scheme {
	case "http":
		addr := parsed.Value
		hostPort := addr[len("http://"):]
		var listenConfig net.ListenConfig
		listener, err := listenConfig.Listen(context.Background(), "tcp", hostPort)
		if err != nil {
			return nil, "", nil, err
		}
		return listener, parsed.Value, func() {}, nil
	case "unix":
		if err := os.MkdirAll(filepath.Dir(parsed.Value), 0o700); err != nil {
			return nil, "", nil, fmt.Errorf("create socket directory: %w", err)
		}
		_ = os.Remove(parsed.Value)
		var listenConfig net.ListenConfig
		listener, err := listenConfig.Listen(context.Background(), "unix", parsed.Value)
		if err != nil {
			return nil, "", nil, err
		}
		cleanup := func() {
			_ = listener.Close()
			_ = os.Remove(parsed.Value)
		}
		return listener, parsed.Raw, cleanup, nil
	case "iroh":
		return irohListen(parsed)
	case "https", "npipe":
		return nil, "", nil, fmt.Errorf("server listen endpoint %q is not supported on this platform", endpoint)
	default:
		return nil, "", nil, fmt.Errorf("unsupported endpoint scheme %q", parsed.Scheme)
	}
}
