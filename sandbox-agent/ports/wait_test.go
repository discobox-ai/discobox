package ports

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

func httpProbe(context.Context, netip.AddrPort) Protocol { return ProtocolHTTP }

// A caller that names no version, or one that is not current, is answered at
// once: waiting is the caller's to ask for by naming what it already has.
func TestWaitAnswersAtOnceUnlessNamedTheCurrentVersion(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write()
	watcher := New(Config{UID: 1000, Scanner: Procfs{Root: fixture.root}, Probe: httpProbe})
	watcher.tick(context.Background())

	start := time.Now()
	current := watcher.wait(context.Background(), "", time.Hour)
	if current == "" || current != watcher.Version() {
		t.Fatalf("first wait = %q, want the current version %q", current, watcher.Version())
	}
	if got := watcher.wait(context.Background(), "stale", time.Hour); got != current {
		t.Fatalf("wait on a stale version = %q, want %q", got, current)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("answering without a hold took %v", elapsed)
	}
}

// A caller naming the current version is held until the ports change, and is
// answered with the new version as soon as they do.
func TestWaitReturnsWhenAPortAppears(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write()
	watcher := New(Config{UID: 1000, Scanner: Procfs{Root: fixture.root}, Probe: httpProbe})
	watcher.tick(context.Background())
	before := watcher.Version()

	got := make(chan string, 1)
	go func() { got <- watcher.wait(context.Background(), before, time.Hour) }()
	select {
	case v := <-got:
		t.Fatalf("wait returned %q with nothing changed", v)
	case <-time.After(50 * time.Millisecond):
	}

	fixture.write(row(0, "0100007F", "05AF", 1000, 41001))
	watcher.tick(context.Background())
	select {
	case v := <-got:
		if v == before {
			t.Fatalf("wait returned the version it was named, %q", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait did not return when port 1455 appeared")
	}
}

// A tick that finds what the last one did changes nothing: the version holds,
// so the pool is not woken to push the same ports again.
func TestAnUnchangedTickKeepsTheVersion(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write(row(0, "0100007F", "05AF", 1000, 41001))
	watcher := New(Config{UID: 1000, Scanner: Procfs{Root: fixture.root}, Probe: httpProbe})
	watcher.tick(context.Background())
	before := watcher.Version()
	watcher.tick(context.Background())
	if after := watcher.Version(); after != before {
		t.Fatalf("version moved from %q to %q with nothing changed", before, after)
	}
}

// With nothing changing, a held wait answers with the version it was named once
// its hold is up.
func TestWaitAnswersUnchangedWhenItsHoldIsUp(t *testing.T) {
	fixture := newProcFixture(t)
	fixture.write()
	watcher := New(Config{UID: 1000, Scanner: Procfs{Root: fixture.root}, Probe: httpProbe})
	watcher.tick(context.Background())
	before := watcher.Version()
	if got := watcher.wait(context.Background(), before, 20*time.Millisecond); got != before {
		t.Fatalf("wait = %q, want the unchanged %q", got, before)
	}
}

// Two watchers — an agent before and after a restart — never share a version,
// so a pool holding the old one is answered at once by the new.
func TestARestartedWatcherHasANewVersion(t *testing.T) {
	a, b := New(Config{UID: 1000}), New(Config{UID: 1000})
	if a.Version() == b.Version() {
		t.Fatalf("two watchers share version %q", a.Version())
	}
}
