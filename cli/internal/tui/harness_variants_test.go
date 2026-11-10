package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

func TestCreateHarnessVariantFromMenuRefreshesAndSelectsIt(t *testing.T) {
	t.Parallel()
	ds := newFakeSource()
	m := newTestModel(t, ds)
	send(t, m, keyPress("f3"), keyPress("."), keyPress("c"))
	if m.dialog == nil || m.dialog.kind != dlgForm {
		t.Fatal("menu did not open variant form")
	}
	parent := m.harnesses.current().ID
	f := m.dialog.form
	if f.value("parent") != m.harnesses.current().DisplayName() || f.answerable(0) {
		t.Fatal("selected parent is not fixed")
	}
	if !strings.Contains(m.dialog.body, "not copied") {
		t.Fatal("missing account isolation explanation")
	}
	f.set("name", "Work account")
	f.set("slug", "work-account")
	send(t, m, keyPress("enter"))
	want := console.NewHarnessVariant{ParentID: parent, Name: "Work account", Slug: "work-account"}
	if len(ds.createdVariants) != 1 || ds.createdVariants[0] != want {
		t.Fatalf("creates: %+v", ds.createdVariants)
	}
	if m.dialog != nil || m.harnesses.current().ID != "variant-created" {
		t.Fatal("created variant was not selected")
	}
	if !slices.Contains(m.opts.Opts[console.OptHarness].Choices, "work-account") {
		t.Fatal("run selector not refreshed")
	}
}

func TestCreateHarnessVariantValidatesCancelsAndKeepsRefusedFields(t *testing.T) {
	t.Parallel()
	ds := newFakeSource()
	m := newTestModel(t, ds)
	send(t, m, keyPress("f3"), keyPress("c"), keyPress("enter"))
	if m.dialog == nil || !strings.Contains(m.dialog.form.err, "name") || len(ds.createdVariants) != 0 {
		t.Fatal("empty name submitted")
	}
	m.dialog.form.set("name", "Work")
	send(t, m, keyPress("enter"))
	if !strings.Contains(m.dialog.form.err, "slug") {
		t.Fatal("empty slug submitted")
	}
	send(t, m, keyPress("esc"))
	if len(ds.createdVariants) != 0 {
		t.Fatal("cancel created a variant")
	}
	send(t, m, keyPress("c"))
	f := m.dialog.form
	f.set("name", "Work")
	f.set("slug", "taken")
	ds.variantErr = errors.New("slug already in use")
	send(t, m, keyPress("enter"))
	if m.dialog == nil || m.dialog.kind != dlgForm || m.dialog.form.value("name") != "Work" || m.dialog.form.value("slug") != "taken" || !strings.Contains(m.dialog.form.err, "slug already in use") {
		t.Fatal("refusal lost form or error")
	}
	ds.variantErr = nil
	m.dialog.form.set("slug", "available")
	send(t, m, keyPress("enter"))
	if len(ds.createdVariants) != 1 || ds.createdVariants[0].Slug != "available" {
		t.Fatal("retry failed")
	}
}

func TestVariantCannotBeUsedAsParentInConsole(t *testing.T) {
	t.Parallel()
	ds := newFakeSource()
	ds.harnesses[0].Parent = "root"
	m := newTestModel(t, ds)
	send(t, m, keyPress("f3"), keyPress("c"))
	if m.dialog == nil || m.dialog.kind == dlgForm || !strings.Contains(m.dialog.body, "standalone parent") || len(ds.createdVariants) != 0 {
		t.Fatal("variant offered as a parent")
	}
}

func TestVariantCreationStaysOnCapturedServerAndDoesNotReplaceNewDialog(t *testing.T) {
	t.Parallel()
	ds := newFakeSource()
	m := newTestModel(t, ds)
	m.list.Filter.Server = "work-server"
	parent := testHarnesses()[0]
	m.newHarnessVariantForm(parent)
	form := m.dialog
	request := console.NewHarnessVariant{ParentID: parent.ID, Name: "Work", Slug: "work"}
	cmd := m.createHarnessVariant("work-server", request, form)
	other := errorDialog("Other operation", "keep me")
	m.dialog = other
	m.list.Filter.Server = "other-server"
	send(t, m, cmd())
	if !slices.Contains(ds.onServer, "CreateHarnessVariant@work-server") {
		t.Fatal("creation changed server")
	}
	if m.dialog != other || m.harnesses.current().ID == "variant-created" {
		t.Fatal("late result replaced new server's state")
	}
	// Refusals likewise do not reopen an abandoned form over another dialog.
	ds.variantErr = errors.New("duplicate")
	cmd = m.createHarnessVariant("work-server", request, form)
	m.dialog = other
	send(t, m, cmd())
	if m.dialog != other {
		t.Fatal("late refusal reopened abandoned form")
	}
}

func TestVariantSubmissionInstallsItsWaitBeforeAnotherInput(t *testing.T) {
	t.Parallel()
	ds := newFakeSource()
	m := newTestModel(t, ds)
	send(t, m, keyPress("f3"), keyPress("c"))
	m.dialog.form.set("name", "Work")
	m.dialog.form.set("slug", "work")
	_, cmd := m.Update(keyPress("enter"))
	if m.dialog == nil || m.dialog.kind != dlgStatus {
		t.Fatal("submission left input unguarded before starting create")
	}
	other := errorDialog("Another operation", "keep this dialog")
	m.dialog = other
	drain(t, m, cmd, 0)
	if m.dialog != other {
		t.Fatal("late creation replaced another dialog")
	}
}
