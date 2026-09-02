package docker

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/moby/moby/client"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

func testLocalDriver(t *testing.T, host string) *LocalDriver {
	t.Helper()
	// Built without a ping: these tests are about what this process can read on
	// its own machine, not about a daemon answering.
	cli, err := client.New(client.WithHost(host))
	if err != nil {
		t.Fatalf("docker client for %s: %v", host, err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return &LocalDriver{client: cli}
}

// A daemon somewhere else keeps its journal there too. Declining says so, with
// the endpoint in the message, rather than streaming this machine's journal as
// if it were the pool host's.
func TestPoolLogsDeclinesRemoteDaemon(t *testing.T) {
	driver := testLocalDriver(t, "tcp://10.0.0.5:2375")
	_, err := driver.PoolLogs(context.Background(), "pool-1", sandbox.PoolLogOptions{})
	if !errors.Is(err, sandbox.ErrPoolLogsUnsupported) {
		t.Fatalf("PoolLogs on a remote daemon = %v, want ErrPoolLogsUnsupported", err)
	}
	if !strings.Contains(err.Error(), "10.0.0.5:2375") {
		t.Fatalf("error %q does not name the daemon it declined", err)
	}
}

// With the daemon on this machine the log is the journal, named as such so an
// operator can tell it apart from a guest console.
func TestPoolLogsReadsTheLocalJournal(t *testing.T) {
	if _, err := exec.LookPath("journalctl"); err != nil {
		t.Skip("journalctl is not installed on this machine")
	}
	driver := testLocalDriver(t, "unix:///var/run/docker.sock")
	stream, err := driver.PoolLogs(context.Background(), "pool-1", sandbox.PoolLogOptions{Tail: 5})
	if err != nil {
		t.Fatalf("PoolLogs: %v", err)
	}
	defer stream.Close()
	if !strings.Contains(stream.Source, "journal") {
		t.Fatalf("source = %q, want the journal named", stream.Source)
	}
	// The content depends on the machine — there may be no Docker unit here at
	// all — but the stream must be readable to its end either way.
	if _, err := io.Copy(io.Discard, stream); err != nil {
		t.Fatalf("read journal: %v", err)
	}
}
