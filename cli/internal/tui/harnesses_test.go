package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// F3 opens the harnesses screen from wherever the window is, and closes it
// again.
func TestHarnessesScreenOpensAndCloses(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	send(t, m, key("f3"))
	if !m.harnessesOpen {
		t.Fatal("F3 should open the harnesses screen")
	}
	out := plainFrame(m)
	for _, want := range []string{"Harnesses", "Claude", "Codex", "Custom", "Scratch", "★ default"} {
		if !strings.Contains(out, want) {
			t.Fatalf("harnesses frame is missing %q:\n%s", want, out)
		}
	}

	send(t, m, key("f3"))
	if m.harnessesOpen {
		t.Fatal("F3 should close the harnesses screen it opened")
	}
	if !strings.Contains(plainFrame(m), "Discoboxes") {
		t.Fatal("closing the harnesses screen should put the launcher back")
	}

	// Esc is the other way out, the same as it is everywhere else.
	send(t, m, key("f3"), key("esc"))
	if m.harnessesOpen {
		t.Fatal("Esc should leave the harnesses screen")
	}
}

// The screen opens out of the prompt window, since it is a whole window rather
// than something that fits beside the opening prompt.
func TestHarnessesScreenExpandsTheWindow(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	m.expanded = false
	m.layout()

	send(t, m, key("f3"))
	if !m.expanded {
		t.Fatal("opening the harnesses screen should open the window out")
	}
	if !m.View().AltScreen {
		t.Fatal("the harnesses screen should be on the alternate screen")
	}
}

// WithHarnesses is `disco configure`: the window opens on the screen, already
// out.
func TestWithHarnessesOpensOnTheScreen(t *testing.T) {
	m := New(t.Context(), newFakeSource(), WithHarnesses())
	if !m.harnessesOpen || !m.expanded {
		t.Fatalf("WithHarnesses = {open:%v expanded:%v}, want the window opened out on the screen", m.harnessesOpen, m.expanded)
	}
}

func TestHarnessesCursorMoves(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	send(t, m, key("f3"))

	// Up at the top and down at the bottom stay where they are: the screen is
	// a list, not a carousel.
	last := len(m.harnesses.all) - 1
	send(t, m, key("k"))
	if m.harnesses.cursor != 0 {
		t.Fatalf("cursor = %d, want the top", m.harnesses.cursor)
	}
	send(t, m, key("G"), key("j"))
	if m.harnesses.cursor != last {
		t.Fatalf("cursor = %d, want the last row (%d)", m.harnesses.cursor, last)
	}
	send(t, m, key("g"))
	if m.harnesses.cursor != 0 {
		t.Fatalf("cursor after g = %d, want the top", m.harnesses.cursor)
	}
}

// Enabling hands the terminal to the harness's own setup, and the listing is
// re-read when it comes back.
func TestHarnessesEnableRunsTheSetup(t *testing.T) {
	ds := newFakeSource()
	m := newTestModel(t, ds)
	send(t, m, key("f3"), key("j"), key("e"))

	if len(ds.configured) != 1 || ds.configured[0] != "hc_codex" {
		t.Fatalf("configured = %v, want the harness under the cursor", ds.configured)
	}
	if !strings.Contains(plainFrame(m), "configured Codex") {
		t.Fatalf("the status line should report the setup:\n%s", plainFrame(m))
	}
	if m.busy != "" {
		t.Fatalf("busy = %q, want it cleared once the setup returned", m.busy)
	}
}

// Disabling asks first, since it deletes the secrets and files the setup wrote.
func TestHarnessesDisableConfirms(t *testing.T) {
	ds := newFakeSource()
	m := newTestModel(t, ds)
	send(t, m, key("f3"), key("d"))

	if m.dialog == nil || m.dialog.kind != dlgConfirm {
		t.Fatal("d should ask before disabling")
	}
	if !strings.Contains(m.dialog.body, "project default") {
		t.Fatalf("the question should say the default is released first: %q", m.dialog.body)
	}
	if len(ds.didHarness) != 0 {
		t.Fatalf("did = %v, want nothing done before the question is answered", ds.didHarness)
	}

	// Answering no leaves the harness alone.
	send(t, m, key("n"))
	if len(ds.didHarness) != 0 {
		t.Fatalf("did = %v, want nothing done after answering no", ds.didHarness)
	}

	send(t, m, key("d"), key("y"))
	if len(ds.didHarness) != 1 || ds.didHarness[0] != "disable hc_claude" {
		t.Fatalf("did = %v, want the harness disabled", ds.didHarness)
	}
	if !strings.Contains(plainFrame(m), "disabled Claude") {
		t.Fatalf("the status line should report the disable:\n%s", plainFrame(m))
	}
}

