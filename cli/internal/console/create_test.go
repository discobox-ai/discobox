package console

import (
	"context"
	"errors"
	"fmt"
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
	q, ask, err := defaultHarnessQuestion(RunRequest{}, harnesses, true)
	if err != nil || !ask || len(q.Choices) != 1 {
		t.Fatalf("question = %+v, ask %v, err %v", q, ask, err)
	}
	if h, ok := chosenHarness(harnesses, q.Choices[0].Key); !ok || h.ID != "c" {
		t.Fatalf("chosen = %+v", h)
	}
	if _, ask, _ := defaultHarnessQuestion(RunRequest{Harness: "codex"}, harnesses, true); ask {
		t.Fatal("a create that names a harness was asked for one")
	}
	if _, ask, _ := defaultHarnessQuestion(RunRequest{}, harnesses, false); ask {
		t.Fatal("asked before the harnesses were listed")
	}
	if _, _, err := defaultHarnessQuestion(RunRequest{}, harnesses[:1], true); !errors.Is(err, ErrNoHarness) {
		t.Fatalf("err = %v, want ErrNoHarness", err)
	}
}

// The working tree's question: a repository's dirty work is excluded when it
// is left, a directory in no repository is not created at all.
func TestTheWorkspaceQuestion(t *testing.T) {
	t.Parallel()
	ws := SourceWorkspace{Directory: "/src/app", Repository: true, Carries: true, Changes: []string{"a", "b", "c", "d"}}
	q, ask := askAboutWorkspace(RunRequest{}, ws)
	if !ask || !q.Repository || q.Subject != "/src/app" || !strings.Contains(q.Body, "4 uncommitted changes (a, b, c and 1 more)") {
		t.Fatalf("question = %+v", q)
	}
	if key, ok := q.Dismissed(); !ok || q.Answer(RunRequest{}, key).IncludeDirty != "false" {
		t.Fatalf("dismissed = %q %v", key, ok)
	}
	if _, ask := askAboutWorkspace(RunRequest{IncludeDirty: "true"}, ws); ask {
		t.Fatal("asked again what the request answered")
	}
	copyQ, ask := askAboutWorkspace(RunRequest{}, SourceWorkspace{Directory: "/tmp/x", Carries: true})
	if !ask || copyQ.Repository || copyQ.Choices[0].Key != "true" {
		t.Fatalf("copy question = %+v", copyQ)
	}
	if _, ok := copyQ.Dismissed(); ok {
		t.Fatal("leaving the copy question creates anyway")
	}
	if copyQ.Answer(RunRequest{}, "false").CopyDirectory != "false" {
		t.Fatal("the copy question's answer is not CopyDirectory")
	}
	if _, ask := askAboutWorkspace(RunRequest{}, SourceWorkspace{Repository: true}); ask {
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

// creating is a DataSource for Create: the harnesses listed, the working
// tree read, and what was asked of it, in order.
type creating struct {
	DataSource
	harnesses []Harness
	workspace SourceWorkspace
	calls     []string
}

func (c *creating) Harnesses(context.Context, string) ([]Harness, error) { return c.harnesses, nil }

func (c *creating) DoHarness(_ context.Context, server string, verb HarnessVerb, id string) error {
	c.calls = append(c.calls, fmt.Sprintf("%s %s on %q", verb, id, server))
	for i := range c.harnesses {
		c.harnesses[i].Default = c.harnesses[i].ID == id
	}
	return nil
}

func (c *creating) Workspace(context.Context, string) (SourceWorkspace, error) {
	c.calls = append(c.calls, "workspace")
	return c.workspace, nil
}

func (c *creating) Run(_ context.Context, req RunRequest, _ *Narration) (Sandbox, error) {
	c.calls = append(c.calls, fmt.Sprintf("run harness=%q dirty=%q copy=%q", req.Harness, req.IncludeDirty, req.CopyDirectory))
	return Sandbox{ID: "sbx_1"}, nil
}

// answering reads a create's narration, answering each question with the
// next of keys ("" declines it, as leaving it does), and returns the titles
// asked and the steps said.
func answering(n *Narration, keys ...string) (asked, steps *[]string, done chan struct{}) {
	asked, steps, done = &[]string{}, &[]string{}, make(chan struct{})
	go func() {
		defer close(done)
		for e := range n.Events() {
			switch {
			case e.Asking != nil:
				*asked = append(*asked, e.Asking.Title)
				if len(keys) == 0 || keys[0] == "" {
					e.Asking.Decline(context.Canceled)
				} else {
					e.Asking.Answer(keys[0])
				}
				if len(keys) > 0 {
					keys = keys[1:]
				}
			case e.Progress != nil:
				*steps = append(*steps, e.Progress.Text)
			}
		}
	}()
	return asked, steps, done
}

// A create asks which harness first, makes it the default on the server the
// create goes to, then reads the working tree and asks about it, and creates
// with both answers; leaving the repository's question starts from the last
// commit, leaving the harness or the copy question creates nothing.
func TestCreateAsksItsQuestionsInOrder(t *testing.T) {
	t.Parallel()
	listed := func() []Harness {
		return []Harness{{ID: "s", Name: "shell", Slug: "shell", Shell: true}, {ID: "c", Name: "codex", Slug: "codex"}}
	}
	dirty := SourceWorkspace{Directory: "/src/app", Repository: true, Carries: true, Changes: []string{"a.go"}}
	copying := SourceWorkspace{Directory: "/tmp/x", Carries: true}
	for _, tc := range []struct {
		name      string
		workspace SourceWorkspace
		keys      []string
		asked     []string
		calls     []string
		defaulted bool
		err       error
	}{
		{"both answered", dirty, []string{"1", "true"},
			[]string{"Which harness should this project run?", "Uncommitted changes"},
			[]string{`set default c on "west"`, "workspace", `run harness="" dirty="true" copy=""`}, true, nil},
		{"the repository's left", dirty, []string{"1", ""},
			[]string{"Which harness should this project run?", "Uncommitted changes"},
			[]string{`set default c on "west"`, "workspace", `run harness="" dirty="false" copy=""`}, true, nil},
		{"the harness left", dirty, []string{""},
			[]string{"Which harness should this project run?"}, nil, false, ErrTurnedDown},
		{"the copy left", copying, []string{"1", ""},
			[]string{"Which harness should this project run?", "Copy this directory?"},
			[]string{`set default c on "west"`, "workspace"}, true, ErrTurnedDown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := &creating{harnesses: listed(), workspace: tc.workspace}
			n := NewNarration()
			asked, steps, done := answering(n, tc.keys...)
			made, err := Create(t.Context(), ds, CreateRun{Server: "west", Harnesses: listed(), Listed: true}, n)
			n.Close()
			<-done
			if !errors.Is(err, tc.err) || made.Defaulted != tc.defaulted {
				t.Fatalf("made %+v, err %v; want defaulted %v, err %v", made, err, tc.defaulted, tc.err)
			}
			if !slices.Equal(*asked, tc.asked) {
				t.Fatalf("asked %q, want %q", *asked, tc.asked)
			}
			if !slices.Equal(ds.calls, tc.calls) {
				t.Fatalf("calls %q, want %q", ds.calls, tc.calls)
			}
			if tc.err == nil && (made.Box.ID != "sbx_1" || !slices.Contains(*steps, "creating the discobox")) {
				t.Fatalf("made %+v, steps %q", made, *steps)
			}
		})
	}
}

// A create that names its harness, or whose working tree carries nothing,
// asks nothing; the copy question names the directory to count, and its
// choices, like the repository's, are its yes and its no.
func TestCreateAsksOnlyWhatIsOpen(t *testing.T) {
	t.Parallel()
	ds := &creating{workspace: SourceWorkspace{Directory: "/src/app", Repository: true}}
	n := NewNarration()
	asked, _, done := answering(n)
	_, err := Create(t.Context(), ds, CreateRun{Request: RunRequest{Harness: "codex"}, Listed: true}, n)
	n.Close()
	<-done
	if err != nil || len(*asked) != 0 {
		t.Fatalf("err %v, asked %q: want nothing asked", err, *asked)
	}

	copyQ, _ := askAboutWorkspace(RunRequest{}, SourceWorkspace{Directory: "/tmp/x", Carries: true})
	if copyQ.Measure != "/tmp/x" || copyQ.Choices[0].Means != AnswerYes || copyQ.Choices[1].Means != AnswerNo {
		t.Fatalf("copy question = %+v", copyQ)
	}
	dirtyQ, _ := askAboutWorkspace(RunRequest{}, SourceWorkspace{Directory: "/src/app", Repository: true, Carries: true})
	if dirtyQ.Measure != "" || dirtyQ.Choices[0].Means != AnswerNo || dirtyQ.Choices[1].Means != AnswerYes {
		t.Fatalf("dirty question = %+v", dirtyQ)
	}
}
