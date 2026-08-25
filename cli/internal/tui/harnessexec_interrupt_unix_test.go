//go:build !windows

package tui

import (
	"context"
	"errors"
	"io"
	"syscall"
	"testing"
	"time"
)

// Bubble Tea keeps its SIGINT handler installed while it steps aside and drops
// what it receives, and a registered handler is what disables Go's default
// terminate-on-interrupt. So ^C reached nothing at all during a flow that can
// wait minutes on an image pull.
func TestHarnessExecCancelsTheFlowOnInterrupt(t *testing.T) {
	ran := make(chan context.Context, 1)
	exec := &harnessExec{
		ctx: context.Background(),
		run: func(ctx context.Context, _ io.Reader, _, _ io.Writer) error {
			ran <- ctx
			<-ctx.Done()
			return ctx.Err()
		},
	}
	exec.SetStdout(io.Discard)
	exec.SetStderr(io.Discard)

	done := make(chan error, 1)
	go func() { done <- exec.Run() }()

	// The flow is given a context of its own, and interrupting cancels it.
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the flow never started")
	}
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("raise SIGINT: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("interrupting did not stop the flow")
	}
}
