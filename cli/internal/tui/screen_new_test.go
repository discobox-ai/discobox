package tui

import (
	"context"
	"testing"
)

func newLoadedForm(f *fakeSource) *newSessionScreen {
	s := newNewSessionScreen(context.Background(), f, defaultKeyMap(), defaultStyles())
	s.Init() // focuses the prompt, matching how the root starts the screen
	next, _ := s.Update(s.loadCmd()())
	return next.(*newSessionScreen)
}

func formPress(s *newSessionScreen, spec string) *newSessionScreen {
	next, _ := s.Update(keyPress(spec))
	return next.(*newSessionScreen)
}

func twoHarnessSource() *fakeSource {
	return &fakeSource{
		harnesses: []Harness{
			{Name: "codex", Label: "codex", Default: true},
			{Name: "claude", Label: "claude"},
		},
		paths:       []string{"/repo/a"},
		defaultPath: "/work",
	}
}

func TestFormDefaults(t *testing.T) {
	s := newLoadedForm(twoHarnessSource())
	if s.focus != fieldPrompt {
		t.Fatalf("focus = %d, want prompt (%d)", s.focus, fieldPrompt)
	}
	if s.selectedHarness() != "codex" {
		t.Fatalf("default harness = %q, want codex", s.selectedHarness())
	}
	// The default path (cwd) leads the options, then distinct existing sources.
	if len(s.paths) != 2 || s.paths[0] != "/work" || s.paths[1] != "/repo/a" {
		t.Fatalf("paths = %v, want [/work /repo/a]", s.paths)
	}
}

func TestFormSubmitCreatesSession(t *testing.T) {
	f := twoHarnessSource()
	f.createOut = Sandbox{ID: "sbx_new", Name: "new-one"}
	s := newLoadedForm(f)

	s.prompt.SetValue("fix the failing tests")
	_, cmd := s.Update(keyPress("enter")) // enter on the prompt submits
	msg := runCmd(cmd)
	created, ok := msg.(sessionCreatedMsg)
	if !ok {
		t.Fatalf("submit returned %T, want sessionCreatedMsg", msg)
	}
	if created.sandbox.ID != "sbx_new" {
		t.Fatalf("created sandbox = %s, want sbx_new", created.sandbox.ID)
	}
	reqs := f.createdReqs()
	if len(reqs) != 1 {
		t.Fatalf("create called %d times, want 1", len(reqs))
	}
	if reqs[0] != (NewSessionRequest{Harness: "codex", Path: "/work", Prompt: "fix the failing tests"}) {
		t.Fatalf("request = %+v", reqs[0])
	}
}

func TestFormEmptyPromptIsValid(t *testing.T) {
	f := twoHarnessSource()
	s := newLoadedForm(f)
	_, cmd := s.Update(keyPress("enter"))
	if _, ok := runCmd(cmd).(sessionCreatedMsg); !ok {
		t.Fatal("empty prompt should still create a session")
	}
	if reqs := f.createdReqs(); len(reqs) != 1 || reqs[0].Prompt != "" {
		t.Fatalf("reqs = %v, want one empty-prompt request", reqs)
	}
}

func TestFormHarnessDropdownSelects(t *testing.T) {
	s := newLoadedForm(twoHarnessSource())

	// Move focus prompt -> path -> harness.
	s = formPress(s, "up")
	s = formPress(s, "up")
	if s.focus != fieldHarness {
		t.Fatalf("focus = %d, want harness", s.focus)
	}
	// Open, move to the second option, select it.
	s = formPress(s, "enter")
	if !s.open {
		t.Fatal("enter on harness field should open the dropdown")
	}
	s = formPress(s, "down")
	s = formPress(s, "enter")
	if s.open {
		t.Fatal("enter should close the dropdown")
	}
	if s.selectedHarness() != "claude" {
		t.Fatalf("harness = %q, want claude", s.selectedHarness())
	}
}

func TestFormPathCycleWithArrows(t *testing.T) {
	s := newLoadedForm(twoHarnessSource())
	s = formPress(s, "up") // prompt -> path
	if s.focus != fieldPath {
		t.Fatalf("focus = %d, want path", s.focus)
	}
	s = formPress(s, "right") // cycle to next path
	if s.selectedPath() != "/repo/a" {
		t.Fatalf("path = %q, want /repo/a", s.selectedPath())
	}
	s = formPress(s, "right") // wraps back to default
	if s.selectedPath() != "/work" {
		t.Fatalf("path after wrap = %q, want /work", s.selectedPath())
	}
}

// TestFormPromptAcceptsNavLetters guards against the vim navigation aliases
// (j/k/h/l) hijacking text entry: in the prompt they must be inserted as text,
// not move focus between fields.
func TestFormPromptAcceptsNavLetters(t *testing.T) {
	s := newLoadedForm(twoHarnessSource())
	for _, ch := range []string{"f", "l", "a", "k", "y", "j", "h", "l"} {
		s = formPress(s, ch)
	}
	if s.focus != fieldPrompt {
		t.Fatalf("focus moved to %d while typing in the prompt", s.focus)
	}
	if got := s.prompt.Value(); got != "flakyjhl" {
		t.Fatalf("prompt value = %q, want flakyjhl", got)
	}
}

func TestFormEscCancels(t *testing.T) {
	s := newLoadedForm(twoHarnessSource())
	_, cmd := s.Update(keyPress("esc"))
	if _, ok := runCmd(cmd).(backMsg); !ok {
		t.Fatal("esc should emit backMsg")
	}
}

func TestFormEscClosesOpenDropdownFirst(t *testing.T) {
	s := newLoadedForm(twoHarnessSource())
	s = formPress(s, "up") // to path
	s = formPress(s, "enter")
	if !s.open {
		t.Fatal("dropdown should be open")
	}
	_, cmd := s.Update(keyPress("esc"))
	if runCmd(cmd) != nil {
		t.Fatal("esc with open dropdown should not cancel the form")
	}
	if s.open {
		t.Fatal("esc should close the dropdown")
	}
}

func TestFormSubmitErrorStaysOnForm(t *testing.T) {
	f := twoHarnessSource()
	f.createErr = errBoom
	s := newLoadedForm(f)
	_, cmd := s.Update(keyPress("enter"))
	msg := runCmd(cmd)
	em, ok := msg.(errMsg)
	if !ok {
		t.Fatalf("submit error returned %T, want errMsg", msg)
	}
	next, _ := s.Update(em)
	form := next.(*newSessionScreen)
	if form.submitting {
		t.Fatal("form should clear submitting after an error")
	}
	if form.errText == "" {
		t.Fatal("form should record the error text")
	}
}
