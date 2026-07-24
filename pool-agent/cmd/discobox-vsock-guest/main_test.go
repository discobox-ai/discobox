package main

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestLifecycleAcceptsShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- serveLifecycle(ctx, listener, func(context.Context) error {
			calls.Add(1)
			return nil
		})
	}()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+listener.Addr().String()+"/shutdown", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("shutdown status = %d", response.StatusCode)
	}
	deadline := time.Now().Add(time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatalf("shutdown calls = %d, want 1", calls.Load())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve lifecycle: %v", err)
	}
}
