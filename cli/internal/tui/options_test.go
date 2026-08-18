package tui

import (
	"strings"
	"testing"
)

// The chip strip names the answers that were given, and an unset harness is not
// one of them. It used to name the project default as though it had been
// chosen, which is a claim the window is not entitled to make: an unset harness
// emits no --harness at all, and what it resolves to is settled at create.
func TestTheChipStripIsSilentAboutAnUnsetHarness(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	st := newStyles(false)

	if chips := m.opts.chips(st); strings.Contains(chips, "claude") {
		t.Fatalf("chips = %q, want nothing about a harness nobody chose", chips)
	}

	// Choosing one is exactly when it earns its place on the strip.
	m.opts.opts[optHarness].idx = 1
	chosen := m.opts.opts[optHarness].display()
	if chips := m.opts.chips(st); !strings.Contains(chips, chosen) {
		t.Fatalf("chips = %q, want the harness that was chosen (%q)", chips, chosen)
	}
}

// "none (shell)" is a choice, not a default, so it is named.
func TestTheChipStripNamesAnExplicitNoHarness(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	harness := m.opts.opts[optHarness]
	harness.idx = len(harness.choices) - 1
	if harness.display() != noHarness {
		t.Fatalf("the last choice is %q, want %q", harness.display(), noHarness)
	}

	if chips := m.opts.chips(newStyles(false)); !strings.Contains(chips, "no harness") {
		t.Fatalf("chips = %q, want the explicit no-harness choice named", chips)
	}
}
