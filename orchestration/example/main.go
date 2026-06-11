package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/obot-platform/disco2/orchestration"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	app, err := NewApp()
	if err != nil {
		log.Fatalf("create app: %v", err)
	}

	if err := app.Dispatcher.Start(ctx); err != nil {
		log.Fatalf("start dispatcher: %v", err)
	}
	defer stopDispatcher(app.Dispatcher)

	sandbox, err := app.Sandboxes.Create(ctx, "project-1", "sandbox-1")
	if err != nil {
		log.Fatalf("accept create intent: %v", err)
	}
	fmt.Printf("accepted create: generation=%d job=%s phase=%s\n", sandbox.Generation, value(sandbox.LastJobID), sandbox.Phase)
	if err := waitForSandboxPhase(ctx, app.Store, sandbox.ID, PhaseRunning); err != nil {
		log.Fatalf("wait create reconcile: %v", err)
	}

	sandbox, err = app.Sandboxes.Stop(ctx, "project-1", "sandbox-1")
	if err != nil {
		log.Fatalf("accept stop intent: %v", err)
	}
	fmt.Printf("accepted stop: generation=%d job=%s phase=%s\n", sandbox.Generation, value(sandbox.LastJobID), sandbox.Phase)
	if err := waitForSandboxPhase(ctx, app.Store, sandbox.ID, PhaseStopped); err != nil {
		log.Fatalf("wait stop reconcile: %v", err)
	}

	sandbox, err = app.Sandboxes.Start(ctx, "project-1", "sandbox-1")
	if err != nil {
		log.Fatalf("accept start intent: %v", err)
	}
	fmt.Printf("accepted start: generation=%d job=%s phase=%s\n", sandbox.Generation, value(sandbox.LastJobID), sandbox.Phase)
	if err := waitForSandboxPhase(ctx, app.Store, sandbox.ID, PhaseRunning); err != nil {
		log.Fatalf("wait start reconcile: %v", err)
	}
}

func stopDispatcher(dispatcher *orchestration.Dispatcher) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := dispatcher.DrainAndStop(ctx); err != nil {
		log.Printf("stop dispatcher: %v", err)
	}
}

func waitForSandboxPhase(ctx context.Context, store *memoryStore, sandboxID string, want SandboxPhase) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		sandbox, err := store.GetSandbox(ctx, sandboxID)
		if err != nil {
			return err
		}
		if sandbox.Phase == want {
			fmt.Printf("reconciled sandbox %s: generation=%d phase=%s\n", sandbox.ID, sandbox.ObservedGeneration, sandbox.Phase)
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func value(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
