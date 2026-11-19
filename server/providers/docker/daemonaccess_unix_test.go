//go:build !windows

package docker

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/x/shorttmp"
	"github.com/moby/moby/client"
)

// The failure this exists for: a fresh Docker install leaves the user out of
// the group that owns the socket, so their shell reaches the daemon and the
// server they start does not. The client's own message names neither the group
// that would grant it nor the command that adds it.
func TestSocketAccessHintNamesTheGroupAndTheCommand(t *testing.T) {
	hint := describeSocketAccess("/var/run/docker.sock", 986, false)

	for _, want := range []string{"/var/run/docker.sock", "986", "usermod -aG", "wsl.exe --shutdown"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("hint %q is missing %q", hint, want)
		}
	}
}

// Being in the group and refused anyway is a different problem, and sending
// the reader to add a group they have would send them in a circle.
func TestSocketAccessHintDoesNotTellAMemberToJoin(t *testing.T) {
	hint := describeSocketAccess("/var/run/docker.sock", 986, true)

	if strings.Contains(hint, "usermod") {
		t.Fatalf("hint %q tells a member of the group to join it", hint)
	}
	if !strings.Contains(hint, "already in") {
		t.Fatalf("hint %q does not say the process is already in the group", hint)
	}
}

// The detection itself, against a real socket. The refusal is reproduced by
// dialing rather than read out of the client's message, because moby's client
// builds that message with no %w and the syscall does not survive it.
func TestProbeSocketAccessReportsPermissionDenial(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is refused by no socket mode")
	}
	ctx := context.Background()
	// shorttmp rather than t.TempDir: a Unix socket path is capped at around
	// 108 bytes, and t.TempDir spells this test's own name into a directory
	// under $TMPDIR, which on CI is already long. Binding then fails with
	// EINVAL, which reads as nothing to do with the length.
	path := filepath.Join(shorttmp.Dir(t), "docker.sock")
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	// Reachable: this is not a permission problem, so there is nothing to say.
	if denied, hint := probeSocketAccess(ctx, path); denied || hint != "" {
		t.Fatalf("denied = %t, hint = %q for a socket that dials", denied, hint)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	if denied, hint := probeSocketAccess(ctx, path); !denied || !strings.Contains(hint, path) {
		t.Fatalf("denied = %t, hint = %q, want denial naming %s", denied, hint, path)
	}
	// And a socket that is not there at all is not this explanation either.
	if denied, hint := probeSocketAccess(ctx, filepath.Join(filepath.Dir(path), "missing.sock")); denied || hint != "" {
		t.Fatalf("denied = %t, hint = %q for a missing socket", denied, hint)
	}

	// A parent directory can deny access to the socket and its metadata.
	dir := filepath.Dir(path)
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	}()
	if denied, hint := probeSocketAccess(ctx, path); !denied || hint != "" {
		t.Fatalf("denied = %t, hint = %q, want denial without a group hint", denied, hint)
	}
}

func TestUnixSocketPath(t *testing.T) {
	if got := unixSocketPath("unix:///var/run/docker.sock"); got != "/var/run/docker.sock" {
		t.Fatalf("unixSocketPath() = %q", got)
	}
	for _, host := range []string{"tcp://10.0.0.5:2375", "npipe:////./pipe/docker_engine", ""} {
		if got := unixSocketPath(host); got != "" {
			t.Fatalf("unixSocketPath(%q) = %q, want no path", host, got)
		}
	}
}

func TestLocalDriverCheckRefusesInaccessibleSocket(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can open a socket with mode 000")
	}
	path := filepath.Join(shorttmp.Dir(t), "docker.sock ")
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}

	host := "unix://" + path
	cli, err := client.New(client.WithHost(host))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	for _, check := range []struct {
		name string
		run  func() error
	}{
		{name: "check", run: func() error {
			return (&LocalDriver{client: cli}).Check(t.Context())
		}},
		{name: "build", run: func() error {
			_, err := NewLocalDriver(t.Context(), host, 0)
			return err
		}},
	} {
		t.Run(check.name, func(t *testing.T) {
			err := check.run()
			if err == nil || errors.Is(err, sandbox.ErrProviderNotReachable) || !strings.Contains(err.Error(), "refuses") {
				t.Fatalf("daemon check error = %v, want refusal without ErrProviderNotReachable", err)
			}
		})
	}
}
