package poolagent

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pool-agent/handover"
)

// An agent that took over sweeps nothing while the agent before it drains:
// that one still holds work this one knows nothing of.
func TestSweepsWaitForTheDrainingAgentToExit(t *testing.T) {
	root := layout.ContainerAt(t.TempDir())
	draining, err := handover.Mark(context.Background(), root.System(handover.AgentDraining))
	if err != nil {
		t.Fatal(err)
	}
	swept := make(chan struct{})
	go sweepsAfterDrain(context.Background(), slog.New(slog.DiscardHandler), root, func() { close(swept) })
	select {
	case <-swept:
		t.Fatal("a sweep ran while the agent before this one drained")
	case <-time.After(200 * time.Millisecond):
	}
	if err := draining.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-swept:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep did not start once the draining agent exited")
	}

	// With no agent draining, it starts at once.
	again := make(chan struct{})
	sweepsAfterDrain(context.Background(), slog.New(slog.DiscardHandler), root, func() { close(again) })
	<-again
}

// An agent that took over serves before its first report, which a failure of
// ends it; one that did not is served by the caller after its report.
func TestAnAgentThatTookOverServesBeforeItsFirstReport(t *testing.T) {
	reported := make(chan struct{})
	served := make(chan struct{})
	err := serveTakingOver(context.Background(), true, func() error {
		<-served
		close(reported)
		return nil
	}, func(context.Context) error {
		close(served)
		<-reported
		return nil
	})
	if err != nil {
		t.Fatalf("serve = %v", err)
	}

	refused := errors.New("the control plane does not hold this pool's key")
	err = serveTakingOver(context.Background(), true, func() error { return refused }, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, refused) {
		t.Fatalf("serve with a refused first report = %v, want the refusal", err)
	}

	calledReport := false
	if err := serveTakingOver(context.Background(), false, func() error { calledReport = true; return nil }, func(context.Context) error { return nil }); err != nil || calledReport {
		t.Fatalf("an agent that did not take over: %v, reported here %v", err, calledReport)
	}
}
