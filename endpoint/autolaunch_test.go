package endpoint

import "testing"

func TestSystemdUnitNameIsStableAndScoped(t *testing.T) {
	opts := LaunchOptions{
		Endpoint: "unix:///tmp/discobox/server.sock",
		Command:  "/usr/local/bin/discobox",
	}

	got := userServiceUnitName(opts)
	if got != "discobox-server-30ad8514897f671d" {
		t.Fatalf("userServiceUnitName() = %q", got)
	}
}

func TestSystemdUnitNameSanitizesCommandName(t *testing.T) {
	got := systemdUnitName("Discobox Dev!", "abc123")
	if got != "discobox-dev-server-abc123" {
		t.Fatalf("systemdUnitName() = %q", got)
	}
}
