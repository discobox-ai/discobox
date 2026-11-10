package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	tea "charm.land/bubbletea/v2"

	"charm.land/lipgloss/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// The harness config card is the other card the window puts up to be read, so
// it takes the same c as the help — with the window's own color stripped off on
// the way to the clipboard, because what belongs there is the text.
func TestTheConfigCardIsCopiedWithoutItsColor(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	copies := make(chan string, 1)
	m.copyOS = func(text string) error { copies <- text; return nil }

	body := "\x1b[32mImage\x1b[0m  ghcr.io/discobox/claude:1"
	send(t, m, sizeMsg(100, 30), harnessCardMsg{title: "claude", body: body})
	if m.dialog == nil {
		t.Fatal("the card did not go up")
	}
	send(t, m, keyPress("c"))
	select {
	case got := <-copies:
		if want := "Image  ghcr.io/discobox/claude:1"; got != want {
			t.Fatalf("copied %q, want %q", got, want)
		}
	default:
		t.Fatal("c did not copy the config card")
	}
}

// The window's own keys are answered over the run options panel: the panel's
// harness row names F3 in its own hint, and the panel is the one surface with
// no other way to the key reference.
func TestTheWindowKeysReachOverTheRunOptions(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	send(t, m, sizeMsg(100, 30), keyPress("ctrl+o"))
	if !m.optionsOpen {
		t.Fatal("ctrl+o should open the run options from the prompt")
	}
	send(t, m, keyPress("f1"))
	if m.dialog == nil {
		t.Fatal("F1 should reach the help over the options panel")
	}
	send(t, m, keyPress("esc"))
	if !m.optionsOpen {
		t.Fatal("closing the help should leave the panel where it was")
	}
	send(t, m, keyPress("f3"))
	if !m.harnessesOpen || m.optionsOpen {
		t.Fatalf("F3 should open the harnesses and put the panel away: harnesses=%v options=%v",
			m.harnessesOpen, m.optionsOpen)
	}

	// And the same key back the other way, from a screen that is up. The panel
	// is the screens' peer, not a layer over one: it puts them away as they put
	// it away, so nothing is left standing under it holding its own key.
	send(t, m, keyPress("ctrl+o"))
	if !m.optionsOpen || m.harnessesOpen {
		t.Fatalf("ctrl+o should open the panel and put the harnesses away: options=%v harnesses=%v",
			m.optionsOpen, m.harnessesOpen)
	}
	// One press, not two: with the harnesses left open underneath, F3 here
	// would close a screen nothing could see and change no frame at all —
	// which is the key the panel's own harness row advertises.
	send(t, m, keyPress("f3"))
	if !m.harnessesOpen || m.optionsOpen {
		t.Fatalf("F3 over the panel should open the harnesses in one press: harnesses=%v options=%v",
			m.harnessesOpen, m.optionsOpen)
	}

	send(t, m, keyPress("ctrl+o"))
	send(t, m, keyPress("ctrl+o"))
	if m.optionsOpen {
		t.Fatal("ctrl+o should close the panel it opened")
	}
}

// F3 opens the harnesses screen from wherever the window is, and closes it
// again.
func TestHarnessesScreenOpensAndCloses(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	send(t, m, keyPress("f3"))
	if !m.harnessesOpen {
		t.Fatal("F3 should open the harnesses screen")
	}
	out := plainFrame(m)
	for _, want := range []string{"Harnesses", "Claude", "Codex", "Custom", "Scratch", "★ default"} {
		if !strings.Contains(out, want) {
			t.Fatalf("harnesses frame is missing %q:\n%s", want, out)
		}
	}

	send(t, m, keyPress("f3"))
	if m.harnessesOpen {
		t.Fatal("F3 should close the harnesses screen it opened")
	}
	// The launcher is the whole window again: the tree is drawn only beside
	// a page or a workspace, and it lists the discoboxes too.
	if m.treeShown() || !strings.Contains(plainFrame(m), "Discoboxes") {
		t.Fatal("closing the harnesses screen should put the launcher back")
	}

	// Esc is the other way out, the same as it is everywhere else.
	send(t, m, keyPress("f3"), keyPress("esc"))
	if m.harnessesOpen {
		t.Fatal("Esc should leave the harnesses screen")
	}
}

// WithHarnesses is `discobox configure`: the window opens on the screen.
func TestWithHarnessesOpensOnTheScreen(t *testing.T) {
	t.Parallel()
	m := New(t.Context(), newFakeSource(), WithHarnesses())
	if !m.harnessesOpen {
		t.Fatal("WithHarnesses should open the window on the harnesses screen")
	}
}

