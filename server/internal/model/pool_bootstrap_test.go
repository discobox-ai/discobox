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

func TestBootstrapAttemptEndsOnFailureOrNewGeneration(t *testing.T) {
	b := NewPoolBootstrap(3, time.Now().Add(-time.Hour))
	if b.Ended(3) {
		t.Fatal("an expired attempt with no recorded failure has not ended: the pass that sees it records why")
	}
	if !b.Ended(4) {
		t.Fatal("an attempt for an earlier generation has ended")
	}
	b.Failure = "image not found"
	if !b.Ended(3) {
		t.Fatal("a failed attempt has ended")
	}
	if NewPoolBootstrap(3, time.Now()).Attempt == b.Attempt {
		t.Fatal("attempts share an identity")
	}
}

// A waiter pinned to an attempt never follows the attempt that replaces it;
// one that arrives after the replacement waits on the new attempt.
func TestBootstrapWaitNeverFollowsANewAttempt(t *testing.T) {
	now := time.Now()
	first := NewPoolBootstrap(1, now)
	w := &BootstrapWait{}
	if _, eligible, err := w.Observe(bootstrapPool(&first), now); err != nil || !eligible {
		t.Fatalf("first attempt: eligible=%t err=%v", eligible, err)
	}
	first.Failure = "image not found"
	if _, _, err := w.Observe(bootstrapPool(&first), now); err == nil || !strings.Contains(err.Error(), "image not found") {
		t.Fatalf("failed attempt: %v", err)
	}
	second := NewPoolBootstrap(1, now.Add(time.Minute))
	if _, _, err := w.Observe(bootstrapPool(&second), now.Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "bootstrap attempt changed") {
		t.Fatalf("pinned waiter on a new attempt: %v", err)
	}
	late := &BootstrapWait{}
	deadline, eligible, err := late.Observe(bootstrapPool(&second), now.Add(time.Minute))
	if err != nil || !eligible || !deadline.After(now.Add(time.Minute)) {
		t.Fatalf("waiter arriving during the new attempt: deadline=%s eligible=%t err=%v", deadline, eligible, err)
	}
	if late.Attempt != second.Attempt {
		t.Fatal("late waiter not pinned to the new attempt")
	}
}

func TestNextAttemptOwesARepairAndCountsFailures(t *testing.T) {
	now := time.Now()
	b := NewPoolBootstrap(2, now)
	b.Accept(BootstrapEvidence{Failure: "image not found"}, now.Add(time.Second))
	if !b.EndedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("EndedAt = %s, want when the failure was recorded", b.EndedAt)
	}
	next := b.Next(2, now.Add(time.Minute))
	if next.Attempt == b.Attempt || next.Failed != 1 || !strings.Contains(next.Repair, "image not found") || !next.ProgressAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("next = %+v", next)
	}
	if again := next.Next(2, now); again.Failed != 2 {
		t.Fatalf("failures before the third attempt = %d, want 2", again.Failed)
	}
	if fresh := next.Next(3, now); fresh.Failed != 0 || fresh.Repair == "" {
		t.Fatalf("attempt for new intent = %+v, want the count started over and a repair owed", fresh)
	}

	marker := next.Marker
	next.Accept(BootstrapEvidence{Repaired: true}, now.Add(time.Minute))
	if next.Repair != "" || next.Marker != marker {
		t.Fatalf("after the repair: owed %q, marker %d -> %d; want it cleared and no progress", next.Repair, marker, next.Marker)
	}
}

func TestExpiryRecordsWhenTheAttemptEnded(t *testing.T) {
	now := time.Now()
	b := NewPoolBootstrap(1, now)
	at := now.Add(PoolBootstrapSilence)
	b.Accept(BootstrapEvidence{Operation: "image", Bytes: 1}, at)
	if b.Failure == "" || !b.EndedAt.Equal(at) {
		t.Fatalf("failure %q ended %s, want expiry recorded at %s", b.Failure, b.EndedAt, at)
	}
}
