package config

import (
	"testing"

	"github.com/discobox-ai/discobox/endpoint"
)

func hasScheme(t *testing.T, endpoints []string, scheme string) bool {
	t.Helper()
	for _, raw := range endpoints {
		parsed, err := endpoint.Parse(raw)
		if err != nil {
			continue
		}
		if parsed.Scheme == scheme {
			return true
		}
	}
	return false
}

// An unconfigured server is local-only on every platform: a TCP port is a
// machine-wide surface nothing needs by default.
func TestDefaultListenIsLocalOnly(t *testing.T) {
	endpoints := requireLocalListenEndpoint(nil)
	if !hasScheme(t, endpoints, "npipe") && !hasScheme(t, endpoints, "unix") {
		t.Fatalf("endpoints = %v, want a local IPC listener", endpoints)
	}
	if hasScheme(t, endpoints, "http") {
		t.Fatalf("endpoints = %v, want no HTTP listener by default", endpoints)
	}
	if len(endpoints) != 1 {
		t.Fatalf("endpoints = %v, want only the local IPC listener", endpoints)
	}
}

// Configuring HTTP stays possible; it is only never implied.
func TestExplicitHTTPIsHonored(t *testing.T) {
	endpoints := requireLocalListenEndpoint([]string{`npipe:////./pipe/discobox`, "http://0.0.0.0:18080"})
	if !hasScheme(t, endpoints, "http") {
		t.Fatalf("endpoints = %v, want the explicitly configured HTTP listener", endpoints)
	}
	if !hasScheme(t, endpoints, "npipe") {
		t.Fatalf("endpoints = %v, want the configured npipe listener", endpoints)
	}
	if len(endpoints) != 2 {
		t.Fatalf("endpoints = %v, want exactly the configured endpoints", endpoints)
	}
}

// Naming the endpoints explicitly must not add a TCP listener the operator did
// not ask for.
func TestExplicitLocalOnlyListenAddsNoHTTP(t *testing.T) {
	endpoints := requireLocalListenEndpoint([]string{`npipe:////./pipe/discobox`})
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
	endpoints := requireLocalListenEndpoint([]string{"http://127.0.0.1:9000"})
	if !hasScheme(t, endpoints, "npipe") && !hasScheme(t, endpoints, "unix") {
		t.Fatalf("endpoints = %v, want the local IPC listener retained", endpoints)
	}
	if !hasScheme(t, endpoints, "http") {
		t.Fatalf("endpoints = %v, want the configured HTTP listener kept", endpoints)
	}
	if len(endpoints) != 2 {
		t.Fatalf("endpoints = %v, want the local listener plus the configured one", endpoints)
	}
}
