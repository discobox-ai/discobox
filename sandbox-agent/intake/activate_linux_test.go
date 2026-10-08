//go:build linux

package intake

import (
	"context"
	"net"
	"testing"
	"time"
)

// A bridge counts as up once its address accepts a connection, which is what a
// Type=simple unit's finished start job does not say.
func TestAwaitListeningWaitsForTheAddress(t *testing.T) {
	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := awaitListening(context.Background(), listener.Addr().String()); err != nil {
		t.Fatalf("a listening address: %v", err)
	}
	closed := listener.Addr().String()
	_ = listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := awaitListening(ctx, closed); err == nil {
		t.Fatal("an address nothing listens on was taken as up")
	}
}