// An action that does not apply says why rather than doing nothing.
func TestHarnessesDisableNeedsAnEnabledHarness(t *testing.T) {
	ds := newFakeSource()
	m := newTestModel(t, ds)
	send(t, m, key("f3"), key("j"), key("j"), key("d"))

	if m.dialog == nil || m.dialog.kind != dlgMessage {
		t.Fatal("disabling a harness that is not enabled should explain itself")
	}
	if len(ds.didHarness) != 0 {
		t.Fatalf("did = %v, want nothing done", ds.didHarness)
	}
}

func TestHarnessesSetDefault(t *testing.T) {
	ds := newFakeSource()
	m := newTestModel(t, ds)
	// The second row is enabled and not the default, which is the only state
	// s applies to.
	send(t, m, key("f3"), key("j"), key("s"))

	if len(ds.didHarness) != 1 || ds.didHarness[0] != "set default hc_codex" {
		t.Fatalf("did = %v, want the default set", ds.didHarness)
	}
	if !strings.Contains(plainFrame(m), "Codex is now the default") {
		t.Fatalf("the status line should report the new default:\n%s", plainFrame(m))
	}

	// The harness that already is the default cannot be made it again.
	send(t, m, key("k"), key("s"))
	if m.dialog == nil || m.dialog.kind != dlgMessage || !strings.Contains(m.dialog.body, "already the default") {
		t.Fatal("setting the default harness as the default should say it already is")
	}
}

// v is the whole configuration: what the harness runs, which secret answers
// each variable it needs, and the files it carries.
func TestHarnessesConfigCard(t *testing.T) {
	ds := newFakeSource()
	ds.secrets = []HarnessSecret{
		//nolint:gosec // These are the names of a variable and of a secret, not a credential.
		{Name: "ANTHROPIC_API_KEY", Required: true, Declared: true,
			SecretID: "sec_1", SecretType: "api_key", SecretName: "anthropic-api-key"},
		{Name: "CLAUDE_CODE_OAUTH_TOKEN", OneOf: "auth", Declared: true},
		{Name: "CUSTOM_TOKEN", SecretID: "sec_2"},
	}
	m := newTestModel(t, ds)
	send(t, m, key("f3"), key("v"))

	if m.dialog == nil || m.dialog.kind != dlgText {
		t.Fatal("v should open the config card")
	}
	body := m.dialog.body
	for _, want := range []string{
		"enabled", "★ default", "built-in", "hc_claude",
		"ghcr.io/example/claude:latest", "claude",
		"ANTHROPIC_API_KEY", "required", "sec_1", "api_key", "anthropic-api-key",
		"CLAUDE_CODE_OAUTH_TOKEN", "one of auth", "nothing bound",
		"CUSTOM_TOKEN", "bound by hand", "sec_2",
		".claude.json", "written by its setup", "settings.json", "from the image",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("config card is missing %q:\n%s", want, body)
		}
	}
}

// f picks a file and hands it to the editor, which reports back whether the
// file changed.
func TestHarnessesEditFile(t *testing.T) {
	ds := newFakeSource()
	ds.editChanged = true
	m := newTestModel(t, ds)
	send(t, m, key("f3"), key("f"))

	if m.dialog == nil || m.dialog.kind != dlgActions {
		t.Fatal("f should open the file picker")
	}
	if len(m.dialog.items) != 2 || m.dialog.items[0].label != ".claude.json" {
		t.Fatalf("files = %+v, want the ones its setup wrote first", m.dialog.items)
	}

	send(t, m, key("enter"))
	if len(ds.editedFiles) != 1 || ds.editedFiles[0] != "hc_claude .claude.json" {
		t.Fatalf("edited = %v, want the chosen file", ds.editedFiles)
	}
	if !strings.Contains(plainFrame(m), "updated .claude.json") {
		t.Fatalf("the status line should report the edit:\n%s", plainFrame(m))
	}

	// An editor that saved nothing says so rather than claiming an update.
	ds.editChanged = false
	send(t, m, key("f"), key("enter"))
	if !strings.Contains(plainFrame(m), ".claude.json unchanged") {
		t.Fatalf("an unchanged file should say so:\n%s", plainFrame(m))
	}
}

// A harness with no files has nothing to edit, and says so.
func TestHarnessesEditNeedsFiles(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	send(t, m, key("f3"), key("G"), key("f"))

	if m.dialog == nil || m.dialog.kind != dlgMessage {
		t.Fatal("f on a harness with no files should explain itself")
	}
}

