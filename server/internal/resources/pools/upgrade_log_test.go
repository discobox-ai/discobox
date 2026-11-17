package pools

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// captureLog sends the default logger's info records to a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// A restart's server log alone says what the upgrade watch waits on from the
// moment it begins, and when each pool's reconcile settles and how long it took.
func TestUpgradeWaitIsLoggedFromBeginToSettle(t *testing.T) {
	f := newUpgradeFixture(t)
	f.addPool("pool-1", true)
	logged := captureLog(t)

	f.start()
	if !strings.Contains(logged.String(), "pool-1 (pool-1): waiting for its reconcile") {
		t.Errorf("Begin did not log the pools it waits on, with their reason; log:\n%s", logged)
	}

	f.reconcile("pool-1")
	var settle string
	for _, line := range strings.Split(logged.String(), "\n") {
		if strings.Contains(line, "pool=pool-1") && strings.Contains(line, "settled") {
			settle = line
		}
	}
	if settle == "" || !strings.Contains(settle, "level=INFO") || !strings.Contains(settle, "duration=") {
		t.Fatalf("a reconcile that settled logged no info line with its duration; log:\n%s", logged)
	}
}

// linesWith is the log's lines that contain every one of parts.
func linesWith(logged *bytes.Buffer, parts ...string) []string {
	var found []string
	for _, line := range strings.Split(logged.String(), "\n") {
		matches := line != ""
		for _, part := range parts {
			matches = matches && strings.Contains(line, part)
		}
		if matches {
			found = append(found, line)
		}
	}
	return found
}

// The drift scan re-checks every pool every minute: a pass that finds the
// pool as the last one left it and does nothing to it is not logged at info.
func TestPoolReconcileRecheckIsNotLoggedAtInfo(t *testing.T) {
	f := newUpgradeFixture(t)
	f.addPool("pool-1", true)
	f.start()
	f.reconcile("pool-1")
	logged := captureLog(t)
	f.reconcile("pool-1")
	if got := linesWith(logged, "pool reconcile settled"); len(got) != 0 {
		t.Fatalf("a re-check that changed nothing logged at info: %q", got)
	}
}

// A host that is still coming up is waited on over many passes a few seconds
// apart: the wait is logged once when it begins, and the pass that settles
// says how long it was. That pass is not the one that started the pool's
// agent, so its line does not say it did; the provider's step lines do.
func TestPoolReconcileLogsAHostWaitOnceAndItsLength(t *testing.T) {
	f := newUpgradeFixture(t)
	f.addPool("pool-1", true)
	f.useProvider("starts", agentStartingPoolProvider{
		passes:      new(int),
		start:       map[int]sandbox.AgentStartKind{1: sandbox.AgentStartBox},
		unreachable: map[int]bool{1: true, 2: true, 3: true},
	})
	f.start()
	logged := captureLog(t)
	for range 4 {
		f.reconcile("pool-1")
	}
	if got := linesWith(logged, "waiting for the pool's host to come up", "pool=pool-1"); len(got) != 1 {
		t.Fatalf("host wait logged %d times, want once: %q\nlog:\n%s", len(got), got, logged)
	}
	settled := linesWith(logged, "pool reconcile settled", "pool=pool-1", "level=INFO")
	if len(settled) != 1 || !strings.Contains(settled[0], "host_wait=") || !strings.Contains(settled[0], "duration=") || strings.Contains(settled[0], "agent_started") {
		t.Fatalf("the settle after a host wait did not say how long it was; log:\n%s", logged)
	}

	// A later wait is a new one, and is logged again.
	f.useProvider("starts-again", agentStartingPoolProvider{passes: new(int), unreachable: map[int]bool{1: true}})
	f.reconcile("pool-1")
	if got := linesWith(logged, "waiting for the pool's host to come up", "pool=pool-1"); len(got) != 2 {
		t.Fatalf("a new host wait was not logged; log:\n%s", logged)
	}
}
