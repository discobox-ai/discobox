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

// There is no "none" among the choices: running without a coding harness is
// the `shell` harness, which is one of the project's like any other.
func TestTheHarnessChoicesOfferNoNone(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	for _, choice := range m.opts.opts[optHarness].choices {
		if strings.Contains(strings.ToLower(choice), "none") {
			t.Fatalf("choices = %v, want no none-shaped entry", m.opts.opts[optHarness].choices)
		}
	}
}

// With no project default there is nothing to lead with, so index zero names no
// harness rather than promoting whichever was registered first — which is how
// the strip came to announce one nobody had chosen.
func TestWithNoProjectDefaultNoHarnessIsClaimed(t *testing.T) {
	ds := newFakeSource()
	for i := range ds.harnesses {
		ds.harnesses[i].Default = false
	}
	m := newTestModel(t, ds)

	harness := m.opts.opts[optHarness]
	if harness.choices[0] != unsetHarness {
		t.Fatalf("leading choice = %q, want %q", harness.choices[0], unsetHarness)
	}
	if m.opts.request("").Harness != "" {
		t.Fatal("an unset harness must emit no --harness")
	}
	if chips := m.opts.chips(newStyles(false)); strings.Contains(chips, unsetHarness) {
		t.Fatalf("chips = %q, want nothing for an unset harness", chips)
	}
}

// With nothing chosen the strip has nothing to introduce, so it is not there at
// all. The marker on its own is one more thing on screen that never changes.
func TestTheChipStripIsEmptyUntilSomethingIsChosen(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	st := newStyles(false)

	if chips := m.opts.chips(st); chips != "" {
		t.Fatalf("chips = %q, want an empty line when everything is default", chips)
	}

	m.opts.opts[optDetach].value = "on"
	if chips := m.opts.chips(st); !strings.Contains(chips, "⏵⏵") || !strings.Contains(chips, "detached") {
		t.Fatalf("chips = %q, want the marker back once there is something to say", chips)
	}
}
