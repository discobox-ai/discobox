package bridge

import (
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

func formField(f *Form, key string) (Field, bool) {
	for _, x := range f.Fields {
		if x.Key == key {
			return x, true
		}
	}
	return Field{}, false
}

func withHarnesses(core *fakeCore) *fakeCore {
	core.harnesses = []console.Harness{{ID: "h1", Name: "claude", Slug: "claude", Default: true}, {ID: "h2", Name: "codex", Slug: "codex"}}
	core.made = running("new1")
	return core
}

// new asks the create card as the core's form, with the command it would run
// under it, creates what it was answered with — the prompt, the harness, a
// source typed and checked — saying the box the moment the server takes it,
// and enters the new box's workspace. Where it was cut from is where the next
// create opens.
func TestNewCreatesWhatTheCardSays(t *testing.T) {
	core := withHarnesses(newFakeCore("home"))
	a := dial(t, core)
	a.hello()
	id := a.command(Command{Command: "new", Args: Args{Prompt: "fix the bug"}})
	form := a.expect(TypeForm).Form
	if form.Command != id || form.Action != "Create" || !strings.Contains(form.Note, "-p 'fix the bug'") {
		t.Fatalf("form %+v", form)
	}
	if _, ok := formField(form, "harness"); !ok {
		t.Fatalf("the card has no harness: %+v", form.Fields)
	}
	if typed, _ := formField(form, console.TypedSourceKey); typed.Applies {
		t.Fatal("the typed source applies before another… is chosen")
	}

	answers := map[string]string{console.SourceField: console.SourceEnter, console.TypedSourceKey: "nowhere", "harness": "codex"}
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: answers, Draft: true}})
	again := a.expect(TypeForm).Form
	if typed, _ := formField(again, console.TypedSourceKey); !typed.Applies {
		t.Fatal("the typed source does not apply once another… is chosen")
	}
	if !strings.Contains(again.Note, "--harness codex") {
		t.Fatalf("the note after a draft: %q", again.Note)
	}
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: answers}})
	if errs := a.expect(TypeFieldErrors).FieldErrors; len(errs.Errors) != 1 || errs.Errors[0].Field != console.TypedSourceKey {
		t.Fatalf("errors %+v", errs)
	}
	answers[console.TypedSourceKey] = "app"
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: answers}})

	var accepted *Box
	for accepted == nil {
		m := a.message()
		if m.Type == TypeProgress && m.Progress.Command == id && m.Progress.Accepted != nil {
			accepted = m.Progress.Accepted
		}
		if m.Type == TypeResult {
			t.Fatalf("finished before it was taken: %+v", m.Result)
		}
	}
	if accepted.Key != "new1" {
		t.Fatalf("accepted %+v", accepted)
	}
	r := a.result(id)
	if r.Status != StatusEnter || r.Box != "new1" {
		t.Fatalf("result %+v", r)
	}
	req := core.ranWith()
	if req == nil || req.Source != "/abs/app" || req.Harness != "codex" || !slices.Equal(req.Prompt, []string{"fix the bug"}) {
		t.Fatalf("ran %+v", req)
	}
	if !slices.Contains(core.called(), "last source /abs/app") {
		t.Fatalf("calls %q: where it was cut from was not kept", core.called())
	}

	// The next card opens where this one was cut from, with the harness as
	// it was left.
	a.command(Command{Command: "new"})
	next := a.expect(TypeForm).Form
	if source, _ := formField(next, console.SourceField); source.Value != "/abs/app" {
		t.Fatalf("the next card opens on %q", source.Value)
	}
	if harness, _ := formField(next, "harness"); harness.Value != "codex" {
		t.Fatalf("the next card's harness is %q", harness.Value)
	}
}

// A create asked to be left detached is done, not entered; a card dismissed
// creates nothing.
func TestNewDetachedAndDismissed(t *testing.T) {
	core := withHarnesses(newFakeCore("home"))
	a := dial(t, core)
	a.hello()

	id := a.command(Command{Command: "new"})
	form := a.expect(TypeForm).Form
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: map[string]string{}, Cancel: true}})
	if r := a.result(id); r.Status != StatusCanceled || core.ranWith() != nil {
		t.Fatalf("a dismissed card: %+v, ran %+v", r, core.ranWith())
	}

	id = a.command(Command{Command: "options"})
	form = a.expect(TypeForm).Form
	if _, ok := formField(form, "detach"); !ok {
		t.Fatalf("the options have no detach: %+v", form.Fields)
	}
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: map[string]string{"detach": "on", console.OptionItemKey(console.OptEnv, 0): "A=1"}}})
	if r := a.result(id); r.Status != StatusDone {
		t.Fatalf("options %+v", r)
	}

	id = a.command(Command{Command: "new"})
	form = a.expect(TypeForm).Form
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: map[string]string{console.SourceField: console.SourceNone}}})
	r := a.result(id)
	if r.Status != StatusDone || r.Box != "new1" || !strings.Contains(r.Note, "detached") {
		t.Fatalf("a detached create: %+v", r)
	}
	if req := core.ranWith(); !req.Detach || !req.NoSource || !slices.Equal(req.Env, []string{"A=1"}) {
		t.Fatalf("ran %+v", req)
	}
}

