package endpoint

import (
	"strings"
	"testing"
)

func TestParseUnixEndpoint(t *testing.T) {
	endpoint, err := Parse("unix:///tmp/discobox/server.sock")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if endpoint.Scheme != "unix" || endpoint.Value != "/tmp/discobox/server.sock" {
		t.Fatalf("endpoint = %#v", endpoint)
	}
}

func TestParseNpipeEndpoint(t *testing.T) {
	endpoint, err := Parse("npipe:////./pipe/discobox")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if endpoint.Scheme != "npipe" || endpoint.Value != `\\.\pipe\discobox` {
		t.Fatalf("endpoint = %#v", endpoint)
	}
}

func TestDefaultEndpointIsLocalIPC(t *testing.T) {
	endpoint := DefaultEndpoint()
	if !strings.HasPrefix(endpoint, "unix://") && !strings.HasPrefix(endpoint, "npipe://") {
		t.Fatalf("DefaultEndpoint() = %q, want local IPC endpoint", endpoint)
	}
}
