package console

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func createOptions() *RunOptions {
	o := NewRunOptions(Session{Directory: "/src/app", Branch: "main", Home: "/home/u", Servers: []string{"local"}})
	o.SetHarnesses([]Harness{{ID: "h1", Name: "claude", Slug: "claude", Default: true}, {ID: "h2", Name: "codex", Slug: "codex"}})
	o.SetSources([]Source{{Value: "/src/other", OriginKey: "o1"}})
	return o
}

func fieldKeys(fields []Field) []string {
	keys := make([]string, 0, len(fields))
	for _, f := range fields {
		keys = append(keys, f.Key)
	}
	return keys
}

func field(fields []Field, key string) (Field, bool) {
	for _, f := range fields {
		if f.Key == key {
			return f, true
		}
	}
	return Field{}, false
}

// The create form is the card: its sources, every one the Source row offers
// and one typed, then the card's rows and no others.
func TestTheCreateFormIsTheCard(t *testing.T) {
	t.Parallel()
	o := createOptions()
	form := o.CreateForm("fix it", nil)

	keys := fieldKeys(form.Fields)
	want := []string{SourceField, TypedSourceKey, "harness", "projectSkills", "userSkills", OptionItemKey(OptSkills, 0), OptionItemKey(OptInclude, 0)}
	if !slices.Equal(keys, want) {
		t.Fatalf("fields = %q, want %q", keys, want)
	}
	source, _ := field(form.Fields, SourceField)
	var sources []string
	for _, c := range source.Choices {
		sources = append(sources, c.Key)
	}
	if !slices.Equal(sources, []string{"/src/app", "/src/other", SourceNone, SourceEnter}) {
		t.Fatalf("sources = %q", sources)
	}
	if source.Opens() != "/src/app" {
		t.Fatalf("the source opens on %q, want the window's directory", source.Opens())
	}
	harness, _ := field(form.Fields, "harness")
	if harness.Choices[0].Label != "auto (claude)" {
		t.Fatalf("the card's harness leads with %q, want the default said as auto", harness.Choices[0].Label)
	}
	typed, _ := field(form.Fields, TypedSourceKey)
	if typed.Applies(form.Opening()) {
		t.Fatal("the typed source applies before another… is chosen")
	}
}

// The options form is every row there is to choose but the source.
func TestTheOptionsFormIsEveryRow(t *testing.T) {
	t.Parallel()
	o := createOptions()
	keys := fieldKeys(o.OptionsForm("", nil).Fields)
	for _, k := range []string{"harness", "dirty", "detach", "projectSkills", "userSkills", OptionItemKey(OptEnv, 0), OptionItemKey(OptSecret, 0)} {
		if !slices.Contains(keys, k) {
			t.Errorf("the options form lacks %q: %q", k, keys)
		}
	}
	if slices.Contains(keys, "pool") {
		t.Error("the options form offers a pool with nothing to choose")
	}
}

// A repeated row grows a field as its last is filled, and its answers are its
// values in order, empty ones dropped.
func TestARepeatedRowGrows(t *testing.T) {
	t.Parallel()
	o := createOptions()
	form := o.OptionsForm("", nil)
	a := form.Opening()
	a[OptionItemKey(OptEnv, 0)] = "A=1"
	if _, ok := field(form.FieldsFor(a), OptionItemKey(OptEnv, 1)); !ok {
		t.Fatal("a filled env field grew no field after it")
	}
	a[OptionItemKey(OptEnv, 1)] = "  "
	a[OptionItemKey(OptEnv, 2)] = "B"
	got, refusal := form.Answer(a)
	if refusal != nil {
		t.Fatal(refusal)
	}
	if !slices.Equal(got.Items[OptEnv], []string{"A=1", "B"}) {
		t.Fatalf("env = %q", got.Items[OptEnv])
	}
	o.TakeOptions(got)
	if req := o.Request("", ""); !slices.Equal(req.Env, []string{"A=1", "B"}) {
		t.Fatalf("the request's env = %q", req.Env)
	}
}

// A typed source is checked as the card is submitted, and refused against the
// field it was typed in; an added source is checked too. The note says what
// the answers would run without checking anything.
func TestATypedSourceIsCheckedOnSubmit(t *testing.T) {
	t.Parallel()
	o := createOptions()
	var checked []string
	resolve := func(s string) (Source, error) {
		checked = append(checked, s)
		if s == "nowhere" {
			return Source{}, errors.New("nowhere is not a directory")
		}
		return Source{Value: "/abs/" + s}, nil
	}
	form := o.CreateForm("go", resolve)
	a := form.Opening()
	a[SourceField] = SourceEnter
	a[TypedSourceKey] = "nowhere"
	if note := form.NoteFor(a); !strings.Contains(note, "run nowhere -p go") {
		t.Fatalf("note = %q", note)
	}
	if len(checked) != 0 {
		t.Fatalf("the note checked %q", checked)
	}
	if _, refusal := form.Answer(a); refusal == nil || refusal.Field != TypedSourceKey {
		t.Fatalf("refusal = %v, want one against the typed source", refusal)
	}
	a[TypedSourceKey] = "there"
	a[OptionItemKey(OptInclude, 0)] = "nowhere"
	if _, refusal := form.Answer(a); refusal == nil || refusal.Field != OptionItemKey(OptInclude, 0) {
		t.Fatalf("refusal = %v, want one against the added source", refusal)
	}
	delete(a, OptionItemKey(OptInclude, 0))
	got, refusal := form.Answer(a)
	if refusal != nil {
		t.Fatal(refusal)
	}
	if got.Source != "/abs/there" || got.Named == nil {
		t.Fatalf("source = %q (named %v), want the checked one", got.Source, got.Named)
	}
}