// What the working tree carries is asked as the core asks it, with the
// directory as its subject; leaving the repository's question starts from the
// last commit, and leaving the copy question creates nothing.
func TestNewAsksAboutTheWorkingTree(t *testing.T) {
	core := withHarnesses(newFakeCore("home"))
	core.workspace = console.SourceWorkspace{Directory: "/src", Repository: true, Carries: true, Changes: []string{"a.go"}}
	a := dial(t, core)
	a.hello()
	id := a.command(Command{Command: "new"})
	form := a.expect(TypeForm).Form
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: map[string]string{}}})
	q := a.expect(TypeQuestion).Question
	if q.Command != id || q.Subject != "/src" || len(q.Choices) != 2 || q.Choices[0].Key != "false" {
		t.Fatalf("question %+v", q)
	}
	a.send(Message{Type: TypeAnswer, Answer: &Answer{Question: q.ID, Declined: true}})
	if r := a.result(id); r.Status != StatusEnter {
		t.Fatalf("result %+v", r)
	}
	if req := core.ranWith(); req.IncludeDirty != "false" {
		t.Fatalf("left, the dirty question answered %q", req.IncludeDirty)
	}

	core.mu.Lock()
	core.workspace = console.SourceWorkspace{Directory: "/tmp/x", Carries: true}
	core.ran = nil
	core.mu.Unlock()
	id = a.command(Command{Command: "new"})
	form = a.expect(TypeForm).Form
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: map[string]string{}}})
	q = a.expect(TypeQuestion).Question
	a.send(Message{Type: TypeAnswer, Answer: &Answer{Question: q.ID, Declined: true}})
	if r := a.result(id); r.Status != StatusCanceled || core.ranWith() != nil {
		t.Fatalf("a copy question left: %+v, ran %+v", r, core.ranWith())
	}
}

// The copy question says how big the directory is as it is counted: asked at
// once with nothing counted, then sent again, the same question, as the count
// climbs and once it is final. The walk stops with the answer.
func TestNewSaysTheCopysSizeAsItIsCounted(t *testing.T) {
	core := withHarnesses(newFakeCore("home"))
	core.workspace = console.SourceWorkspace{Directory: "/tmp/x", Carries: true}
	a := dial(t, core)
	a.hello()
	id := a.command(Command{Command: "new"})
	form := a.expect(TypeForm).Form
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: map[string]string{}}})
	q := a.expect(TypeQuestion).Question
	if q.Count != "calculating…" || q.Subject != "/tmp/x" {
		t.Fatalf("the copy question asked %+v, want nothing counted yet", q)
	}

	core.mu.Lock()
	core.total = console.DirectoryTotal{Bytes: 5 << 20, Files: 3}
	core.mu.Unlock()
	again := a.expect(TypeQuestion).Question
	if again.ID != q.ID || again.Count != "5.0 MiB in 3 files, still counting…" {
		t.Fatalf("sent again %+v, want the same question with the running count", again)
	}
	core.mu.Lock()
	core.total = console.DirectoryTotal{Bytes: 7 << 20, Files: 4, Done: true}
	core.mu.Unlock()
	final := a.expect(TypeQuestion).Question
	if final.ID != q.ID || final.Count != "7.0 MiB in 4 files" {
		t.Fatalf("sent again %+v, want the final count", final)
	}
	a.quiet(3 * console.MeasureEvery)

	a.send(Message{Type: TypeAnswer, Answer: &Answer{Question: q.ID, Key: "true"}})
	if r := a.result(id); r.Status != StatusEnter || core.ranWith().CopyDirectory != "true" {
		t.Fatalf("result %+v, ran %+v", r, core.ranWith())
	}
	core.mu.Lock()
	defer core.mu.Unlock()
	if len(core.measured) != 1 || core.measured[0] != "/tmp/x" || core.walksStopped != 1 {
		t.Fatalf("measured %v, stopped %d: want the directory counted once and the walk stopped", core.measured, core.walksStopped)
	}
}

// A project with no default harness is asked which to make it, and the
// create goes on with it.
func TestNewAsksForADefaultHarness(t *testing.T) {
	core := withHarnesses(newFakeCore("home"))
	core.harnesses[0].Default = false
	a := dial(t, core)
	a.hello()
	id := a.command(Command{Command: "new"})
	form := a.expect(TypeForm).Form
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: map[string]string{}}})
	q := a.expect(TypeQuestion).Question
	if len(q.Choices) != 2 || q.Choices[1].Label != "codex" {
		t.Fatalf("question %+v", q)
	}
	a.send(Message{Type: TypeAnswer, Answer: &Answer{Question: q.ID, Key: q.Choices[1].Key}})
	if r := a.result(id); r.Status != StatusEnter {
		t.Fatalf("result %+v", r)
	}
	if !slices.Contains(core.called(), "harness set default h2") {
		t.Fatalf("calls %q", core.called())
	}
}

