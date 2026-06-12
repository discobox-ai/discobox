package model_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/obot-platform/discobox/internal/model"
)

func TestWorkerSchedulingPreferenceFromStatusColumns(t *testing.T) {
	worker := &model.Worker{
		Ready:             true,
		Schedulable:       true,
		ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.WorkerDesiredStateActive},
	}
	if got := worker.SchedulingPreference(); got != model.WorkerSchedulingPreferred {
		t.Fatalf("preference = %q, want preferred", got)
	}

	worker.Degraded = true
	if got := worker.SchedulingPreference(); got != model.WorkerSchedulingDegraded {
		t.Fatalf("preference = %q, want degraded", got)
	}

	worker.Schedulable = false
	if got := worker.SchedulingPreference(); got != model.WorkerSchedulingUnavailable {
		t.Fatalf("preference = %q, want unavailable", got)
	}
}

func TestWorkerSchedulingPreferenceUnavailableWhenDrainingOrRevoked(t *testing.T) {
	worker := &model.Worker{
		Ready:             true,
		Schedulable:       true,
		ResourceLifecycle: model.ResourceLifecycle{DesiredState: model.WorkerDesiredStateDrained},
	}
	if got := worker.SchedulingPreference(); got != model.WorkerSchedulingUnavailable {
		t.Fatalf("draining preference = %q, want unavailable", got)
	}

	now := time.Now().UTC()
	worker.DesiredState = model.WorkerDesiredStateActive
	worker.RevokedAt = &now
	if got := worker.SchedulingPreference(); got != model.WorkerSchedulingUnavailable {
		t.Fatalf("revoked preference = %q, want unavailable", got)
	}
}

func TestWorkerConditionsAreOpaqueJSON(t *testing.T) {
	conditions := json.RawMessage(`{"memoryPressure":{"status":"False"},"message":"display only"}`)
	worker := &model.Worker{Conditions: conditions}
	if string(worker.Conditions) != string(conditions) {
		t.Fatalf("conditions = %s, want %s", worker.Conditions, conditions)
	}
}
