package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

func settingsSandboxes() []console.Sandbox {
	return []console.Sandbox{
		{ID: "sbx_run", Name: "running box", State: console.StateRunning, HasRuntime: true, Harness: "claude",
			HarnessID: "hc_claude", OriginKey: testKey("/src/disco2"), OriginHostID: testHostID},
		{ID: "sbx_stop", Name: "stopped box", State: console.StateStopped, HasRuntime: true, Harness: "claude",
			HarnessID: "hc_claude", OriginKey: testKey("/src/disco2"), OriginHostID: testHostID},
	}
}

func settingsSource() *fakeSource {
	ds := newFakeSource(settingsSandboxes()...)
	ds.harnesses = []console.Harness{
		{ID: "hc_claude", Name: "Claude Code", Slug: "claude"},
		{ID: "hc_codex", Name: "Codex", Slug: "codex"},
		{ID: "hc_other", Name: "Other", Slug: "other"},
	}
	stored := console.BoxSettings{Name: "gen-name", HarnessID: "hc_claude",
		Egress: console.Egress{Allow: []string{"github.com"}}}
	ds.settings = map[string]console.BoxSettings{"sbx_run": stored, "sbx_stop": stored}
	return ds
}

// openSettings puts the list's cursor on the box and opens its settings form.
func openSettings(t *testing.T, m *Model, id string) *form {
	t.Helper()
	send(t, m, keyPress("tab"))
	for m.list.current() != nil && m.list.current().ID != id {
		send(t, m, keyPress("j"))
	}
	send(t, m, keyPress(settingsKey))
	if m.dialog == nil || m.dialog.kind != dlgForm {
		t.Fatalf("%s should open the settings form, got %+v", settingsKey, m.dialog)
	}
	return m.dialog.form
}

// The form opens on what the server holds, and Enter on it unchanged sends
// nothing.
func TestSettingsOpensOnTheStoredValuesAndSavesNothingUnchanged(t *testing.T) {
	t.Parallel()
	ds := settingsSource()
	m := newTestModel(t, ds)
	f := openSettings(t, m, "sbx_run")
	if got := f.value("name"); got != "gen-name" {
		t.Fatalf("name = %q, want the configured name", got)
	}
	if got := f.value("allow"); got != "github.com" {
		t.Fatalf("allow = %q", got)
	}
	if got := f.chosen("deny"); got != "no" {
		t.Fatalf("deny = %q", got)
	}
	send(t, m, keyPress("enter"))
	if m.dialog != nil {
		t.Fatal("enter should close the form")
	}
	if len(ds.updates) != 0 {
		t.Fatalf("an unchanged form should send nothing, got %+v", ds.updates)
	}
}

// Only what was changed is sent: the parts left alone stay nil, so the server
// keeps what it has for them.
func TestSettingsSendsOnlyWhatChanged(t *testing.T) {
	t.Parallel()
	ds := settingsSource()
	m := newTestModel(t, ds)
	f := openSettings(t, m, "sbx_run")
	f.set("allow", "github.com, *.npmjs.org ")
	f.choose("deny", "yes")
	f.choose("api", "yes")
	send(t, m, keyPress("enter"))

	if len(ds.updates) != 1 || ds.updates[0].id != "sbx_run" {
		t.Fatalf("updates = %+v", ds.updates)
	}
	u := ds.updates[0].update
	if u.Name != nil || u.Harness != nil || u.EgressPassthrough != nil {
		t.Fatalf("unchanged parts were sent: %+v", u)
	}
	if u.EgressAllow == nil || !slices.Equal(*u.EgressAllow, []string{"github.com", "*.npmjs.org"}) {
		t.Fatalf("allow = %v", u.EgressAllow)
	}
	if u.EgressDeny == nil || !*u.EgressDeny || u.DiscoboxAPI == nil || !*u.DiscoboxAPI {
		t.Fatalf("deny/api = %v/%v", u.EgressDeny, u.DiscoboxAPI)
	}
	if !strings.Contains(m.statusMessage(80), "settings saved") {
		t.Fatalf("status = %q", m.statusMessage(80))
	}
}

// The harness is changed only on a stopped box; on a running one the row says
// why and is stepped over.
func TestSettingsChangesTheHarnessOnlyWhenStopped(t *testing.T) {
	t.Parallel()
	ds := settingsSource()
	m := newTestModel(t, ds)
	f := openSettings(t, m, "sbx_run")
	if f.answerable(1) {
		t.Fatal("the harness row should not be answerable on a running box")
	}
	send(t, m, keyPress("esc"))

	f = openSettings(t, m, "sbx_stop")
	harness := f.rows[1]
	if !f.answerable(1) || harness.Choices[0].Key != "hc_claude" {
		t.Fatalf("the stopped box's row should open on its harness, got %+v", harness.Choices)
	}
	// Every harness can run, so every one is offered.
	offered := false
	for _, c := range harness.Choices {
		offered = offered || c.Key == "hc_other"
	}
	if !offered {
		t.Fatalf("every harness should be offered, got %+v", harness.Choices)
	}
	f.choose("harness", "hc_codex")
	send(t, m, keyPress("enter"))
	if len(ds.updates) != 1 || ds.updates[0].update.Harness == nil || *ds.updates[0].update.Harness != "hc_codex" {
		t.Fatalf("updates = %+v", ds.updates)
	}
}

