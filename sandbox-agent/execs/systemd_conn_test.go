package execs

import (
	"context"
	"os"
	"testing"
)

// requireSystemBus skips a test that needs a real D-Bus connection. Everything
// about a connection's lifetime is godbus's behavior rather than this
// package's, so a fake cannot show it — which is exactly how a runner that
// closed its own connection on the way out of every call passed the whole
// suite twice.
func requireSystemBus(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/run/dbus/system_bus_socket"); err != nil {
		t.Skip("no system bus: the sandbox's dbus-daemon is what this exercises")
	}
}

// The context passed to a call must not own the connection. godbus stores the
// context a connection was dialed with and closes the connection when it is
// done, so dialing with anything shorter than the runner's own lifetime — a
// request's context, or a timeout scoped to the dial — hands every later caller
// a connection that is already dead, and leaves Watch subscribed to nothing.
func TestConnectionOutlivesTheCallThatDialedIt(t *testing.T) {
	requireSystemBus(t)
	runner := NewSystemdRunner()
	defer runner.Close()

	ctx, cancel := context.WithCancel(context.Background())
	conn, err := runner.connection(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !conn.Connected() {
		t.Fatal("the connection was already closed when connection() returned")
	}

	// The caller is done. The connection is not.
	cancel()
	if !conn.Connected() {
		t.Fatal("the connection died with the context of the call that dialed it")
	}

	again, err := runner.connection(context.Background())
	if err != nil {
		t.Fatalf("second connect: %v", err)
	}
	if again != conn {
		t.Fatal("a live connection was redialed instead of reused")
	}

	runner.Close()
	if conn.Connected() {
		t.Fatal("Close left the connection open")
	}
	// And a closed runner does not dial a new one.
	if _, err := runner.connection(context.Background()); err == nil {
		t.Fatal("a closed runner dialed a new connection")
	}
}

// A subscription has to survive the call that established it, for the same
// reason — and it is the one that fails quietly, because a closed connection
// stops go-systemd's dispatch loop without closing the channel the watcher
// waits on. The watcher then sits on a live channel that will never carry a
// name, and nothing converges.
//
// The assertion is by identity deliberately. Asking connection() whether it can
// produce a working connection proves nothing: it redials whenever the cached
// one is dead, so it would hand back a healthy replacement while the
// subscription sat on the corpse. What has to hold is that the connection Watch
// subscribed on is still the runner's connection, and still up.
func TestWatchSurvivesItsCallersContext(t *testing.T) {
	requireSystemBus(t)
	runner := NewSystemdRunner()
	defer runner.Close()

	// Dial under a context that then ends, the way a request-scoped call would.
	dialCtx, cancelDial := context.WithCancel(context.Background())
	conn, err := runner.connection(dialCtx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	cancelDial()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := runner.Watch(ctx); err != nil {
		t.Fatalf("watch: %v", err)
	}

	if !conn.Connected() {
		t.Fatal("the connection Watch subscribed on is closed; no unit change can ever arrive")
	}
	again, err := runner.connection(context.Background())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if again != conn {
		t.Fatal("the runner replaced the connection the subscription is on")
	}
}
