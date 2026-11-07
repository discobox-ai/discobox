package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func bootstrapPool(b *PoolBootstrap) *Pool {
	data, err := json.Marshal(b)
	if err != nil {
		panic(err)
	}
	return &Pool{ID: "pool-test", Bootstrap: data, ResourceLifecycle: ResourceLifecycle{Generation: b.Generation}}
}

func TestBootstrapWorkOutlastsFiveMinutes(t *testing.T) {
	now := time.Now()
	b := &PoolBootstrap{Attempt: "first", Generation: 1, ProgressAt: now}
	w := &BootstrapWait{}
	for minute := 0; minute < 30; minute++ {
		at := now.Add(time.Duration(minute) * time.Minute)
		b.Accept(BootstrapEvidence{Operation: "image:sha256:one", Bytes: int64(minute + 1)}, at)
		deadline, eligible, err := w.Observe(bootstrapPool(b), at)
		if err != nil || !eligible || !deadline.After(at) {
			t.Fatalf("minute %d: eligible=%t deadline=%s err=%v", minute, eligible, deadline, err)
		}
	}
	b.Accept(BootstrapEvidence{Operation: "apply", Bounded: true}, now.Add(30*time.Minute))
	for minute := 30; minute < 45; minute++ {
		at := now.Add(time.Duration(minute) * time.Minute)
		if _, eligible, err := w.Observe(bootstrapPool(b), at); err != nil || !eligible {
			t.Fatalf("silent apply minute %d: %v", minute, err)
		}
	}
	at := now.Add(45 * time.Minute)
	if _, _, err := w.Observe(bootstrapPool(b), at); err == nil || !strings.Contains(err.Error(), "operation timed out") {
		t.Fatalf("expired apply: %v", err)
	}
}

func TestBootstrapEvidenceCannotReplayOrResetBudgets(t *testing.T) {
	now := time.Now()
	b := &PoolBootstrap{Attempt: "first", Generation: 1, ProgressAt: now}
	b.Accept(BootstrapEvidence{Operation: "image", Bytes: 100}, now)
	marker := b.Marker
	for _, e := range []BootstrapEvidence{{Operation: "image", Bytes: 100}, {Operation: "image", Bytes: 0}, {Operation: "image", Bytes: 80}, {}} {
		b.Accept(e, now.Add(time.Minute))
		if b.Marker != marker {
			t.Fatalf("false progress from %+v", e)
		}
	}
	b.Accept(BootstrapEvidence{Operation: "apply", Bounded: true}, now.Add(time.Minute))
	deadline := b.Deadline
	b.Accept(BootstrapEvidence{Operation: "image", Bytes: 200}, now.Add(2*time.Minute))
	b.Accept(BootstrapEvidence{Operation: "apply", Bounded: true}, now.Add(3*time.Minute))
	if b.Deadline != deadline {
		t.Fatal("replay moved fixed deadline")
	}
	b.Accept(BootstrapEvidence{Operation: "apply", Complete: true, Registration: true}, now.Add(4*time.Minute))
	registration := b.RegistrationDeadline
	b.Accept(BootstrapEvidence{Operation: "apply", Complete: true, Registration: true}, now.Add(5*time.Minute))
	if b.RegistrationDeadline != registration {
		t.Fatal("replay moved registration deadline")
	}
	if reason := b.Expired(now.Add(6 * time.Minute)); reason != "pool agent did not register before timeout" {
		t.Fatal(reason)
	}
}

func TestBootstrapWaitPinsAttemptAndEndsAtFirstRegistration(t *testing.T) {
	now := time.Now()
	b := &PoolBootstrap{Attempt: "first", Generation: 1, ProgressAt: now}
	w := &BootstrapWait{}
	if _, _, err := w.Observe(bootstrapPool(b), now); err != nil {
		t.Fatal(err)
	}
	b.Attempt = "second"
	if _, _, err := w.Observe(bootstrapPool(b), now); err == nil {
		t.Fatal("followed newer attempt")
	}
	b.Attempt = "first"
	pool := bootstrapPool(b)
	pool.RegisteredAt = &now
	if _, eligible, err := w.Observe(pool, now); err != nil || eligible {
		t.Fatalf("registration: %t %v", eligible, err)
	}
	pool.RegisteredAt = nil
	if _, eligible, err := w.Observe(pool, now); err != nil || eligible {
		t.Fatalf("regained entitlement: %t %v", eligible, err)
	}
	if _, eligible, err := (&BootstrapWait{}).Observe(&Pool{ID: "legacy"}, now); err != nil || eligible {
		t.Fatal("legacy entitlement")
	}
}

func TestBootstrapExpiryIsDurableAndOldMarkersDoNotRenew(t *testing.T) {
	now := time.Now()
	b := &PoolBootstrap{Attempt: "first", Generation: 1, ProgressAt: now.Add(-119 * time.Second), Marker: 100}
	w := &BootstrapWait{}
	deadline, _, err := w.Observe(bootstrapPool(b), now)
	if err != nil || !deadline.Equal(now.Add(time.Second)) {
		t.Fatalf("old marker: %s %v", deadline, err)
	}
	b.Accept(BootstrapEvidence{Operation: "apply", Bounded: true}, now.Add(2*time.Second))
	if b.Failure != "bootstrap stopped making progress" {
		t.Fatal("expired attempt restarted", b)
	}
	if _, _, err := (&BootstrapWait{}).Observe(bootstrapPool(b), now.Add(3*time.Second)); err == nil {
		t.Fatal("restart forgot expiry")
	}
}
