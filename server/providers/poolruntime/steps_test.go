package poolruntime

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func captureSteps(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

// steppedClock answers each read a second after the last.
func steppedClock() func() time.Time {
	at := time.Unix(0, 0)
	return func() time.Time {
		at = at.Add(time.Second)
		return at
	}
}

// A pass's steps are held until it says it brings the host up, then logged
// at info, each with its own duration and the pass's elapsed time.
func TestHostStepsLogAtInfoOnceTheHostIsBroughtUp(t *testing.T) {
	logged := captureSteps(t)
	ctx := context.Background()
	steps := NewHostSteps("pool-1")
	steps.now = steppedClock()
	steps.began, steps.last = time.Unix(0, 0), time.Unix(0, 0)

	steps.Step(ctx, "the pool's VM is running")
	if logged.Len() != 0 {
		t.Fatalf("a step was logged before the pass said it brings the host up:\n%s", logged)
	}
	steps.BringingUp(ctx)
	steps.Step(ctx, "the pool's Docker daemon is ready")
	steps.Finish(ctx)

	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	want := []string{
		`level=INFO msg="the pool's VM is running" pool=pool-1 duration=1s elapsed=1s`,
		`level=INFO msg="the pool's Docker daemon is ready" pool=pool-1 duration=1s elapsed=2s`,
	}
	if len(lines) != len(want) {
		t.Fatalf("logged %d lines, want %d:\n%s", len(lines), len(want), logged)
	}
	for i := range want {
		if !strings.Contains(lines[i], want[i]) {
			t.Errorf("line %d = %q, want it to contain %q", i, lines[i], want[i])
		}
	}
}

// The steps of a pass that found the host up are logged at debug: the drift
// scan walks them every minute.
func TestHostStepsOfARecheckLogAtDebug(t *testing.T) {
	logged := captureSteps(t)
	ctx := context.Background()
	steps := NewHostSteps("pool-1")
	steps.Step(ctx, "kept the pool's container")
	steps.Finish(ctx)
	if !strings.Contains(logged.String(), `level=DEBUG msg="kept the pool's container" pool=pool-1`) || strings.Contains(logged.String(), "level=INFO") {
		t.Fatalf("a re-check's steps were not logged at debug alone:\n%s", logged)
	}
}