// The run options' harness choices are the harnesses, with the default leading,
// so enabling one makes it selectable without the window being reopened.
func TestHarnessChoicesFollowTheListing(t *testing.T) {
	ds := newFakeSource()
	m := newTestModel(t, ds)

	harness := m.opts.opts[optHarness]
	want := []string{"claude", "codex", "custom", "scratch", noHarness}
	if strings.Join(harness.choices, ",") != strings.Join(want, ",") {
		t.Fatalf("choices = %v, want %v", harness.choices, want)
	}
	if !strings.Contains(harness.hint, "which is claude") {
		t.Fatalf("hint = %q, want the project default named", harness.hint)
	}

	// A chosen harness survives a refresh of the listing.
	harness.idx = 3
	send(t, m, tickMsg{})
	if got := harness.choices[m.opts.opts[optHarness].idx]; got != "scratch" {
		t.Fatalf("chosen harness = %q, want the one that was chosen", got)
	}

	// Nothing chosen means the default, which is what an empty --harness does.
	m.opts.opts[optHarness].idx = 0
	if m.opts.request("").Harness != "" {
		t.Fatal("the leading choice is the project default and should emit no flag")
	}
}

// Enter on the harness row goes to where harnesses are set up, since that row
// is a choice of harness.
func TestOptionsHarnessRowOpensTheScreen(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	send(t, m, key("shift+tab"))
	if !m.optionsOpen {
		t.Fatal("Shift-Tab should open the run options")
	}
	send(t, m, key("enter"))
	if !m.harnessesOpen || m.optionsOpen {
		t.Fatalf("Enter on the harness row should open the harnesses screen: harnesses=%v options=%v", m.harnessesOpen, m.optionsOpen)
	}
}

// A failed harness's row says why, under the cursor, where there is room for
// it.
func TestHarnessesFailedRowShowsTheError(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	send(t, m, key("f3"), key("G"))
	if !strings.Contains(plainFrame(m), "the setup exited before it finished") {
		t.Fatalf("a failed harness should say why under the cursor:\n%s", plainFrame(m))
	}
	send(t, m, key("g"))
	if strings.Contains(plainFrame(m), "the setup exited before it finished") {
		t.Fatal("the error belongs to the row under the cursor, not to every frame")
	}
}

// The frame is exactly the terminal's height, harnesses screen included: one
// row too many scrolls the terminal, which is the one thing the renderer cannot
// redraw its way out of.
func TestHarnessesFrameFitsTheTerminal(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	for _, size := range [][2]int{{120, 40}, {100, 24}, {80, 20}} {
		send(t, m, sizeMsg(size[0], size[1]))
		if !m.harnessesOpen {
			send(t, m, key("f3"))
		}
		lines := strings.Split(rawFrame(m), "\n")
		if len(lines) != size[1] {
			t.Fatalf("frame at %dx%d is %d rows, want %d", size[0], size[1], len(lines), size[1])
		}
	}
}

// Every action that applies to the row under the cursor is on the hint line.
// Hiding the ones that do not apply is the point of the line, but an action
// that applies and is named nowhere is one nothing would tell you about: `s`
// in particular applies only to a harness that is enabled and not already the
// default, which is the state the fixture's second row is in.
func TestHarnessHintsNameEveryApplicableAction(t *testing.T) {
	ds := newFakeSource()
	m := newTestModel(t, ds)
	send(t, m, key("f3"))
	// 80 columns is the narrow terminal worth holding to: the hint line grows
	// with the actions that apply, and the widest row is the one every action
	// applies to.
	send(t, m, sizeMsg(80, 40))

	for i := range ds.harnesses {
		m.harnesses.moveTo(i)
		harness := *m.harnesses.current()
		hints := m.harnessHints()
		for _, a := range harnessActions(harness) {
			named := strings.Contains(hints, a.key+" "+a.label)
			if a.enabled && !named {
				t.Errorf("%s: %q applies but the hints do not name it: %q", harness.displayName(), a.key, hints)
			}
			if !a.enabled && named {
				t.Errorf("%s: %q does not apply but the hints name it: %q", harness.displayName(), a.key, hints)
			}
		}
		// The line has to fit as written. viewStatus truncates rather than
		// wrapping, so measuring the frame would measure the cut and always
		// pass; what matters is whether there was room for the whole of it.
		if got := lipgloss.Width("  " + hints); got > m.inner() {
			t.Errorf("%s: the hint line needs %d columns of the %d there are: %q",
				harness.displayName(), got, m.inner(), hints)
		}
	}
}

// s is the only action that needs a harness which is enabled and not already
// the default, so it is the one most easily left unreachable.
func TestHarnessHintsOfferTheDefault(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	send(t, m, key("f3"), key("j"))

	if hints := m.harnessHints(); !strings.Contains(hints, "s default") {
		t.Fatalf("hints = %q, want the default offered on an enabled non-default harness", hints)
	}
}
