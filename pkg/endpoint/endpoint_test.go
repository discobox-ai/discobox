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

// "unix://" and "npipe://" name the transport and leave the address to this
// machine, the way "iroh://" leaves the identity to the key file. It is what
// makes "unix://,iroh://" a thing an operator can write.
func TestParseLocalSchemeWithoutAPathIsTheDefault(t *testing.T) {
	local, err := Parse(DefaultEndpoint())
	if err != nil {
		t.Fatalf("Parse(DefaultEndpoint()) error = %v", err)
	}

	empty, err := Parse(local.Scheme + "://")
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", local.Scheme+"://", err)
	}
	if empty.Scheme != local.Scheme || empty.Value != local.Value {
		t.Fatalf("Parse(%q) = %#v, want the default endpoint %#v", local.Scheme+"://", empty, local)
	}
	// Raw is the resolved endpoint rather than the shorthand, because Raw is
	// what a listener displays: "listening on unix://" tells an operator
	// nothing about where their server actually is.
	if empty.Raw != DefaultEndpoint() {
		t.Fatalf("Raw = %q, want the resolved %q", empty.Raw, DefaultEndpoint())
	}
}

// The other local scheme has no default here, and saying so is better than
// binding something else: it means the configuration was written for a
// different platform.
func TestParseLocalSchemeWithoutAPathRefusesTheWrongPlatform(t *testing.T) {
	local, err := Parse(DefaultEndpoint())
	if err != nil {
		t.Fatalf("Parse(DefaultEndpoint()) error = %v", err)
	}
	other := "npipe"
	if local.Scheme == "npipe" {
		other = "unix"
	}

	_, err = Parse(other + "://")
	if err == nil {
		t.Fatalf("Parse(%q) succeeded, want an error on a platform whose local endpoint is %s", other+"://", local.Scheme)
	}
	if !strings.Contains(err.Error(), DefaultEndpoint()) {
		t.Fatalf("error = %v, want it to name the endpoint this platform does use", err)
	}
}
