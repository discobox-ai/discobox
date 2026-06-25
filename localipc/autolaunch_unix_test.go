//go:build !windows

package localipc

import (
	"slices"
	"testing"
)

func TestSystemdRunArgsStartsUserUnitWithEnvironment(t *testing.T) {
	opts := LaunchOptions{
		Endpoint: "unix:///tmp/discobox/server.sock",
		Command:  "/usr/local/bin/discobox",
		Args:     []string{"server"},
		Env: []string{
			"DISCOBOX_SERVER=unix:///tmp/discobox/server.sock",
			"DISCOBOX_SERVER_IDLE_TIMEOUT=5m",
		},
	}

	args := systemdRunArgs(opts)
	for _, want := range []string{
		"--user",
		"--collect",
		"--unit=discobox-server-30ad8514897f671d",
		"--property=Description=Discobox local API server",
		"--setenv=DISCOBOX_SERVER=unix:///tmp/discobox/server.sock",
		"--setenv=DISCOBOX_SERVER_IDLE_TIMEOUT=5m",
	} {
		if !slices.Contains(args, want) {
			t.Fatalf("systemdRunArgs() missing %q", want)
		}
	}
	if got := args[len(args)-3:]; !slices.Equal(got, []string{"--", "/usr/local/bin/discobox", "server"}) {
		t.Fatalf("systemdRunArgs() command tail = %#v", got)
	}
}