// A configured name cannot be emptied: the form refuses and stays up.
func TestSettingsRefusesAnEmptyName(t *testing.T) {
	t.Parallel()
	ds := settingsSource()
	m := newTestModel(t, ds)
	f := openSettings(t, m, "sbx_run")
	f.set("name", "")
	send(t, m, keyPress("enter"))
	if m.dialog == nil || len(ds.updates) != 0 {
		t.Fatalf("an empty name should keep the form up and send nothing, got %+v", ds.updates)
	}
}

// A failed read says so on a card, and no form opens on values nobody read.
func TestSettingsReportsAFailedRead(t *testing.T) {
	t.Parallel()
	ds := settingsSource()
	ds.settingsErr = errors.New("boom")
	m := newTestModel(t, ds)
	send(t, m, keyPress("tab"), keyPress(settingsKey))
	if m.dialog == nil || m.dialog.kind == dlgForm || !strings.Contains(m.dialog.body, "boom") {
		t.Fatalf("want an error card naming the failure, got %+v", m.dialog)
	}
}

// The harness listing only fills the harness row: when it fails the form
// still opens, on the box's own harness.
func TestSettingsOpensWithoutTheHarnessListing(t *testing.T) {
	t.Parallel()
	ds := settingsSource()
	ds.harnessErr = errors.New("no listing")
	m := newTestModel(t, ds)
	f := openSettings(t, m, "sbx_stop")
	if row := f.rows[1]; len(row.Choices) != 1 || row.Choices[0].Key != "hc_claude" {
		t.Fatalf("harness row = %+v, want the box's own harness alone", row.Choices)
	}
}

// A refused save puts the form back with everything typed and the refusal on
// it, rather than closing it and dropping the edits.
func TestSettingsKeepsTheEditsWhenTheSaveIsRefused(t *testing.T) {
	t.Parallel()
	ds := settingsSource()
	ds.updateErr = errors.New("name already taken")
	m := newTestModel(t, ds)
	f := openSettings(t, m, "sbx_run")
	f.set("name", "taken")
	f.choose("api", "yes")
	send(t, m, keyPress("enter"))
	if m.dialog == nil || m.dialog.kind != dlgForm {
		t.Fatalf("the form should come back, got %+v", m.dialog)
	}
	f = m.dialog.form
	if f.value("name") != "taken" || f.chosen("api") != "yes" {
		t.Fatalf("edits lost: name=%q api=%q", f.value("name"), f.chosen("api"))
	}
	if !strings.Contains(f.err, "name already taken") {
		t.Fatalf("err = %q", f.err)
	}
}

// A read given up with Esc opens nothing when its answer arrives, and neither
// does one whose card something else has replaced.
func TestSettingsReadGivenUpOpensNothing(t *testing.T) {
	t.Parallel()
	ds := settingsSource()
	m := newTestModel(t, ds)
	cmd := m.askSettings(settingsSandboxes()[0])
	wait := m.dialog
	if wait == nil || wait.kind != dlgStatus {
		t.Fatalf("the read should wait behind a card, got %+v", wait)
	}
	send(t, m, keyPress("esc"))
	if m.dialog != nil {
		t.Fatalf("esc should give the read up, got %+v", m.dialog)
	}
	send(t, m, cmd())
	if m.dialog != nil {
		t.Fatalf("a given-up read opened %+v", m.dialog)
	}
}

// A name holding a character its field drops is not sent as a rename when
// something else is saved.
func TestSettingsDoesNotRenameAcrossADroppedCharacter(t *testing.T) {
	t.Parallel()
	// Each name is one the field holds otherwise than it is stored: a control
	// character dropped, a line break folded into a space each, a rune that
	// does not decode dropped. The core compares with what the field holds
	// (console.OneLine), so a save of something else renames nothing.
	for _, name := range []string{"odd\x07name", "odd\r\nname", "odd\uFFFDname", "odd\xffname"} {
		ds := settingsSource()
		ds.settings["sbx_run"] = console.BoxSettings{Name: name, HarnessID: "hc_claude"}
		m := newTestModel(t, ds)
		f := openSettings(t, m, "sbx_run")
		if f.value("name") == name {
			t.Fatalf("%q: the field kept it as stored; this test no longer covers a changed one", name)
		}
		f.choose("api", "yes")
		send(t, m, keyPress("enter"))
		if len(ds.updates) != 1 || ds.updates[0].update.Name != nil {
			t.Fatalf("%q: updates = %+v, want the API alone", name, ds.updates)
		}
	}
}
