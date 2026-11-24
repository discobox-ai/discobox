package provision

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

func TestAwaitFeaturesWaitsForThePlanAndAsyncCompletion(t *testing.T) {
	tracker := NewTracker("")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := tracker.AwaitFeatures(ctx, []string{"core"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("before plan: %v", err)
	}
	tracker.update(func(s *provisionspec.Status) {
		tracker.running = true
		s.Features = map[string]provisionspec.FeatureStatus{
			"core":   {State: provisionspec.StateReady},
			"docker": {State: provisionspec.StateProvisioning, Async: true},
			"brew":   {State: provisionspec.StateOff},
		}
	})
	tracker.ready(nil)
	if err := tracker.AwaitFeatures(context.Background(), []string{"core"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := tracker.AwaitFeatures(ctx, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("all features while async provisioning: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- tracker.AwaitFeatures(context.Background(), nil) }()
	tracker.update(func(s *provisionspec.Status) {
		s.Features["docker"] = provisionspec.FeatureStatus{State: provisionspec.StateReady}
	})
	tracker.finished()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("completion did not wake the waiter")
	}
}

func TestAwaitFeaturesReportsOnlyItsDependencies(t *testing.T) {
	tracker := NewTracker("")
	tracker.update(func(s *provisionspec.Status) {
		s.Features = map[string]provisionspec.FeatureStatus{
			"go":     {State: provisionspec.StateReady},
			"docker": {State: provisionspec.StateFailed, Error: "install failed", Log: "/logs/docker"},
			"brew":   {State: provisionspec.StateOff, Reason: "disabled"},
		}
	})
	tracker.ready(nil)
	for _, tc := range []struct {
		features []string
		problem  string
	}{
		{[]string{"go"}, ""},
		{[]string{"image-feature"}, ""},
		{[]string{"go", "docker"}, "/logs/docker"},
		{[]string{"brew"}, "off"},
		{nil, "/logs/docker"},
	} {
		err := tracker.AwaitFeatures(context.Background(), tc.features)
		if tc.problem == "" && err != nil || tc.problem != "" && (err == nil || !strings.Contains(err.Error(), tc.problem)) {
			t.Errorf("features %v: %v, want %q", tc.features, err, tc.problem)
		}
	}
}

func TestAwaitFeaturesFailsWhenThePlanCannotStart(t *testing.T) {
	tracker := NewTracker("")
	tracker.Fail(errors.New("fetch bundles failed"))
	for _, features := range [][]string{nil, {"go"}} {
		if err := tracker.AwaitFeatures(context.Background(), features); err == nil || !strings.Contains(err.Error(), "fetch bundles failed") {
			t.Fatalf("features %v: %v", features, err)
		}
	}
}

func TestSelectedFeaturesIgnoreUnrelatedReadinessFailure(t *testing.T) {
	tracker := NewTracker("")
	tracker.update(func(s *provisionspec.Status) {
		s.Features["go"] = provisionspec.FeatureStatus{State: provisionspec.StateReady}
		s.Features["other"] = provisionspec.FeatureStatus{State: provisionspec.StateFailed, Error: "unrelated failure"}
	})
	tracker.ready(errors.New("unrelated failure"))
	if err := tracker.AwaitFeatures(context.Background(), []string{"go"}); err != nil {
		t.Fatal(err)
	}
	if err := tracker.AwaitFeatures(context.Background(), nil); err == nil {
		t.Fatal("default service ignored failed provisioning")
	}
}

func TestDefaultFeatureWaitOutlastsReadinessFailure(t *testing.T) {
	tracker := NewTracker("")
	tracker.update(func(s *provisionspec.Status) {
		tracker.running = true
		s.Features["core"] = provisionspec.FeatureStatus{State: provisionspec.StateFailed, Error: "failed"}
		s.Features["other"] = provisionspec.FeatureStatus{State: provisionspec.StateProvisioning, Async: true}
	})
	tracker.ready(errors.New("core failed"))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := tracker.AwaitFeatures(ctx, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("run not finished: %v", err)
	}
	tracker.update(func(s *provisionspec.Status) {
		s.Features["other"] = provisionspec.FeatureStatus{State: provisionspec.StateReady}
	})
	tracker.finished()
	if err := tracker.AwaitFeatures(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "core failed") {
		t.Fatalf("completed failed run: %v", err)
	}
}

// An observer is handed every change, a step within a feature included,
// which the status wait's signal does not move for: a snapshot build streams
// them all (ADR 26-11-23-004 §1).
func TestAnObserverSeesEveryChangeStepsIncluded(t *testing.T) {
	tracker := NewTracker("")
	var seen []provisionspec.Status
	tracker.Observe(func(status provisionspec.Status) { seen = append(seen, status) })
	tracker.update(func(s *provisionspec.Status) {
		tracker.running = true
		s.Features = map[string]provisionspec.FeatureStatus{"docker": {State: provisionspec.StatePending}}
	})
	tracker.step("docker", "install")
	tracker.step("docker", "configure")
	if len(seen) != 3 {
		t.Fatalf("observed %d changes, want 3", len(seen))
	}
	if got := seen[2].Features["docker"]; got.State != provisionspec.StateProvisioning || got.Step != "configure" {
		t.Fatalf("last observed docker = %+v", got)
	}
	// What it was handed is a copy: a later change does not reach it.
	tracker.step("docker", "verify")
	if seen[2].Features["docker"].Step != "configure" {
		t.Fatal("an observed status changed after it was handed over")
	}
}