func TestHarnessesCursorMoves(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	send(t, m, keyPress("f3"))

	// Up at the top and down at the bottom stay where they are: the screen is
	// a list, not a carousel.
	last := len(m.harnesses.all) - 1
	send(t, m, keyPress("k"))
	if m.harnesses.cursor != 0 {
		t.Fatalf("cursor = %d, want the top", m.harnesses.cursor)
	}
	send(t, m, keyPress("G"), keyPress("j"))
	if m.harnesses.cursor != last {
		t.Fatalf("cursor = %d, want the last row (%d)", m.harnesses.cursor, last)
	}
	send(t, m, keyPress("g"))
	if m.harnesses.cursor != 0 {
		t.Fatalf("cursor after g = %d, want the top", m.harnesses.cursor)
	}
}

// Reset asks first, since it deletes the settings and logins saved to the
// harness, and nothing brings those back but signing in and saving again.
func TestHarnessesResetConfirms(t *testing.T) {
	t.Parallel()
	ds := newFakeSource()
	m := newTestModel(t, ds)
	send(t, m, keyPress("f3"), keyPress("r"))

	if m.dialog == nil || m.dialog.kind != dlgConfirm {
		t.Fatal("r should ask before resetting")
	}
	for _, want := range []string{"bundle's defaults", "logins saved", "bound to it by hand are kept"} {
		if !strings.Contains(m.dialog.body, want) {
			t.Fatalf("the question should say %q: %q", want, m.dialog.body)
		}
	}
	if len(ds.didHarness) != 0 {
		t.Fatalf("did = %v, want nothing done before the question is answered", ds.didHarness)
	}

	// Answering no leaves the harness alone.
	send(t, m, keyPress("n"))
	if len(ds.didHarness) != 0 {
		t.Fatalf("did = %v, want nothing done after answering no", ds.didHarness)
	}

	send(t, m, keyPress("r"), keyPress("y"))
	if len(ds.didHarness) != 1 || ds.didHarness[0] != "reset hc_claude" {
		t.Fatalf("did = %v, want the harness reset", ds.didHarness)
	}
	if !strings.Contains(plainFrame(m), "reset Claude to its bundle's defaults") {
		t.Fatalf("the status line should report the reset:\n%s", plainFrame(m))
	}
}

// A harness with files a person added or overrode asks which reset is meant:
// a plain one keeps those files, and is offered first; the other leaves
// nothing but the bundle's (ADR 26-11-04-711 §1).
func TestResettingAHarnessWithYourFilesAsksWhetherToKeepThem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		key, want string
	}{
		{"r", "reset hc_claude"},
		{"a", "reset with your files hc_claude"},
	} {
		ds := newFakeSource()
		ds.harnesses[0].Files = append(ds.harnesses[0].Files, console.HarnessFile{Path: ".config/x.txt", Content: "mine", Source: console.HarnessFileOverride})
		m := newTestModel(t, ds)
		send(t, m, keyPress("f3"), keyPress("r"))
		if m.dialog == nil || m.dialog.kind != dlgActions || m.dialog.items[0].key != "r" {
			t.Fatalf("dialog = %s, want keeping your files offered first", describe(m.dialog))
		}
		send(t, m, keyPress(tc.key))
		if len(ds.didHarness) != 1 || ds.didHarness[0] != tc.want {
			t.Fatalf("pressing %s did %v, want %q", tc.key, ds.didHarness, tc.want)
		}
	}
}

// Every harness can be run, so nothing on the screen offers to set one up.
func TestHarnessesOfferNoSetup(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	send(t, m, keyPress("f3"))
	for i := range m.harnesses.all {
		for _, a := range harnessActions(m.harnesses.all[i]) {
			if a.key == "e" || a.key == "d" || strings.Contains(a.label, "enable") || strings.Contains(a.label, "configure") {
				t.Fatalf("%s offers %q (%s), want no setup action", m.harnesses.all[i].DisplayName(), a.key, a.label)
			}
		}
	}
	// Enter reads the harness rather than starting anything.
	send(t, m, keyPress("enter"))
	if m.dialog == nil || m.dialog.kind != dlgText {
		t.Fatal("Enter should open the config card")
	}
}

