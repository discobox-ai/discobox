package config

import (
	"runtime"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/localipc"
)

func hasScheme(t *testing.T, endpoints []string, scheme string) bool {
	t.Helper()
	for _, endpoint := range endpoints {
		parsed, err := localipc.Parse(endpoint)
		if err != nil {
			continue
		}
		if parsed.Scheme == scheme {
			return true
		}
	}
	return false
}

// Where the Docker provider is the common case, an unconfigured server still
// opens the HTTP listener its pool agents dial.
func TestDefaultListenAddsHTTPWhenThePlatformWantsIt(t *testing.T) {
	endpoints := requireLocalAndHTTPListenEndpoints(nil, 18080, true)
	if !hasScheme(t, endpoints, "npipe") && !hasScheme(t, endpoints, "unix") {
		t.Fatalf("endpoints = %v, want a local IPC listener", endpoints)
	}
	if !hasScheme(t, endpoints, "http") {
		t.Fatalf("endpoints = %v, want an HTTP listener", endpoints)
	}
}

// On Windows nothing needs a TCP listener and one costs a firewall prompt, so an
// unconfigured server opens none.
func TestDefaultListenOmitsHTTPWhenThePlatformDoesNotWantIt(t *testing.T) {
	endpoints := requireLocalAndHTTPListenEndpoints(nil, 18080, false)
	if hasScheme(t, endpoints, "http") {
		t.Fatalf("endpoints = %v, want no HTTP listener by default", endpoints)
	}
	if len(endpoints) != 1 {
		t.Fatalf("endpoints = %v, want only the local IPC listener", endpoints)
	}
}

// The platform default must match this build, so the Windows server really does
// come up without a TCP listener.
func TestDefaultHTTPListenerFollowsPlatform(t *testing.T) {
	if got, want := defaultHTTPListener(), runtime.GOOS != "windows"; got != want {
		t.Fatalf("defaultHTTPListener() = %v on %s, want %v", got, runtime.GOOS, want)
	}
}

// Configuring HTTP stays possible everywhere; it is only no longer implied.
func TestExplicitHTTPIsHonoredEvenWhenNotDefaulted(t *testing.T) {
	endpoints := requireLocalAndHTTPListenEndpoints(
		[]string{`npipe:////./pipe/discobox`, "http://0.0.0.0:18080"}, 18080, false)
	if !hasScheme(t, endpoints, "http") {
		t.Fatalf("endpoints = %v, want the explicitly configured HTTP listener", endpoints)
	}
	if !hasScheme(t, endpoints, "npipe") {
		t.Fatalf("endpoints = %v, want the configured npipe listener", endpoints)
	}
}

// Naming the endpoints explicitly must not add a TCP listener the operator did
// not ask for. On Windows that listener costs a firewall prompt, and a wslc or
// libkrun pool never needs it: those agents reach the control plane over the
// guest relay socket and VSOCK.
func TestExplicitLocalOnlyListenAddsNoHTTP(t *testing.T) {
	endpoints := requireLocalAndHTTPListenEndpoints([]string{`npipe:////./pipe/discobox`}, 18080, false)
	if hasScheme(t, endpoints, "http") {
		t.Fatalf("endpoints = %v, want no HTTP listener when configured explicitly", endpoints)
	}
	if len(endpoints) != 1 {
		t.Fatalf("endpoints = %v, want exactly the configured endpoint", endpoints)
	}
}

// The CLI reaches the server over local IPC, so that listener is added back even
// when the operator names only an HTTP endpoint. Losing it would leave a running
// server the CLI cannot reach.
func TestExplicitHTTPOnlyStillGetsLocalIPC(t *testing.T) {
	endpoints := requireLocalAndHTTPListenEndpoints([]string{"http://127.0.0.1:9000"}, 18080, false)
	if !hasScheme(t, endpoints, "npipe") && !hasScheme(t, endpoints, "unix") {
		t.Fatalf("endpoints = %v, want the local IPC listener retained", endpoints)
	}
	if !hasScheme(t, endpoints, "http") {
		t.Fatalf("endpoints = %v, want the configured HTTP listener kept", endpoints)
	}
	for _, endpoint := range endpoints {
		if strings.Contains(endpoint, "18080") {
			t.Fatalf("endpoints = %v, want no extra default HTTP listener", endpoints)
		}
	}
}
