package docker

import (
	"slices"
	"testing"

	"github.com/discobox-ai/discobox/server/providers/dockerworker"
)

func TestLocalSourceBindSupported(t *testing.T) {
	tests := []struct {
		daemonHost string
		want       bool
		why        string
	}{
		{"unix:///var/run/docker.sock", true, "a socket means the daemon is on this machine"},
		{"unix:///Users/darren/.docker/run/docker.sock", true, "Docker Desktop shares host paths into its VM"},
		{"npipe:////./pipe/docker_engine", true, "the Windows socket equivalent"},
		{"ssh://user@remote", false, "a remote daemon cannot see these files"},
		{"tcp://10.0.0.5:2375", false, "a remote daemon cannot see these files"},
		// Local only by appearance: the port may be forwarded anywhere, and a
		// wrong true binds a path the daemon cannot resolve.
		{"tcp://localhost:2375", false, "a forwarded port proves nothing about the daemon's filesystem"},
		{"tcp://127.0.0.1:2375", false, "a forwarded port proves nothing about the daemon's filesystem"},
		{"", false, "an unknown daemon is not assumed to be local"},
		{"/var/run/docker.sock", false, "a bare path carries no transport to classify"},
	}
	for _, tc := range tests {
		t.Run(tc.daemonHost, func(t *testing.T) {
			if got := localSourceBindSupported(tc.daemonHost); got != tc.want {
				t.Fatalf("localSourceBindSupported(%q) = %t, want %t: %s", tc.daemonHost, got, tc.want, tc.why)
			}
		})
	}
}

// The roots are the host mounts, and only when the daemon is one this process
// shares a filesystem with. A pool worker sees a host directory only if it was
// mounted for it, so a directory outside them is as unreachable as one on
// another machine — and the source has to be pushed instead of cloned.
func TestLocalSourceRoots(t *testing.T) {
	mounts := []dockerworker.HostMount{{Source: "/home", ReadOnly: true}, {Source: "/Users"}}

	tests := []struct {
		name       string
		daemonHost string
		hostMounts []dockerworker.HostMount
		want       []string
		why        string
	}{
		{
			name: "a local daemon reaches what it mounts", daemonHost: "unix:///var/run/docker.sock",
			hostMounts: mounts, want: []string{"/home", "/Users"},
			why: "these are the paths the worker is given",
		},
		{
			name: "a local daemon with no mounts reaches nothing", daemonHost: "unix:///var/run/docker.sock",
			hostMounts: nil, want: nil,
			why: "sharing a filesystem is not seeing it: nothing was mounted in",
		},
		{
			name: "a remote daemon reaches nothing it is configured with", daemonHost: "ssh://user@remote",
			hostMounts: mounts, want: nil,
			why: "the mounts name paths on a machine these files are not on",
		},
		{
			name: "an empty mount source names no path", daemonHost: "unix:///var/run/docker.sock",
			hostMounts: []dockerworker.HostMount{{Source: "  "}}, want: nil,
			why: "a blank source is not a root",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := localSourceRoots(tc.daemonHost, tc.hostMounts)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("localSourceRoots = %v, want %v: %s", got, tc.want, tc.why)
			}
		})
	}
}