// The composer topic is the run options' summary, sent again when they
// change; the session opens on the last window's draft, and a draft the app
// says is kept.
func TestTheComposerAndItsDraft(t *testing.T) {
	core := withHarnesses(newFakeCore("home"))
	a := dial(t, core)
	a.hello()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicComposer}})
	// The project's harness is said before anything is created, unchosen.
	if snap := a.expect(TypeSnapshot).Snapshot; snap.Composer == nil || len(snap.Composer.Chips) != 1 ||
		snap.Composer.Chips[0].Text != "claude" || snap.Composer.Chips[0].Chosen {
		t.Fatalf("composer %+v: want the project's harness, unchosen", snap.Composer)
	}
	id := a.command(Command{Command: "options"})
	form := a.expect(TypeForm).Form
	a.send(Message{Type: TypeSubmit, Submit: &Submit{Form: form.ID, Answers: map[string]string{"harness": "codex", "dirty": "include"}}})
	// The composer can send its changed options before the result arrives.
	var result *Result
	var snap *Snapshot
	for result == nil || snap == nil {
		switch m := a.message(); m.Type {
		case TypeResult:
			if m.Result.ID == id {
				result = m.Result
			}
		case TypeSnapshot:
			if m.Snapshot.Sub == 1 {
				snap = m.Snapshot
			}
		}
	}
	if result.Status != StatusDone {
		t.Fatalf("options: %+v", result)
	}
	if snap.Composer == nil {
		t.Fatalf("composer missing: %+v", snap)
	}
	var chips []string
	for _, c := range snap.Composer.Chips {
		chips = append(chips, c.Text)
	}
	if !slices.Equal(chips, []string{"codex", "+dirty"}) || !snap.Composer.Chips[0].Chosen {
		t.Fatalf("chips %+v", snap.Composer.Chips)
	}

	a.send(Message{Type: TypeDraft, Draft: &Draft{Prompt: "half a thought"}})
	eventually(t, "the draft is kept", func() bool {
		core.mu.Lock()
		defer core.mu.Unlock()
		return slices.Equal(core.drafts, []string{"half a thought"})
	})
}

// A harness's own way out typed into the composer leaves the window: new
// enters nothing and creates nothing.
func TestNewWithExitLeaves(t *testing.T) {
	core := withHarnesses(newFakeCore("home"))
	a := dial(t, core)
	a.hello()
	id := a.command(Command{Command: "new", Args: Args{Prompt: " /exit "}})
	if r := a.result(id); r.Status != StatusEnter || r.Box != "" {
		t.Fatalf("result %+v", r)
	}
	if core.ranWith() != nil {
		t.Fatal("/exit created a box")
	}
}

// A folder dropped on the composer is options with a source: checked as a
// source typed on a form is, it sets the Source row without asking anything,
// the composer's line names it, and the next create's card opens on it. One
// the creation path refuses is refused with why, and moves nothing.
func TestOptionsWithADroppedFolder(t *testing.T) {
	core := withHarnesses(newFakeCore("home"))
	a := dial(t, core)
	a.hello()
	a.send(Message{Type: TypeSubscribe, Subscribe: &Subscribe{Sub: 1, Topic: TopicComposer}})
	a.expect(TypeSnapshot)

	id := a.command(Command{Command: "options", Args: Args{Source: "nowhere"}})
	if r := a.result(id); r.Status != StatusFailed || !strings.Contains(r.Why, "not a directory") {
		t.Fatalf("a folder the creation path refuses: %+v", r)
	}

	id = a.command(Command{Command: "options", Args: Args{Source: "app"}})
	var r *Result
	var chips []Chip
	for r == nil || chips == nil {
		switch m := a.message(); m.Type {
		case TypeForm:
			t.Fatal("a dropped folder asked the run options' form")
		case TypeResult:
			r = m.Result
		case TypeSnapshot:
			chips = m.Snapshot.Composer.Chips
		}
	}
	if r.ID != id || r.Status != StatusDone || !strings.Contains(r.Note, "/abs/app") {
		t.Fatalf("result %+v", r)
	}
	if !slices.ContainsFunc(chips, func(c Chip) bool { return c.Text == "/abs/app" && c.Chosen }) {
		t.Fatalf("the composer's line does not name the folder: %+v", chips)
	}

	a.command(Command{Command: "new"})
	card := a.expect(TypeForm).Form
	if source, _ := formField(card, console.SourceField); source.Value != "/abs/app" {
		t.Fatalf("the card opens on %q, not the dropped folder", source.Value)
	}
}
