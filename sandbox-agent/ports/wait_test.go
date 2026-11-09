package ports

import (
	"context"
	"net/netip"
	"testing"
)

func httpProbe(context.Context, netip.AddrPort) Protocol { return ProtocolHTTP }

// A port appearing moves the snapshot's version, which is what wakes a status
// wait held on it (ADR 26-11-03-815 §2).
func TestAPortAppearingMovesTheVersion(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write()
	watcher := New(Config{UID: 1000, Scanner: Procfs{Root: fixture.root}, Probe: httpProbe})
	watcher.tick(context.Background())
	before := watcher.Changes().Version()

	fixture.write(row(0, "0100007F", "05AF", 1000, 41001))
	watcher.tick(context.Background())
	if after := watcher.Changes().Version(); after == before {
		t.Fatalf("version stayed %q when port 1455 appeared", after)
	}
}

// A tick that finds what the last one did changes nothing: the version holds,
// so the pool is not woken to push the same ports again.
func TestAnUnchangedTickKeepsTheVersion(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write(row(0, "0100007F", "05AF", 1000, 41001))
	watcher := New(Config{UID: 1000, Scanner: Procfs{Root: fixture.root}, Probe: httpProbe})
	watcher.tick(context.Background())
	before := watcher.Changes().Version()
	watcher.tick(context.Background())
	if after := watcher.Changes().Version(); after != before {
		t.Fatalf("version moved from %q to %q with nothing changed", before, after)
	}
}

// Two watchers — an agent before and after a restart — never share a version,
// so a pool holding the old one is answered at once by the new.
func TestARestartedWatcherHasANewVersion(t *testing.T) {
	a, b := New(Config{UID: 1000}), New(Config{UID: 1000})
	if a.Changes().Version() == b.Changes().Version() {
		t.Fatalf("two watchers share version %q", a.Changes().Version())
	}
}