func TestHarnessesSetDefault(t *testing.T) {
	t.Parallel()
	ds := newFakeSource()
	m := newTestModel(t, ds)
	// The second row is not the default, which is the only state s applies to.
	send(t, m, keyPress("f3"), keyPress("j"), keyPress("s"))

	if len(ds.didHarness) != 1 || ds.didHarness[0] != "set default hc_codex" {
		t.Fatalf("did = %v, want the default set", ds.didHarness)
	}
	if !strings.Contains(plainFrame(m), "Codex is now the default") {
		t.Fatalf("the status line should report the new default:\n%s", plainFrame(m))
	}

	// The harness that already is the default cannot be made it again.
	send(t, m, keyPress("k"), keyPress("s"))
	if m.dialog == nil || m.dialog.kind != dlgMessage || !strings.Contains(m.dialog.body, "already the default") {
		t.Fatal("setting the default harness as the default should say it already is")
	}
}

// v is the whole configuration: what the harness runs, which secret answers
// each variable it needs, and the files it carries.
func TestHarnessesConfigCard(t *testing.T) {
	t.Parallel()
	ds := newFakeSource()
	ds.secrets = []console.HarnessSecret{
		//nolint:gosec // These are the names of a variable and of a secret, not a credential.
		{Name: "ANTHROPIC_API_KEY", Required: true, Declared: true,
			SecretID: "sec_1", SecretType: "api_key", SecretName: "anthropic-api-key"},
		{Name: "CLAUDE_CODE_OAUTH_TOKEN", OneOf: "auth", Declared: true},
		{Name: "CUSTOM_TOKEN", SecretID: "sec_2"},
	}
	m := newTestModel(t, ds)
	send(t, m, keyPress("f3"), keyPress("v"))

	if m.dialog == nil || m.dialog.kind != dlgText {
		t.Fatal("v should open the config card")
	}
	body := m.dialog.body
	for _, want := range []string{
		"★ default", "built-in", "hc_claude",
		"ghcr.io/example/claude:latest", "claude",
		"ANTHROPIC_API_KEY", "required", "sec_1", "api_key", "anthropic-api-key",
		"CLAUDE_CODE_OAUTH_TOKEN", "one of auth", "nothing bound",
		"CUSTOM_TOKEN", "bound by hand", "sec_2",
		".claude.json", "saved from a discobox", "settings.json", "from the image",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("config card is missing %q:\n%s", want, body)
		}
	}
}

// A file says which set it comes from, and the one beneath it that it
// overrides.
func TestHarnessFileDetailNamesItsSource(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		file console.HarnessFile
		want string
	}{
		{console.HarnessFile{Content: "{}", Source: console.HarnessFileFromBundle, CreateOnly: true}, "2 B · from the image · create-only"},
		{console.HarnessFile{Content: "{}", Source: console.HarnessFileSaved, Shadows: []console.HarnessFileSource{console.HarnessFileFromBundle}}, "2 B · saved from a discobox · over the image's"},
		{console.HarnessFile{Content: "{}", Source: console.HarnessFileOverride}, "2 B · yours"},
		{console.HarnessFile{Content: "{}", Source: console.HarnessFileOverride, Shadows: []console.HarnessFileSource{console.HarnessFileSaved, console.HarnessFileFromBundle}}, "2 B · yours · over the saved one"},
	} {
		if got := harnessFileDetail(tc.file); got != tc.want {
			t.Errorf("detail = %q, want %q", got, tc.want)
		}
	}
}

// f picks a file and hands it to the editor, which reports back whether the
// file changed. The edit waits on the person, so the window is driven.
func TestHarnessesEditFile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource()
		ds.editChanged = true
		m := newTestModel(t, ds)
		d := newDriver(t, m)
		d.start()
		d.key("f3")
		d.key("f")

		if m.dialog == nil || m.dialog.kind != dlgActions {
			t.Fatal("f should open the file picker")
		}
		if len(m.dialog.items) != 2 || m.dialog.items[0].label != ".claude.json" {
			t.Fatalf("files = %+v, want the saved ones first", m.dialog.items)
		}

		d.key("enter")
		d.wait("the edit reported", func() bool { return strings.Contains(plainFrame(m), "updated .claude.json") })
		if len(ds.editedFiles) != 1 || ds.editedFiles[0] != "hc_claude .claude.json" {
			t.Fatalf("edited = %v, want the chosen file", ds.editedFiles)
		}

		// An editor that saved nothing says so rather than claiming an update.
		ds.editChanged = false
		d.key("f")
		d.key("enter")
		d.wait("an unchanged file said", func() bool { return strings.Contains(plainFrame(m), ".claude.json unchanged") })
	})
}

