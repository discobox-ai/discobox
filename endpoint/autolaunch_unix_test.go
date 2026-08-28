//go:build !windows

package endpoint

import (
	"slices"
	"testing"
)

func TestSystemdRunArgsStartsUserUnitWithEnvironment(t *testing.T) {
	opts := LaunchOptions{
		Endpoint: "unix:///tmp/discobox/server.sock",
		LogPath:  "/tmp/discobox-state/server.log",
		Command:  "/usr/local/bin/discobox",
		Args:     []string{"server"},
		Env: []string{
			"DISCOBOX_SERVER=unix:///tmp/discobox/server.sock",
		},
	}

	args := systemdRunArgs(opts)
	for _, want := range []string{
		"--user",
		"--collect",
		"--unit=discobox-server-30ad8514897f671d",
		"--property=Description=Discobox local API server",
		"--setenv=DISCOBOX_SERVER=unix:///tmp/discobox/server.sock",
		// The unit writes where a directly executed child writes, so reading a
		// server's output does not depend on how this machine started it.
		"--property=StandardOutput=append:/tmp/discobox-state/server.log",
		"--property=StandardError=append:/tmp/discobox-state/server.log",
	} {
		if !slices.Contains(args, want) {
			t.Fatalf("systemdRunArgs() missing %q", want)
		}
	}
	if got := args[len(args)-3:]; !slices.Equal(got, []string{"--", "/usr/local/bin/discobox", "server"}) {
		t.Fatalf("systemdRunArgs() command tail = %#v", got)
	}
}
