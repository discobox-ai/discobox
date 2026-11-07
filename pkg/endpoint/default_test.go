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

func TestAddressEnvCannotRecursivelyResolveItself(t *testing.T) {
	for _, address := range []string{"unix://", "npipe://", "unix:", "npipe:"} {
		t.Run(address, func(t *testing.T) {
			t.Setenv(AddressEnv, address)
			if _, err := Parse(DefaultEndpoint()); err == nil {
				t.Fatal("expected a missing socket or pipe path error")
			}
		})
	}
}
