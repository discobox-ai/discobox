package bridge_test

import (
	"context"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/proxy/bridge"
)

func newLifecycleForwarder(t *testing.T) *bridge.Forwarder {
	t.Helper()
	prepared := poolMaterial(t)
	forwarder, err := bridge.New(context.Background(), bridge.Config{
		ListenAddress: "127.0.0.1:0",
		Pool:          dialConfig(prepared.Clients["sandbox-1"], unixPool(t, prepared.Bundle), poolServerName),
	})
	if err != nil {
		t.Fatalf("bridge.New: %v", err)
	}
	return forwarder
}

// A Close that lands before ListenAndServe has opened its listener still stops
// it: ListenAndServe must not go on to listen and forward for a closed
// forwarder.
func TestForwarderListenAndServeAfterCloseDoesNotServe(t *testing.T) {
	forwarder := newLifecycleForwarder(t)
	if err := forwarder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- forwarder.ListenAndServe() }()
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		addr := forwarder.Addr()
		t.Fatalf("ListenAndServe on a closed forwarder is still serving on %v", addr)
	}
}

// Addr is polled while ListenAndServe starts; under -race the two must not
// race over the listener.
func TestForwarderAddrWhileListenAndServeStarts(t *testing.T) {
	forwarder := newLifecycleForwarder(t)
	errCh := make(chan error, 1)
	go func() { errCh <- forwarder.ListenAndServe() }()
	deadline := time.Now().Add(5 * time.Second)
	for forwarder.Addr() == nil {
		if time.Now().After(deadline) {
			t.Fatal("Addr() never reported the listener")
		}
		time.Sleep(time.Millisecond)
	}
	if err := forwarder.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("ListenAndServe() error = %v", err)
	}
}
