package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The call to action is in the middle of the band, not at the end of its
// sentence: the bar is the width of the window, and the right end of a bar that
// wide is where the eye goes last.
func TestTheBandCentresItsCallToAction(t *testing.T) {
	t.Parallel()
	st := newStyles(false)
	const width = 120
	row := ansi.Strip(bannerRow(st, width, st.readyMark, "⇡", "ready to apply", "click to apply", "ctrl+a y", colReadyBG))

	if got := lipgloss.Width(row); got != width {
		t.Fatalf("band is %d cells wide, want %d: %q", got, width, row)
	}
	at := strings.Index(row, "click to apply")
	if at < 0 {
		t.Fatalf("band = %q, want the call on it", row)
	}
	// Centered in the row itself rather than in the gap between the sentence
	// and the key, so it holds still as the sentence changes length. Measured
	// in cells: the mark in front of it is one cell and three bytes.
	middle := lipgloss.Width(row[:at]) + lipgloss.Width("click to apply")/2
	if middle < width/2-1 || middle > width/2+1 {
		t.Fatalf("the call is centered on cell %d of %d: %q", middle, width, row)
	}
	// The key is still pinned to the end, which is the half of the bar a
	// keyboard reads.
	if !strings.HasSuffix(strings.TrimRight(row, " "), "ctrl+a y") {
		t.Fatalf("band = %q, want the key pinned to the right", row)
	}
}

// A band too narrow for all three drops the call whole. A chip reading "click
// to ap…" is a button with a typo on it, and what a narrow bar must still carry
// is what is waiting and what to press about it.
func TestANarrowBandDropsTheCallRatherThanCuttingIt(t *testing.T) {
	t.Parallel()
	st := newStyles(false)
	const width = 34
	row := ansi.Strip(bannerRow(st, width, st.attentionMark, "⚠", "credential request  ·  gh", "click to answer", "ctrl+a g", colAlertBG))

	if got := lipgloss.Width(row); got != width {
		t.Fatalf("band is %d cells wide, want %d: %q", got, width, row)
	}
	if strings.Contains(row, "click") {
		t.Fatalf("band = %q, want the call gone rather than cut", row)
	}
	for _, want := range []string{"⚠", "credential", "ctrl+a g"} {
		if !strings.Contains(row, want) {
			t.Fatalf("band = %q, want it to keep %q", row, want)
		}
	}
	// The key's "or" is the other half of the chip. With the chip gone it
	// would be an alternative to nothing.
	if strings.Contains(row, " or ") {
		t.Fatalf("band = %q, want no \"or\" once the call is gone", row)
	}
}

// A subject long enough to reach the chip is cut back for it, and still stops
// two cells short: an ellipsis that runs into the chip reads as part of it.
func TestACutSubjectKeepsItsDistanceFromTheCall(t *testing.T) {
	t.Parallel()
	st := newStyles(false)
	const width = 50
	subject := "credential request  ·  a token with a very long name for somewhere"
	row := ansi.Strip(bannerRow(st, width, st.attentionMark, "⚠", subject, "click to answer", "ctrl+a g", colAlertBG))

	if got := lipgloss.Width(row); got != width {
		t.Fatalf("band is %d cells wide, want %d: %q", got, width, row)
	}
	if !strings.Contains(row, "…  click to answer") {
		t.Fatalf("band = %q, want the cut subject two cells short of the call", row)
	}
	if !strings.HasSuffix(strings.TrimRight(row, " "), "or  ctrl+a g") {
		t.Fatalf("band = %q, want the key pinned to the right", row)
	}
}

// The request's call throbs. It is the only thing in the window that moves
// without somebody having done something, and it moves because it is the only
// thing on screen with an agent stopped behind it.
func TestTheRequestsCallThrobs(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithRequest(t)
	m.st = newStyles(true)
	m.paneBox = Sandbox{ID: "sbx_one", Name: "one"}

	beats := make([]string, len(bannerPulseHues))
	for beat := range bannerPulseHues {
		m.pulse = beat
		beats[beat] = m.viewCredentialBanner(120)
		if field := "48;5;" + bannerPulseHues[beat]; !strings.Contains(beats[beat], field) {
			t.Fatalf("beat %d does not carry %q:\n%q", beat, field, beats[beat])
		}
		// A chip that dropped to the band's own field would be a bar blinking
		// on and off rather than a button asking to be pressed.
		if bannerPulseHues[beat] == colAlertBG {
			t.Fatalf("beat %d is the band's own color: the bar blinks", beat)
		}
	}
	if beats[0] == beats[1] {
		t.Fatalf("two beats painted the same band:\n%q", beats[0])
	}
	// The beat comes back round: it is a heartbeat, not a fade that ends.
	m.pulse = len(bannerPulseHues)
	if again := m.viewCredentialBanner(120); again != beats[0] {
		t.Fatalf("the throb does not repeat:\n%q\n%q", beats[0], again)
	}
}

// The clock behind the throb runs while the band is up and stops with it: one
// left running would be a window that never idles, and a band with none is a
// button that sits still while somebody waits.
func TestTheThrobsClockRunsOnlyWhileTheBandIsUp(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithRequest(t)
	m.st = newStyles(true)
	m.paneBox = Sandbox{ID: "sbx_one", Name: "one"}
	// A tool window is a workspace as far as the band is concerned: the bar is
	// drawn over whatever the screen is showing.
	m.toolOpen = true

	if cmd := m.armBannerPulse(); cmd == nil || !m.pulsing {
		t.Fatal("no clock started behind the request's band")
	}
	if cmd := m.armBannerPulse(); cmd != nil {
		t.Fatal("a second clock started on a band that already had one")
	}
	run := m.pulseGen
	if cmd := m.advanceBannerPulse(bannerPulseMsg{gen: run}); cmd == nil || m.pulse != 1 {
		t.Fatalf("a beat left the throb on frame %d with no next beat asked for", m.pulse)
	}
	if cmd := m.advanceBannerPulse(bannerPulseMsg{gen: run - 1}); cmd != nil || m.pulse != 1 {
		t.Fatalf("a beat from an older run was answered: frame %d", m.pulse)
	}

	// Answered: the band goes, and the clock goes with it.
	m.requests = nil
	if cmd := m.armBannerPulse(); cmd != nil {
		t.Fatal("the clock asked for another beat after the band went")
	}
	if m.pulsing || m.pulse != 0 {
		t.Fatalf("pulsing = %v on frame %d, want the throb stopped and reset", m.pulsing, m.pulse)
	}
	if cmd := m.advanceBannerPulse(bannerPulseMsg{gen: run}); cmd != nil || m.pulse != 0 {
		t.Fatalf("a beat left in flight restarted the throb: frame %d", m.pulse)
	}

	// And the offer's band gets none: work that is ready will still be ready in
	// a minute, and a screen with two things moving on it has nothing that
	// stands out.
	ready := readySandboxes()
	m.list.setAll(ready)
	m.paneBox = ready[0]
	if m.bannerShowing() != bannerApply {
		t.Fatalf("banner = %v, want the offer", m.bannerShowing())
	}
	if cmd := m.armBannerPulse(); cmd != nil || m.pulsing {
		t.Fatal("the offer's band got a clock")
	}
}