// The file the data source writes out for an edit is handed to the editor
// with the terminal, and what the editor saved there is what is read back.
func TestHarnessesEditFileHandsTheTerminalToTheEditor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ds := newFakeSource()
		ds.editFile = filepath.Join(t.TempDir(), "settings.json")
		m := newTestModel(t, ds)
		var titles []string
		m.exec = func(c tea.ExecCommand, done tea.ExecCallback) tea.Cmd {
			return func() tea.Msg {
				titles = append(titles, c.(*harnessExec).title)
				return done(os.WriteFile(ds.editFile, []byte("after"), 0o600))
			}
		}
		d := newDriver(t, m)
		d.start()
		d.key("f3")
		d.key("f")
		d.key("enter")
		d.wait("the editor's save read back", func() bool { return strings.Contains(plainFrame(m), "updated .claude.json") })
		if len(titles) != 1 || titles[0] != "Editing .claude.json" {
			t.Fatalf("the terminal went to %q, want the edit by its title", titles)
		}
	})
}

// A harness with no files has nothing to edit, and says so.
func TestHarnessesEditNeedsFiles(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	send(t, m, keyPress("f3"), keyPress("G"), keyPress("f"))

	if m.dialog == nil || m.dialog.kind != dlgMessage {
		t.Fatal("f on a harness with no files should explain itself")
	}
}

// The run options' harness choices are the harnesses, with the default leading.
func TestHarnessChoicesFollowTheListing(t *testing.T) {
	t.Parallel()
	ds := newFakeSource()
	m := newTestModel(t, ds)

	harness := m.opts.Opts[console.OptHarness]
	want := []string{"claude", "codex", "custom", "scratch", "shell"}
	if strings.Join(harness.Choices, ",") != strings.Join(want, ",") {
		t.Fatalf("choices = %v, want %v", harness.Choices, want)
	}
	if !strings.Contains(harness.Hint, "which is claude") {
		t.Fatalf("hint = %q, want the project default named", harness.Hint)
	}

	// A chosen harness survives a refresh of the listing.
	harness.Index = 3
	send(t, m, tickMsg{})
	if got := harness.Choices[m.opts.Opts[console.OptHarness].Index]; got != "scratch" {
		t.Fatalf("chosen harness = %q, want the one that was chosen", got)
	}

	// Nothing chosen means the default, which is what an empty --harness does.
	m.opts.Opts[console.OptHarness].Index = 0
	if m.opts.Request("", m.opts.Opts[console.OptSource].Selected()).Harness != "" {
		t.Fatal("the leading choice is the project default and should emit no flag")
	}
}

// Enter on the harness row advances the choice, the way it does on every other
// row of the panel. Leaving for the harnesses screen would make one row of a
// picker behave unlike the rest of it; F3 is how that screen is reached, and
// the row's own hint says so.
func TestEnterOnTheHarnessRowChangesTheChoice(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	send(t, m, keyPress("ctrl+o"))
	if !m.optionsOpen {
		t.Fatal("Ctrl-O should open the run options")
	}
	before := m.opts.Opts[console.OptHarness].Display()

	send(t, m, keyPress("enter"))

	if m.harnessesOpen {
		t.Fatal("Enter on the harness row should not leave the panel")
	}
	if !m.optionsOpen {
		t.Fatal("Enter on the harness row should stay in the run options")
	}
	if after := m.opts.Opts[console.OptHarness].Display(); after == before {
		t.Fatalf("harness = %q both before and after Enter, want the choice advanced", after)
	}
}

