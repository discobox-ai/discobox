//go:build linux

package intake

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A bridge counts as up once its address accepts a connection, which is what a
// Type=simple unit's finished start job does not say.
func TestAwaitListeningWaitsForTheAddress(t *testing.T) {
	// Keep the port bound throughout the test. Closing a listener to obtain an
	// unused address lets another process listen there before the next probe.
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	bound, err := unix.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(bound.(*unix.SockaddrInet4).Port))
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := awaitListening(ctx, address); err == nil {
		t.Fatal("a bound socket that is not listening was taken as up")
	}
	if ctx.Err() == nil {
		t.Fatal("returned before the wait deadline")
	}
	if err := unix.Listen(fd, 1); err != nil {
		t.Fatal(err)
	}
	if err := awaitListening(context.Background(), address); err != nil {
		t.Fatalf("a listening address: %v", err)
	}
}

// Reproduce the old fixture's false positive deterministically: after Close,
// the address belongs to whichever listener binds it next, not to the test.
func TestAwaitListeningReleasedAddressCanBeReused(t *testing.T) {
	// A collision while arranging the reproduction is expected and harmless:
	// retry with a new allocation rather than failing on the race we demonstrate.
	var lc net.ListenConfig
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		original, err := lc.Listen(ctx, "tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := original.Addr().String()
		if err := original.Close(); err != nil {
			t.Fatal(err)
		}
		replacement, err := lc.Listen(ctx, "tcp4", address)
		if errors.Is(err, unix.EADDRINUSE) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		defer replacement.Close()
		if err := awaitListening(ctx, address); err != nil {
			t.Fatalf("a replacement listener on the released address: %v", err)
		}
		return
	}
	t.Fatal("could not arrange address reuse before the deadline")
}
