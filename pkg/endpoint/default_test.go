package endpoint

import "testing"

func TestDefaultEndpointHonorsAddressEnv(t *testing.T) {
	t.Setenv(AddressEnv, "")
	platform := platformDefaultEndpoint()
	if got := DefaultEndpoint(); got != platform {
		t.Fatalf("DefaultEndpoint() = %q with %s unset, want the platform default %q", got, AddressEnv, platform)
	}
	const pinned = "unix:///tmp/discobox-test/discobox/server.sock"
	t.Setenv(AddressEnv, " "+pinned+" ")
	if got := DefaultEndpoint(); got != pinned {
		t.Fatalf("DefaultEndpoint() = %q, want %s's %q", got, AddressEnv, pinned)
	}
}