// The frame is exactly the terminal's height, harnesses screen included: one
// row too many scrolls the terminal, which is the one thing the renderer cannot
// redraw its way out of.
func TestHarnessesFrameFitsTheTerminal(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	for _, size := range [][2]int{{120, 40}, {100, 24}, {80, 20}} {
		send(t, m, sizeMsg(size[0], size[1]))
		if !m.harnessesOpen {
			send(t, m, keyPress("f3"))
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
// in particular applies only to a harness that is not already the default,
// which is the state the fixture's second row is in.
func TestHarnessHintsNameEveryApplicableAction(t *testing.T) {
	t.Parallel()
	ds := newFakeSource()
	m := newTestModel(t, ds)
	send(t, m, keyPress("f3"))
	// 80 columns is the narrow terminal worth holding to: the hint line grows
	// with the actions that apply, and the widest row is the one every action
	// applies to.
	send(t, m, sizeMsg(80, 40))

	for i := range ds.harnesses {
		m.harnesses.moveTo(i)
		harness := *m.harnesses.current()
		hints := hintLine(m.harnessHints())
		for _, a := range harnessActions(harness) {
			named := strings.Contains(hints, a.key+" "+a.label)
			if a.enabled && !named {
				t.Errorf("%s: %q applies but the hints do not name it: %q", harness.DisplayName(), a.key, hints)
			}
			if !a.enabled && named {
				t.Errorf("%s: %q does not apply but the hints name it: %q", harness.DisplayName(), a.key, hints)
			}
		}
		// The line has to fit as written. viewStatus truncates rather than
		// wrapping, so measuring the frame would measure the cut and always
		// pass; what matters is whether there was room for the whole of it.
		if got := lipgloss.Width("  " + hints); got > m.inner() {
			t.Errorf("%s: the hint line needs %d columns of the %d there are: %q",
				harness.DisplayName(), got, m.inner(), hints)
		}
	}
}

// s is the only action that needs a harness which is not already the default,
// so it is the one most easily left unreachable.
func TestHarnessHintsOfferTheDefault(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	send(t, m, keyPress("f3"), keyPress("j"))

	if hints := hintLine(m.harnessHints()); !strings.Contains(hints, "s default") {
		t.Fatalf("hints = %q, want the default offered on a non-default harness", hints)
	}
}

// A run on any harness goes straight to create: there is no setup a harness
// has to have been through first (ADR 26-11-02-152).
func TestRunningAnyHarnessDoesNotAsk(t *testing.T) {
	t.Parallel()
	ds := newFakeSource()
	m := newTestModel(t, ds)
	harness := m.opts.Opts[console.OptHarness]
	for i, choice := range harness.Choices {
		if choice == "custom" {
			harness.Index = i
		}
	}
	send(t, m, typeString("do the thing")...)
	send(t, m, keyPress("enter"), keyPress("enter"))

	if m.dialog != nil && (m.dialog.kind == dlgConfirm || m.dialog.kind == dlgActions) {
		t.Fatalf("running a harness should not ask anything: %s", describe(m.dialog))
	}
	if len(ds.runs) != 1 || ds.runs[0].Harness != "custom" {
		t.Fatalf("runs = %v, want the run on the chosen harness", ds.runs)
	}
}

// noDefaultSource is a project that has never chosen a default: what a fresh
// one looks like before anybody has set anything up.
func noDefaultSource(t *testing.T) *fakeSource {
	t.Helper()
	ds := newFakeSource()
	for i := range ds.harnesses {
		ds.harnesses[i].Default = false
	}
	return ds
}

// Submitting a prompt with nothing chosen and no project default asks which
// harness the project should run, rather than letting the server refuse the
// create a moment later.
func TestAPromptWithNoDefaultAsksForOne(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := noDefaultSource(t)
		d, m := drivenTestModel(t, ds)
		send(t, m, typeString("do the thing")...)
		d.key("enter")
		d.key("enter")

		if m.dialog == nil || m.dialog.kind != dlgActions {
			t.Fatal("a prompt with no harness and no default should ask for one")
		}
		if runs := ds.runRequests(); len(runs) != 0 {
			t.Fatalf("runs = %v, want the run held until the project has a harness", runs)
		}
		// `shell` runs like any other harness but is not a coding harness, so it is
		// not offered as the thing a project runs by default.
		for _, item := range m.dialog.items {
			if strings.EqualFold(item.label, "Shell") {
				t.Fatalf("shell should not be offered as a project default: %v", m.dialog.items)
			}
		}
		d.key("esc")
	})
}

// Choosing a harness makes it the default outright.
func TestChoosingAHarnessMakesItTheDefault(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := noDefaultSource(t)
		d, m := drivenTestModel(t, ds)
		d.key("enter")
		d.key("enter")
		d.wait("the harness question", func() bool { return m.dialog != nil && m.dialog.kind == dlgActions })

		// Claude is the first candidate.
		d.key("1")

		d.wait("the run", func() bool { return len(ds.runRequests()) == 1 })
		if did := ds.harnessCalls(); len(did) != 1 || did[0] != "set default hc_claude" {
			t.Fatalf("did = %v, want the chosen harness made default", did)
		}
	})
}

