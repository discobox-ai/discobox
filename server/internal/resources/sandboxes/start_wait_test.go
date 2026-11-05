package sandboxes

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
	services "github.com/discobox-ai/discobox/server/internal/services"
)

func readyStartFixture(t *testing.T) (*Service, *containerProvider) {
	t.Helper()
	svc, provider, db := useFixture(t)
	if err := db.Model(&model.Sandbox{}).Where("id = ?", "sb-1").Updates(map[string]any{"state": model.SandboxStateReady, "error_message": nil}).Error; err != nil {
		t.Fatal(err)
	}
	provider.container.Store(true)
	return svc, provider
}

func TestExplicitStartWaitsForItsPoolAndAdmitsOnce(t *testing.T) {
	svc, p := readyStartFixture(t)
	var epoch int64
	p.startCall = func(_ context.Context, ref sandbox.SandboxRef) error {
		if ref.StartEpoch == nil {
			t.Error("start was not fenced")
		} else if epoch == 0 {
			epoch = *ref.StartEpoch
		} else if epoch != *ref.StartEpoch {
			t.Error("retry canceled wake again")
		}
		if p.starts.Load() < 3 {
			return sandbox.ErrPoolNotReachable
		}
		return nil
	}
	if _, err := svc.StartSandbox(useContext(t), "project-1", "sb-1", services.StartSandboxBody{}); err != nil {
		t.Fatal(err)
	}
	if p.starts.Load() != 3 || storedSandbox(t, svc).WakeEpoch != epoch {
		t.Fatalf("starts=%d epoch=%d", p.starts.Load(), storedSandbox(t, svc).WakeEpoch)
	}
}