// Choosing on the form sets the rows it answers, and only those.
func TestApplyingAnAnswerSetsItsRows(t *testing.T) {
	t.Parallel()
	o := createOptions()
	form := o.OptionsForm("", nil)
	a := form.Opening()
	a["harness"] = "codex"
	a["detach"] = "on"
	got, refusal := form.Answer(a)
	if refusal != nil {
		t.Fatal(refusal)
	}
	o.TakeOptions(got)
	req := o.Request("", "")
	if req.Harness != "codex" || !req.Detach {
		t.Fatalf("request = %+v, want codex detached", req)
	}
	if chips := o.Summary(); len(chips) < 2 || chips[0].Text != "codex" || !chips[0].Chosen {
		t.Fatalf("summary = %+v", chips)
	}
}

// A project with no default asks which harness to make it, never offering
// the shell; one with nothing at all to run refuses.
func TestTheDefaultHarnessQuestion(t *testing.T) {
	t.Parallel()
	harnesses := []Harness{{ID: "s", Name: "shell", Slug: "shell", Shell: true}, {ID: "c", Name: "codex", Slug: "codex"}}
	q, ask, err := DefaultHarnessQuestion(RunRequest{}, harnesses, true)
	if err != nil || !ask || len(q.Choices) != 1 {
		t.Fatalf("question = %+v, ask %v, err %v", q, ask, err)
	}
	if h, ok := ChosenHarness(harnesses, q.Choices[0].Key); !ok || h.ID != "c" {
		t.Fatalf("chosen = %+v", h)
	}
	if _, ask, _ := DefaultHarnessQuestion(RunRequest{Harness: "codex"}, harnesses, true); ask {
		t.Fatal("a create that names a harness was asked for one")
	}
	if _, ask, _ := DefaultHarnessQuestion(RunRequest{}, harnesses, false); ask {
		t.Fatal("asked before the harnesses were listed")
	}
	if _, _, err := DefaultHarnessQuestion(RunRequest{}, harnesses[:1], true); !errors.Is(err, ErrNoHarness) {
		t.Fatalf("err = %v, want ErrNoHarness", err)
	}
}

// The working tree's question: a repository's dirty work is excluded when it
// is left, a directory in no repository is not created at all.
func TestTheWorkspaceQuestion(t *testing.T) {
	t.Parallel()
	ws := SourceWorkspace{Directory: "/src/app", Repository: true, Carries: true, Changes: []string{"a", "b", "c", "d"}}
	q, ask := AskAboutWorkspace(RunRequest{}, ws)
	if !ask || !q.Repository || q.Subject != "/src/app" || !strings.Contains(q.Body, "4 uncommitted changes (a, b, c and 1 more)") {
		t.Fatalf("question = %+v", q)
	}
	if key, ok := q.Dismissed(); !ok || q.Answer(RunRequest{}, key).IncludeDirty != "false" {
		t.Fatalf("dismissed = %q %v", key, ok)
	}
	if _, ask := AskAboutWorkspace(RunRequest{IncludeDirty: "true"}, ws); ask {
		t.Fatal("asked again what the request answered")
	}
	copyQ, ask := AskAboutWorkspace(RunRequest{}, SourceWorkspace{Directory: "/tmp/x", Carries: true})
	if !ask || copyQ.Repository || copyQ.Choices[0].Key != "true" {
		t.Fatalf("copy question = %+v", copyQ)
	}
	if _, ok := copyQ.Dismissed(); ok {
		t.Fatal("leaving the copy question creates anyway")
	}
	if copyQ.Answer(RunRequest{}, "false").CopyDirectory != "false" {
		t.Fatal("the copy question's answer is not CopyDirectory")
	}
	if _, ask := AskAboutWorkspace(RunRequest{}, SourceWorkspace{Repository: true}); ask {
		t.Fatal("a clean tree asks")
	}
}

// An answer names each choice by what it stands for, so rows rebuilt between
// the form and its answer — another harness listing — still take the choice
// that was made; and the options' answer takes its source.
func TestAnAnswerSurvivesTheRowsBeingRebuilt(t *testing.T) {
	t.Parallel()
	o := createOptions()
	form := o.OptionsForm("", nil)
	a := form.Opening()
	a["harness"] = "codex"
	a[SourceField] = SourceNone
	got, refusal := form.Answer(a)
	if refusal != nil {
		t.Fatal(refusal)
	}
	// codex moves up the row: the listing now leads with another default.
	o.SetHarnesses([]Harness{{ID: "h3", Name: "opencode", Slug: "opencode", Default: true}, {ID: "h2", Name: "codex", Slug: "codex"}, {ID: "h1", Name: "claude", Slug: "claude"}})
	o.TakeOptions(got)
	req := o.Request("", o.Opts[OptSource].Selected())
	if req.Harness != "codex" || !req.NoSource {
		t.Fatalf("request = %+v, want codex with no source", req)
	}
}
