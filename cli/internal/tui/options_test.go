package tui

import (
	"strings"
	"testing"
)

// The chip strip always names the resolved harness.
func TestTheChipStripAlwaysNamesTheResolvedHarness(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	st := newStyles(false)

	if chips := m.opts.chips(st); !strings.Contains(chips, "claude") {
		t.Fatalf("chips = %q, want the resolved default harness", chips)
	}

	// Choosing one is exactly when it earns its place on the strip.
	m.opts.opts[optHarness].idx = 1
	chosen := m.opts.opts[optHarness].display()
	if chips := m.opts.chips(st); !strings.Contains(chips, chosen) {
		t.Fatalf("chips = %q, want the harness that was chosen (%q)", chips, chosen)
	}
}

func TestShiftTabCyclesTheHarnessWithoutOpeningOptions(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	before := m.opts.opts[optHarness].display()

	send(t, m, keyPress("shift+tab"))

	if m.optionsOpen {
		t.Fatal("shift+tab should not open the run options")
	}
	if after := m.opts.opts[optHarness].display(); after == before {
		t.Fatalf("harness = %q, want a choice after %q", after, before)
	}
}

func TestHarnessChipAndMarkerAreMutedForDefaultAndGoldForOverride(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	st := newStyles(true)
	harness := m.opts.opts[optHarness]

	if chips := m.opts.chips(st); !strings.Contains(chips, st.chip.Render(harness.display())) ||
		!strings.Contains(chips, st.chip.Render("⏵⏵ ")) {
		t.Fatalf("default chips = %q, want a muted marker and %q", chips, harness.display())
	}
	harness.cycle(1)
	if chips := m.opts.chips(st); !strings.Contains(chips, st.chipOn.Render(harness.display())) ||
		!strings.Contains(chips, st.chipOn.Render("⏵⏵ ")) {
		t.Fatalf("override chips = %q, want a gold marker and %q", chips, harness.display())
	}
}

func TestHarnessChipStripIsEntirelyMutedWithoutPromptFocus(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	st := newStyles(true)
	m.opts.opts[optDetach].value = "on"

	chips := m.opts.mutedChips(st)
	for _, want := range []string{"⏵⏵ ", "claude", " · ", "detached"} {
		if !strings.Contains(chips, st.chip.Render(want)) {
			t.Fatalf("muted chips = %q, want muted %q", chips, want)
		}
	}
	if strings.Contains(chips, st.chipOn.Render("⏵⏵ ")) || strings.Contains(chips, st.chipOn.Render("claude")) {
		t.Fatalf("muted chips retain an active color: %q", chips)
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
	if chips := m.opts.chips(newStyles(false)); !strings.Contains(chips, unsetHarness) {
		t.Fatalf("chips = %q, want the unresolved harness state", chips)
	}
}

// The strip exists even before an override because the resolved harness is
// always useful context for the prompt.
func TestTheChipStripStartsWithTheHarness(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	st := newStyles(false)

	if chips := m.opts.chips(st); !strings.Contains(chips, "⏵⏵") || !strings.Contains(chips, "claude") {
		t.Fatalf("chips = %q, want the default harness", chips)
	}

	m.opts.opts[optDetach].value = "on"
	if chips := m.opts.chips(st); !strings.Contains(chips, "⏵⏵") || !strings.Contains(chips, "detached") {
		t.Fatalf("chips = %q, want the marker back once there is something to say", chips)
	}
}

// The Source row offers what the project has been cut from, off the listing,
// with the folder the header is on leading and "no source" last. It is a
// selector rather than a text field so the common case — the two or three
// places you work in — never opens one.
func TestTheSourceRowOffersTheProjectsSources(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	source := m.opts.opts[optSource]
	if source.choices[0] != m.opts.sourceLabel() {
		t.Fatalf("leading choice = %q, want the folder the header is on (%q)", source.choices[0], m.opts.sourceLabel())
	}
	if source.changed() {
		t.Fatal("the leading choice is what run would do anyway, so nothing is changed")
	}
	for _, want := range []string{"/src/obot", "https://github.com/acme/foo"} {
		if !hasSourceValue(source, want) {
			t.Fatalf("values = %v, want %q from the listing", source.values, want)
		}
	}
	if last := source.values[len(source.values)-1]; last != sourceNone {
		t.Fatalf("last value = %q, want no source", last)
	}
}

// "No source" is a create with nothing checked out in it, which is one flag
// rather than a source that happens to be empty.
func TestNoSourceAsksForTheFlagAndNoDirectory(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	m.opts.chooseSource(sourceNone)

	req := m.opts.request("do a thing")
	if !req.NoSource {
		t.Fatal("request should ask for --no-source")
	}
	if req.Source != "" {
		t.Fatalf("source = %q, want nothing to cut from", req.Source)
	}
	if cmd := m.opts.command("do a thing"); !strings.Contains(cmd, "run --no-source") || strings.Contains(cmd, "-C") {
		t.Fatalf("command = %q, want --no-source and no -C", cmd)
	}
	if chips := m.opts.chips(newStyles(false)); !strings.Contains(chips, noSourceChoice) {
		t.Fatalf("chips = %q, want the strip to say there is no source", chips)
	}
}

// A source the listing has never seen is typed in, and keeps its place on the
// row until something else is chosen — a refresh of the listing underneath an
// open panel must not drop the one entry the listing could not know about.
func TestATypedSourceSurvivesARefresh(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	m.opts.chooseSource("/src/elsewhere@main")
	m.opts.setSources(m.list.sources())

	if got := m.opts.request("").Source; got != "/src/elsewhere@main" {
		t.Fatalf("source = %q, want the one that was typed", got)
	}
	if got := m.opts.typedSource(); got != "/src/elsewhere@main" {
		t.Fatalf("the input field should reopen holding %q, got %q", "/src/elsewhere@main", got)
	}
}

// The header and the Source row are one control in both directions. Choosing a
// local source moves the list to the folder that source's discoboxes are filed
// under; a remote one and "no source" have no folder of their own, so the list
// goes to the directory the window is running in — which is where a discobox
// from either is filed.
func TestChoosingASourceMovesTheList(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	m.opts.chooseSource("/src/obot")
	m.followSource()
	if m.list.folder != "/src/obot" {
		t.Fatalf("folder = %q, want the list to follow the source", m.list.folder)
	}
	// Followed there, the source is the folder again and emits no -C: the
	// header already says where the next discobox is cut from.
	if req := m.opts.request(""); req.Source != "/src/obot" {
		t.Fatalf("source = %q, want the folder the list moved to", req.Source)
	}

	m.opts.chooseSource("https://github.com/acme/foo")
	m.followSource()
	if m.list.folder != m.session.Directory {
		t.Fatalf("folder = %q, want %q: a remote source has no folder of its own", m.list.folder, m.session.Directory)
	}
	if req := m.opts.request(""); req.Source != "https://github.com/acme/foo" {
		t.Fatalf("source = %q, want the remote repository", req.Source)
	}

	m.opts.chooseSource(sourceNone)
	m.followSource()
	if m.list.folder != m.session.Directory {
		t.Fatalf("folder = %q, want %q: a sourceless discobox is filed where the window runs", m.list.folder, m.session.Directory)
	}
}

func hasSourceValue(o *option, want string) bool {
	for _, value := range o.values {
		if value == want {
			return true
		}
	}
	return false
}