func TestLaterInstructionsSupersedeAWaitingStart(t *testing.T) {
	for _, verb := range []string{"stop", "restart", "start", "archive", "purge", "repair", "suspend"} {
		t.Run(verb, func(t *testing.T) {
			svc, p := readyStartFixture(t)
			refused := make(chan struct{})
			var first, announced atomic.Bool
			var firstEpoch atomic.Int64
			laterAdmitted := make(chan struct{})
			p.startCall = func(_ context.Context, ref sandbox.SandboxRef) error {
				if first.CompareAndSwap(false, true) {
					firstEpoch.Store(*ref.StartEpoch)
					close(refused)
				} else if ref.StartEpoch != nil && *ref.StartEpoch != firstEpoch.Load() && announced.CompareAndSwap(false, true) {
					close(laterAdmitted)
				}
				return sandbox.ErrPoolNotReachable
			}
			done := make(chan error, 1)
			go func() {
				_, err := svc.StartSandbox(useContext(t), "project-1", "sb-1", services.StartSandboxBody{})
				done <- err
			}()
			<-refused
			ctx := useContext(t)
			switch verb {
			case "stop":
				_, _ = svc.StopSandbox(ctx, "project-1", "sb-1", services.StopSandboxBody{})
			case "restart":
				_, _ = svc.RestartSandbox(ctx, "project-1", "sb-1", services.RestartSandboxBody{})
			case "start", "repair":
				later, cancel := context.WithCancel(ctx)
				laterDone := make(chan struct{})
				go func() {
					defer close(laterDone)
					if verb == "start" {
						_, _ = svc.StartSandbox(later, "project-1", "sb-1", services.StartSandboxBody{})
					} else {
						_, _ = svc.RepairSandbox(later, "project-1", "sb-1")
					}
				}()
				select {
				case <-laterAdmitted:
				case <-ctx.Done():
					cancel()
					t.Fatal("later instruction was never admitted")
				}
				cancel()
				<-laterDone
			case "archive":
				_ = svc.DeleteSandbox(ctx, "project-1", "sb-1")
				_ = svc.UnarchiveSandbox(ctx, "project-1", "sb-1")
			case "purge":
				_ = svc.PurgeSandbox(ctx, "project-1", "sb-1")
			case "suspend":
				_, _ = svc.SuspendSandbox(ctx, "project-1", "sb-1", services.SuspendSandboxBody{})
			}
			select {
			case err := <-done:
				if err == nil || (verb != "purge" && !strings.Contains(err.Error(), "superseded")) {
					t.Fatalf("old start: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("waiting start survived a later instruction")
			}
		})
	}
}

func TestExplicitStartPoolWaitCancellationAndCeiling(t *testing.T) {
	for _, cancelCaller := range []bool{false, true} {
		t.Run(map[bool]string{false: "ceiling", true: "cancel"}[cancelCaller], func(t *testing.T) {
			svc, p := readyStartFixture(t)
			old := sandboxPoolHostWaitCeiling
			sandboxPoolHostWaitCeiling = 80 * time.Millisecond
			defer func() { sandboxPoolHostWaitCeiling = old }()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p.startCall = func(context.Context, sandbox.SandboxRef) error {
				if cancelCaller {
					cancel()
				}
				return sandbox.ErrPoolNotReachable
			}
			began := time.Now()
			_, err := svc.StartSandbox(ctx, "project-1", "sb-1", services.StartSandboxBody{})
			if cancelCaller {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel: %v", err)
				}
			} else if !errors.Is(err, sandbox.ErrPoolNotReachable) || !strings.Contains(err.Error(), "waiting ended") {
				t.Fatalf("ceiling: %v", err)
			}
			if time.Since(began) > time.Second {
				t.Fatal("deadline overrun")
			}
		})
	}
}

func TestStartPoolWaitBoundsAnUnansweredRetry(t *testing.T) {
	svc, p := readyStartFixture(t)
	old := sandboxPoolHostWaitCeiling
	sandboxPoolHostWaitCeiling = 650 * time.Millisecond
	defer func() { sandboxPoolHostWaitCeiling = old }()
	p.startCall = func(ctx context.Context, _ sandbox.SandboxRef) error {
		if p.starts.Load() == 1 {
			return sandbox.ErrPoolNotReachable
		}
		<-ctx.Done()
		return ctx.Err()
	}
	began := time.Now()
	_, err := svc.StartSandbox(useContext(t), "project-1", "sb-1", services.StartSandboxBody{})
	if !errors.Is(err, sandbox.ErrPoolNotReachable) || time.Since(began) > time.Second {
		t.Fatalf("unanswered retry: %v after %v", err, time.Since(began))
	}
}

func TestFailedSandboxWaitsForPoolThenRepairsItsMissingContainer(t *testing.T) {
	svc, p, _ := useFixture(t)
	p.startCall = func(context.Context, sandbox.SandboxRef) error {
		if p.starts.Load() == 1 {
			return sandbox.ErrPoolNotReachable
		}
		return nil
	}
	if _, err := svc.StartSandbox(useContext(t), "project-1", "sb-1", services.StartSandboxBody{}); err != nil {
		t.Fatal(err)
	}
	if p.creates.Load() != 1 {
		t.Fatalf("repairs = %d", p.creates.Load())
	}
}

func TestSuspendCompletionIsIdempotentAcrossServiceInstances(t *testing.T) {
	svc, _ := readyStartFixture(t)
	other := NewService(svc.store, svc.sandboxProviders, "user-1", svc.engine)
	ctx := useContext(t)
	for _, s := range []*Service{svc, other, svc} {
		if err := s.CompleteSandboxSuspend(ctx, "project-1", "sb-1", "suspend-1", 0); err != nil {
			t.Fatal(err)
		}
	}
	if epoch := storedSandbox(t, svc).WakeEpoch; epoch != 1 {
		t.Fatalf("duplicate completion epoch=%d", epoch)
	}
	if _, err := other.StopSandbox(ctx, "project-1", "sb-1", services.StopSandboxBody{}); err != nil {
		t.Fatal(err)
	}
	if err := svc.CompleteSandboxSuspend(ctx, "project-1", "sb-1", "suspend-2", 2); err != nil {
		t.Fatal(err)
	}
	if err := other.CompleteSandboxSuspend(ctx, "project-1", "sb-1", "suspend-1", 0); err != nil {
		t.Fatal(err)
	}
	if epoch := storedSandbox(t, svc).WakeEpoch; epoch != 3 {
		t.Fatalf("late old completion epoch=%d", epoch)
	}
}

func TestFirstDelayedSuspendCompletionPreservesANewerStart(t *testing.T) {
	svc, _ := readyStartFixture(t)
	if _, err := svc.StopSandbox(useContext(t), "project-1", "sb-1", services.StopSandboxBody{}); err != nil {
		t.Fatal(err)
	}
	if err := svc.CompleteSandboxSuspend(useContext(t), "project-1", "sb-1", "delayed-suspend", 0); err != nil {
		t.Fatal(err)
	}
	if epoch := storedSandbox(t, svc).WakeEpoch; epoch != 1 {
		t.Fatalf("delayed completion canceled newer instruction: epoch=%d", epoch)
	}
}
