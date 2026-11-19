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

	"github.com/discobox-ai/x/shorttmp"
	"github.com/moby/moby/client"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
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
func TestSocketAccessSpeaksOnlyForARefusedSocket(t *testing.T) {
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
	if refused, hint := socketAccess(ctx, path); refused || hint != "" {
		t.Fatalf("socketAccess = %t, %q for a socket that dials", refused, hint)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	if refused, hint := socketAccess(ctx, path); !refused || !strings.Contains(hint, path) {
		t.Fatalf("socketAccess = %t, %q, want refused with a hint naming the socket", refused, hint)
	}
	// And a socket that is not there at all is not this explanation either.
	if refused, hint := socketAccess(ctx, filepath.Join(t.TempDir(), "missing.sock")); refused || hint != "" {
		t.Fatalf("socketAccess = %t, %q for a socket that does not exist", refused, hint)
	}
}

// The client's error is what the daemon and the client actually said, so it
// stays at the front of what comes back and a hint that does not apply cannot
// replace it.
func TestExplainDaemonUnreachableKeepsTheOriginalError(t *testing.T) {
	original := errors.New("permission denied while trying to connect to the docker API at unix:///var/run/docker.sock")

	refused, err := explainDaemonUnreachable(context.Background(), "tcp://10.0.0.5:2375", original)

	if refused {
		t.Fatal("a daemon that is not a Unix socket was read as refusing this process")
	}
	if !errors.Is(err, original) {
		t.Fatalf("error %v no longer carries the client's own", err)
	}
	if err.Error() != original.Error() {
		t.Fatalf("error %q was decorated for a daemon that is not a Unix socket", err)
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

// A socket this process may not open answers: the daemon is there and its
// socket refused this process. moby's client reports that as a connection
// failure, the same as a daemon that is not there, so a check that read the
// client's error alone called it unreachable and sent the reader to their
// network (#1470). A socket that is not there at all is still unreachable.
func TestLocalDriverCheckReadsARefusedSocketAsARefusal(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is refused by no socket mode")
	}
	path := filepath.Join(shorttmp.Dir(t), "docker.sock")
	var config net.ListenConfig
	listener, err := config.Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}

	err = unixDriver(t, path).Check(t.Context())
	if err == nil || errors.Is(err, sandbox.ErrProviderNotReachable) || !strings.Contains(err.Error(), "refuses") {
		t.Fatalf("check err = %v, want a refusal, not the provider unreachable", err)
	}

	err = unixDriver(t, filepath.Join(shorttmp.Dir(t), "missing.sock")).Check(t.Context())
	if !errors.Is(err, sandbox.ErrProviderNotReachable) {
		t.Fatalf("check err = %v for a missing socket, want the provider unreachable", err)
	}
}

func unixDriver(t *testing.T, path string) *LocalDriver {
	t.Helper()
	cli, err := client.New(client.WithHost("unix://" + path))
	if err != nil {
		t.Fatalf("new docker client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return &LocalDriver{client: cli}
}