// A project with nothing but `shell` has nothing to offer, and says so rather
// than opening a menu with no choices on it.
func TestAProjectWithOnlyShellSaysThereIsNothingToRun(t *testing.T) {
	t.Parallel()
	ds := newFakeSource()
	ds.harnesses = []console.Harness{{
		ID: "hc_shell", Name: "Shell", Slug: "shell",
		BuiltIn: true, Shell: true,
	}}
	m := newTestModel(t, ds)
	send(t, m, keyPress("enter"), keyPress("enter"))

	if m.dialog != nil && m.dialog.kind == dlgActions {
		t.Fatal("there is nothing to choose between, so no menu should open")
	}
	if !strings.Contains(plainFrame(m), "no harness to run") {
		t.Fatalf("the window should say the project has nothing to run:\n%s", plainFrame(m))
	}
}

// With a default set, a prompt just runs.
func TestAPromptWithADefaultDoesNotAsk(t *testing.T) {
	t.Parallel()
	ds := newFakeSource()
	m := newTestModel(t, ds)
	send(t, m, typeString("go")...)
	send(t, m, keyPress("enter"))

	if m.dialog != nil && m.dialog.kind == dlgActions {
		t.Fatal("a project with a default should not be asked for one")
	}
}

// A prompt submitted before the listing has landed is not refused on the
// strength of what has not arrived. An empty listing and one still in flight
// look the same and mean opposite things; the server refuses a create it cannot
// resolve, which is the answer this would only be guessing at.
func TestAPromptBeforeTheListingLandsIsNotRefused(t *testing.T) {
	t.Parallel()
	ds := noDefaultSource(t)
	m := newTestModel(t, ds)
	// Put the model back where it is a moment after opening: nothing loaded.
	m.harnesses.loaded = false
	m.harnesses.all = nil

	send(t, m, keyPress("enter"))

	if m.dialog != nil && m.dialog.kind == dlgActions {
		t.Fatal("nothing is known about the project's harnesses yet, so nothing should be asked")
	}
	if strings.Contains(plainFrame(m), "no harness to run") {
		t.Fatalf("a listing in flight is not an empty project:\n%s", plainFrame(m))
	}
}

// The question interrupted a run, so answering it runs: choosing a harness sets
// the default and then submits the prompt that asked.
func TestChoosingADefaultResumesTheRunThatAsked(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ds := noDefaultSource(t)
		d, m := drivenTestModel(t, ds)
		send(t, m, typeString("fix the reaper")...)
		d.key("enter")
		d.key("enter")
		d.wait("the harness question", func() bool { return m.dialog != nil && m.dialog.kind == dlgActions })
		d.key("1")

		d.wait("the run", func() bool { return len(ds.runRequests()) == 1 })
		if got := promptText(ds.runRequests()[0]); got != "fix the reaper" {
			t.Fatalf("prompt = %q, want the one that was waiting", got)
		}
	})
}

// The header offers F3 and F4 from everywhere, so each key has to work from
// the other's screen: a screen that swallowed the key to its neighbor would
// make the header's offer a lie for as long as it was up.
func TestTheTwoScreensReachEachOther(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource(testSandboxes()...))

	send(t, m, keyPress(harnessesKey))
	if !m.harnessesOpen {
		t.Fatalf("F3 did not open the harnesses")
	}

	send(t, m, keyPress(secretsKey))
	if !m.secretsOpen || m.harnessesOpen {
		t.Fatalf("F4 from the harnesses: secrets=%v harnesses=%v, want the secrets alone", m.secretsOpen, m.harnessesOpen)
	}

	send(t, m, keyPress(harnessesKey))
	if !m.harnessesOpen || m.secretsOpen {
		t.Fatalf("F3 from the secrets: harnesses=%v secrets=%v, want the harnesses alone", m.harnessesOpen, m.secretsOpen)
	}

	// And leaving either goes back to the launcher, not to the other one left
	// standing behind it.
	send(t, m, keyPress("esc"))
	if m.harnessesOpen || m.secretsOpen {
		t.Fatalf("esc left harnesses=%v secrets=%v, want the launcher", m.harnessesOpen, m.secretsOpen)
	}
}

// Every harness is offered, the default leading: a harness names no
// platform and no image kind, so no pool rules one out (ADR 26-11-01-122).
func TestHarnessChoicesOfferEveryHarness(t *testing.T) {
	t.Parallel()
	m := newTestModel(t, newFakeSource())
	m.opts.SetHarnesses([]console.Harness{
		{Slug: "shell"},
		{Slug: "codex", Default: true},
		{Slug: "claude"},
	})
	want := []string{"codex", "shell", "claude"}
	if got := m.opts.Opts[console.OptHarness].Choices; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("choices = %v, want %v", got, want)
	}
}
