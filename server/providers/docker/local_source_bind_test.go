package docker

import "testing"

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
