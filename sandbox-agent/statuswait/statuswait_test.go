package statuswait

import (
	"context"
	"testing"
	"time"
)

// A caller that names no version, or one that is not current, is answered at
// once: waiting is the caller's to ask for by naming what it already has.
func TestWaitAnswersAtOnceUnlessNamedTheCurrentVersion(t *testing.T) {
	var ports, harness Signal
	start := time.Now()
	current := Wait(context.Background(), "", time.Hour, &ports, &harness)
	if current == "" || current != Version(&ports, &harness) {
		t.Fatalf("first wait = %q, want the current version %q", current, Version(&ports, &harness))
	}
	if got := Wait(context.Background(), "stale", time.Hour, &ports, &harness); got != current {
		t.Fatalf("wait on a stale version = %q, want %q", got, current)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("answering without a hold took %v", elapsed)
	}
}

// A caller naming the current version is held until any one signal changes —
// a port or a harness configuration change (ADR 26-11-09-062) — and answered
// with the new version as soon as it does.
func TestWaitReturnsWhenAnySignalChanges(t *testing.T) {
	for name, which := range map[string]int{"ports": 0, "harness": 1} {
		t.Run(name, func(t *testing.T) {
			signals := []*Signal{{}, {}}
			before := Version(signals...)
			got := make(chan string, 1)
			go func() { got <- Wait(context.Background(), before, time.Hour, signals...) }()
			select {
			case v := <-got:
				t.Fatalf("wait returned %q with nothing changed", v)
			case <-time.After(50 * time.Millisecond):
			}
			signals[which].Changed()
			select {
			case v := <-got:
				if v == before {
					t.Fatalf("wait returned the version it was named, %q", v)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("wait did not return when %s changed", name)
			}
		})
	}
}

// With nothing changing, a held wait answers with the version it was named once
// its hold is up.
func TestWaitAnswersUnchangedWhenItsHoldIsUp(t *testing.T) {
	var s Signal
	before := s.Version()
	if got := Wait(context.Background(), before, 20*time.Millisecond, &s); got != before {
		t.Fatalf("wait = %q, want the unchanged %q", got, before)
	}
}

// Nil signals contribute nothing to wait on; none at all is an empty version,
// which the pool takes for an agent without the wait.
func TestNilSignals(t *testing.T) {
	if got := Wait(context.Background(), "", time.Hour, nil, nil); got != "" {
		t.Fatalf("wait on nil signals = %q, want empty", got)
	}
	var s Signal
	with, without := Version(&s, nil), Version(&s)
	if with == "" || with == without {
		t.Fatalf("Version(s, nil) = %q, Version(s) = %q: want a non-empty, distinct join", with, without)
	}
}

// Two signals — an agent before and after a restart — never share a version.
func TestARestartedSignalHasANewVersion(t *testing.T) {
	var a, b Signal
	if a.Version() == b.Version() {
		t.Fatalf("two signals share version %q", a.Version())
	}
}
