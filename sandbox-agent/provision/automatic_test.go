package provision

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/discobox-ai/discobox/pkg/provisionspec"
)

func TestAutomaticReprovisionStopsAfterThreeAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tracker := NewTracker("")
		failure := errors.New("could not fetch bundles")
		runs := make(chan provisionspec.Status, 3)
		tracker.EnableAutomaticRetry(t.Context(), func(status provisionspec.Status) {
			runs <- status
			tracker.Fail(failure)
		})
		tracker.Fail(failure)
		for i, interval := range retryIntervals {
			status := tracker.Status()
			if status.State != provisionspec.StateFailed || status.Retry == nil ||
				status.Retry.Attempt != i+1 || status.Retry.Of != 3 {
				t.Fatalf("scheduled attempt %d: %+v", i+1, status)
			}
			if delay := time.Until(status.Retry.At); delay < interval*4/5 || delay > interval*6/5 {
				t.Fatalf("attempt %d delay = %s", i+1, delay)
			}
			time.Sleep(time.Until(status.Retry.At))
			run := <-runs
			if run.Retry != nil || run.State != provisionspec.StateProvisioning || len(run.Features) != 0 {
				t.Fatalf("started attempt %d: %+v", i+1, run)
			}
			synctest.Wait()
		}
		if status := tracker.Status(); status.Retry != nil || status.State != provisionspec.StateFailed {
			t.Fatalf("exhausted attempts: %+v", status)
		}
	})
}

func TestManualReprovisionReplacesScheduledRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tracker := NewTracker("")
		failure := errors.New("could not fetch bundles")
		tracker.EnableAutomaticRetry(t.Context(), func(provisionspec.Status) {
			t.Error("the superseded timer started a run")
		})
		tracker.Fail(failure)
		first := tracker.Status().Retry
		status, started := tracker.Rerun()
		if first == nil || !started || status.Retry != nil {
			t.Fatalf("manual run did not replace scheduled retry: %+v", status)
		}
		time.Sleep(time.Until(first.At) + time.Second)
		tracker.Fail(failure)
		status = tracker.Status()
		if status.Retry == nil || status.Retry.Attempt != 1 || !status.Retry.At.After(first.At) {
			t.Fatalf("manual run did not reset the schedule: %+v", status)
		}
		if _, started := tracker.Rerun(); !started {
			t.Fatal("could not cancel the new timer")
		}
	})
}
