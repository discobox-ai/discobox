package tui

import (
	"context"
	"testing"
)

func twoDefinitionSource() *fakeSource {
	return &fakeSource{
		definitions: []HarnessDefinition{
			{ID: "hd_codex", Name: "codex", Description: "OpenAI Codex"},
			{ID: "hd_claude", Name: "claude", Description: "Claude Code"},
		},
	}
}

func newLoadedHarnessForm(f *fakeSource, edit *HarnessConfig) *harnessFormScreen {
	s := newHarnessFormScreen(context.Background(), f, defaultKeyMap(), defaultStyles(), edit)
	next, _ := s.Update(s.loadCmd()())
	s = next.(*harnessFormScreen)
	next, _ = s.Update(resizeMsg{width: 100, height: 20})
	return next.(*harnessFormScreen)
}

func harnessFormPress(s *harnessFormScreen, spec string) *harnessFormScreen {
	next, _ := s.Update(keyPress(spec))
	return next.(*harnessFormScreen)
}

func TestHarnessFormCreateFromDefinition(t *testing.T) {
	f := twoDefinitionSource()
	f.saveOut = HarnessConfig{ID: "hc_new", Name: "codex"}
	s := newLoadedHarnessForm(f, nil)

	// Create mode starts on the source field with the first definition selected.
	if s.currentField() != hfSource {
		t.Fatalf("focus = %v, want hfSource", s.currentField())
	}
	def, ok := s.selectedDefinition()
	if !ok || def.ID != "hd_codex" {
		t.Fatalf("selected definition = %+v, want hd_codex", def)
	}

	// Focus the submit button and submit.
	for s.currentField() != hfSubmit {
		s = harnessFormPress(s, "tab")
	}
	_, cmd := s.Update(keyPress("enter"))
	msg := runCmd(cmd)
	saved, ok := msg.(harnessSavedMsg)
	if !ok {
		t.Fatalf("submit returned %T, want harnessSavedMsg", msg)
	}
	if !saved.created || saved.config.ID != "hc_new" {
		t.Fatalf("saved = %+v, want created hc_new", saved)
	}
	reqs := f.savedReqs()
	if len(reqs) != 1 {
		t.Fatalf("SaveHarness called %d times, want 1", len(reqs))
	}
	if reqs[0].DefinitionID != "hd_codex" || reqs[0].ID != "" {
		t.Fatalf("request = %+v, want create from hd_codex", reqs[0])
	}
}

func TestHarnessFormCustomRequiresImage(t *testing.T) {
	f := twoDefinitionSource()
	s := newLoadedHarnessForm(f, nil)

	// Open the source dropdown and pick the trailing "Custom image" entry.
	s = harnessFormPress(s, "enter")
	if !s.open {
		t.Fatal("enter should open the source dropdown")
	}
	for i := 0; i < s.sourceCount(); i++ {
		s = harnessFormPress(s, "down")
	}
	s = harnessFormPress(s, "enter")
	if !s.isCustom() {
		t.Fatal("selecting the last option should choose the custom source")
	}

	// Submitting without an image is rejected locally (no SaveHarness call).
	for s.currentField() != hfSubmit {
		s = harnessFormPress(s, "tab")
	}
	_, cmd := s.Update(keyPress("enter"))
	if runCmd(cmd) != nil {
		t.Fatal("custom submit without an image should not call SaveHarness")
	}
	if s.errText == "" {
		t.Fatal("missing image should surface an error")
	}
	if len(f.savedReqs()) != 0 {
		t.Fatalf("SaveHarness should not run, got %v", f.savedReqs())
	}
}

func TestHarnessFormCustomSubmits(t *testing.T) {
	f := twoDefinitionSource()
	f.saveOut = HarnessConfig{ID: "hc_custom"}
	s := newLoadedHarnessForm(f, nil)

	s = harnessFormPress(s, "enter") // open dropdown
	for i := 0; i < s.sourceCount(); i++ {
		s = harnessFormPress(s, "down")
	}
	s = harnessFormPress(s, "enter") // choose custom

	// Custom create takes only an image.
	s = harnessFormPress(s, "tab") // to Image
	s.image.SetValue("reg/img:2")

	_, cmd := s.Update(keyPress("enter")) // enter on a text field submits
	msg := runCmd(cmd)
	if _, ok := msg.(harnessSavedMsg); !ok {
		t.Fatalf("submit returned %T, want harnessSavedMsg", msg)
	}
	reqs := f.savedReqs()
	if len(reqs) != 1 {
		t.Fatalf("SaveHarness called %d times, want 1", len(reqs))
	}
	got := reqs[0]
	if got.DefinitionID != "" || got.Name != "" || got.Image != "reg/img:2" {
		t.Fatalf("request = %+v, want custom image-only create", got)
	}
}

func TestHarnessFormEditRenamesOnly(t *testing.T) {
	f := twoDefinitionSource()
	f.saveOut = HarnessConfig{ID: "hc_a", Name: "renamed"}
	edit := &HarnessConfig{ID: "hc_a", Name: "codex", Slug: "codex", DefinitionID: "hd_codex", Default: true}
	s := newLoadedHarnessForm(f, edit)

	// Edit mode omits the source/image fields; only the name is editable.
	for _, field := range s.fields {
		if field == hfSource || field == hfImage {
			t.Fatalf("edit form should not show field %v", field)
		}
	}
	// Name is prefilled from the config.
	if s.name.Value() != "codex" {
		t.Fatalf("name = %q, want codex", s.name.Value())
	}
	s.name.SetValue("renamed")
	_, cmd := s.Update(keyPress("enter"))
	msg := runCmd(cmd)
	saved, ok := msg.(harnessSavedMsg)
	if !ok {
		t.Fatalf("submit returned %T, want harnessSavedMsg", msg)
	}
	if saved.created {
		t.Fatal("edit should report an update, not a create")
	}
	reqs := f.savedReqs()
	if len(reqs) != 1 || reqs[0].ID != "hc_a" || reqs[0].Name != "renamed" {
		t.Fatalf("request = %+v, want update hc_a name=renamed", reqs)
	}
}

func TestHarnessFormHasNoDefaultField(t *testing.T) {
	// Choosing the default is done from the agents list, not the form.
	f := twoDefinitionSource()
	s := newLoadedHarnessForm(f, nil)
	s = harnessFormPress(s, "enter") // open source dropdown
	for i := 0; i < s.sourceCount(); i++ {
		s = harnessFormPress(s, "down")
	}
	s = harnessFormPress(s, "enter") // custom: widest field set
	want := []harnessField{hfSource, hfImage, hfSubmit}
	if len(s.fields) != len(want) {
		t.Fatalf("custom fields = %v, want %v", s.fields, want)
	}
	for i, f := range want {
		if s.fields[i] != f {
			t.Fatalf("custom fields = %v, want %v", s.fields, want)
		}
	}
}

func TestHarnessFormEscGoesBack(t *testing.T) {
	f := twoDefinitionSource()
	s := newLoadedHarnessForm(f, nil)
	// Move off the source field so esc is not swallowed by a dropdown.
	s = harnessFormPress(s, "tab")
	_, cmd := s.Update(keyPress("esc"))
	if _, ok := runCmd(cmd).(harnessFormBackMsg); !ok {
		t.Fatal("esc should emit harnessFormBackMsg")
	}
}
