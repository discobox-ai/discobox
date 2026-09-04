package endpoint

import (
	"testing"

	"github.com/discobox-ai/discobox/health"
)

func TestOlderServer(t *testing.T) {
	tests := map[string]struct {
		server   string
		expected string
		older    bool
	}{
		"older release":       {server: "v0.5.0", expected: "v0.5.1", older: true},
		"same release":        {server: "v0.5.1", expected: "v0.5.1"},
		"newer server":        {server: "v0.6.0", expected: "v0.5.1"},
		"unversioned server":  {expected: "v0.5.1"},
		"development client":  {server: "v0.5.0", expected: "accd52c7"},
		"prerelease is older": {server: "v0.5.1-rc.1", expected: "v0.5.1", older: true},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got := olderServer(health.Status{Version: test.server}, test.expected)
			if got != test.older {
				t.Fatalf("olderServer(%q, %q) = %v, want %v", test.server, test.expected, got, test.older)
			}
		})
	}
}

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
