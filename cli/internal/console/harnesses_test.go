package console

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// A harness's menu lists every action, and the ones that do not apply say
// why: a variant is not a parent, the default is already it, nothing to
// edit without files.
func TestHarnessActionsSayWhyNot(t *testing.T) {
	t.Parallel()
	why := func(h Harness) map[HarnessAction]string {
		out := map[HarnessAction]string{}
		for _, o := range h.Actions() {
			if o.Enabled != (o.Why == "") {
				t.Fatalf("%s: enabled %v with why %q", o.Action, o.Enabled, o.Why)
			}
			out[o.Action] = o.Why
		}
		return out
	}
	plain := why(Harness{ID: "h1", Name: "Claude", Files: []HarnessFile{{Path: "a"}}})
	for _, a := range []HarnessAction{HarnessActionVariant, HarnessActionDefault, HarnessActionConfig, HarnessActionFiles, HarnessActionReset} {
		if w, ok := plain[a]; !ok || w != "" {
			t.Fatalf("%s: offered %v, why %q; want it offered", a, ok, w)
		}
	}
	refused := why(Harness{ID: "h2", Name: "Work", Parent: "h1", Default: true})
	for _, a := range []HarnessAction{HarnessActionVariant, HarnessActionDefault, HarnessActionFiles} {
		if refused[a] == "" {
			t.Fatalf("%s should be refused for a default variant with no files", a)
		}
	}
	if _, ok := (Harness{}).Offer("nothing"); ok {
		t.Fatal("an action no menu lists is no offer")
	}
}

// A reset asks yes or no; with a person's own files it asks which reset,
// keeping them first, and reads each answer as its verb.
func TestResetAsksWhetherToKeepYourFiles(t *testing.T) {
	t.Parallel()
	plain := Harness{Name: "Claude"}
	if q := plain.ResetQuestion(); len(q.Choices) != 0 || !strings.Contains(q.Body, "Reset Claude") {
		t.Fatalf("question = %+v, want a yes or no about Claude", q)
	}
	if v, ok := plain.ResetVerb(AnswerYes); !ok || v != HarnessReset {
		t.Fatalf("yes = %q %v", v, ok)
	}
	if _, ok := plain.ResetVerb(AnswerNo); ok {
		t.Fatal("no resets nothing")
	}

	mine := Harness{Name: "Claude", Files: []HarnessFile{{Path: "x", Source: HarnessFileOverride}}}
	q := mine.ResetQuestion()
	if len(q.Choices) != 2 || q.Choices[0].Key != string(HarnessReset) {
		t.Fatalf("choices = %+v, want keeping your files first", q.Choices)
	}
	for _, c := range q.Choices {
		if v, ok := mine.ResetVerb(c.Key); !ok || string(v) != c.Key {
			t.Fatalf("%q reads as %q %v", c.Key, v, ok)
		}
	}
	if _, ok := mine.ResetVerb(AnswerYes); ok {
		t.Fatal("a pick of one is not answered yes")
	}
}

// The card says what the harness is and runs, each secret and what answers
// it, and each file with its notes and its first lines.
func TestHarnessCardRows(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("line\n", HarnessCardFileLines+3)
	h := Harness{
		ID: "hc_1", Name: "Claude", Slug: "claude", Default: true, BuiltIn: true, Bundle: "b:1", Run: []string{"claude", "--yes"},
		Files:   []HarnessFile{{Path: ".claude.json", Content: long, Source: HarnessFileSaved, Shadows: []HarnessFileSource{HarnessFileFromBundle}}},
		Updated: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC),
	}
	secrets := []HarnessSecret{
		{Name: "API_KEY", Required: true, Declared: true, SecretID: "sec_1", SecretType: "token", SecretName: "anthropic"},
		{Name: "OTHER", Declared: true},
	}
	rows := HarnessCard(h, secrets)
	text := func(r CardRow) string {
		var b strings.Builder
		for _, s := range r.Spans {
			b.WriteString(s.Text)
		}
		return b.String()
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.Label+": "+text(r))
	}
	want := []string{
		"Role: ★ default  built-in",
		"Run as: claude",
		"ID: hc_1",
		"Bundle: b:1",
		"Run: claude --yes",
		"Secrets: API_KEY (required) → sec_1 (token, anthropic)",
		": OTHER → nothing bound",
		"Files: .claude.json  (saved from a discobox · over the image's)",
		"Updated: 2026-11-10T00:00:00Z",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("card =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if rows[0].Spans[0].Tone != CardStandout || rows[4].Spans[0].Tone != CardCommand {
		t.Fatalf("tones = %+v, %+v", rows[0].Spans, rows[4].Spans)
	}
	file := rows[7]
	if len(file.Lines) != HarnessCardFileLines || file.More != 3 {
		t.Fatalf("file shows %d lines and %d more, want %d and 3", len(file.Lines), file.More, HarnessCardFileLines)
	}
}

// The review offers a save only of something to save, and a discard only of
// a pending candidate.
func TestHarnessChangeOffers(t *testing.T) {
	t.Parallel()
	enabled := func(c HarnessChanges) []HarnessChange {
		var out []HarnessChange
		for _, o := range c.Offers() {
			if o.Enabled {
				out = append(out, o.Change)
			}
		}
		return out
	}
	files := []HarnessChangedFile{{Path: "a"}}
	for _, tc := range []struct {
		c    HarnessChanges
		want []HarnessChange
	}{
		{HarnessChanges{CandidateID: "c1", State: "pending", Files: files}, []HarnessChange{HarnessChangeSave, HarnessChangeDiscard}},
		{HarnessChanges{CandidateID: "c1", State: "pending"}, []HarnessChange{HarnessChangeDiscard}},
		{HarnessChanges{CandidateID: "c1", State: "saved-handoff-pending"}, []HarnessChange{HarnessChangeSave}},
		{HarnessChanges{State: "pending", Files: files}, nil},
		{HarnessChanges{CandidateID: "c1", State: "saved", Files: files}, nil},
	} {
		if got := enabled(tc.c); !slices.Equal(got, tc.want) {
			t.Errorf("%+v offers %v, want %v", tc.c, got, tc.want)
		}
	}
}

type changeSource struct {
	DataSource
	changes HarnessChanges
	did     []string
}

func (s *changeSource) HarnessChanges(context.Context, string) (HarnessChanges, error) {
	return s.changes, nil
}

func (s *changeSource) SaveHarnessChanges(_ context.Context, box, candidate string) (string, error) {
	s.did = append(s.did, "save "+box+" "+candidate)
	return "saved", nil
}

func (s *changeSource) DiscardHarnessChanges(_ context.Context, box, candidate string) (string, error) {
	s.did = append(s.did, "discard "+box+" "+candidate)
	return "discarded", nil
}

// A change names the candidate that was reviewed, and is refused when the
// box has moved on from it or the review would not offer it.
func TestRunHarnessChangeNamesTheReviewedCandidate(t *testing.T) {
	t.Parallel()
	src := &changeSource{changes: HarnessChanges{CandidateID: "c1", State: "pending", Files: []HarnessChangedFile{{Path: "a"}}}}
	if state, err := RunHarnessChange(t.Context(), src, "b1", HarnessChangeSave, "c1"); err != nil || state != "saved" {
		t.Fatalf("save = %q, %v", state, err)
	}
	if state, err := RunHarnessChange(t.Context(), src, "b1", HarnessChangeDiscard, "c1"); err != nil || state != "discarded" {
		t.Fatalf("discard = %q, %v", state, err)
	}
	for _, tc := range []struct {
		name      string
		changes   HarnessChanges
		candidate string
	}{
		{"none reviewed", src.changes, ""},
		{"a newer candidate", src.changes, "c0"},
		{"nothing to save", HarnessChanges{CandidateID: "c1", State: "pending"}, "c1"},
	} {
		src.changes = tc.changes
		if _, err := RunHarnessChange(t.Context(), src, "b1", HarnessChangeSave, tc.candidate); err == nil {
			t.Errorf("%s: a save should be refused", tc.name)
		}
	}
	if !slices.Equal(src.did, []string{"save b1 c1", "discard b1 c1"}) {
		t.Fatalf("did %v", src.did)
	}
}

type harnessSource struct {
	DataSource
	listed []Harness
	did    []string
}

func (s *harnessSource) Harnesses(context.Context, string) ([]Harness, error) { return s.listed, nil }

func (s *harnessSource) DoHarness(_ context.Context, _ string, verb HarnessVerb, id string) error {
	s.did = append(s.did, string(verb)+" "+id)
	return nil
}

func (s *harnessSource) EditHarnessFile(_ context.Context, _, id, path string, _ *Narration) (bool, error) {
	s.did = append(s.did, "edit "+id+" "+path)
	return true, nil
}

// An action is taken on the harness as listed now, refused in its offer's
// words: a verb, an edit of a file it carries, nothing on one gone.
func TestHarnessActionsAreReadAgainAndRefused(t *testing.T) {
	t.Parallel()
	src := &harnessSource{listed: []Harness{{ID: "h1", Name: "Claude", Default: true, Files: []HarnessFile{{Path: "a.json"}}}, {ID: "h2", Name: "Codex"}}}
	if note, err := RunHarnessVerb(t.Context(), src, "", "h2", HarnessSetDefault); err != nil || note != "Codex is now the default" {
		t.Fatalf("default = %q, %v", note, err)
	}
	if _, err := RunHarnessVerb(t.Context(), src, "", "h1", HarnessSetDefault); err == nil || err.Error() != "cannot default Claude: already the default" {
		t.Fatalf("default on the default = %v", err)
	}
	if note, err := EditHarness(t.Context(), src, "", "h1", "a.json", nil); err != nil || note != "updated a.json" {
		t.Fatalf("edit = %q, %v", note, err)
	}
	for _, refused := range []func() error{
		func() error { _, err := EditHarness(t.Context(), src, "", "h1", "nope", nil); return err },
		func() error { _, err := EditHarness(t.Context(), src, "", "h2", "a.json", nil); return err },
		func() error { _, err := RunHarnessVerb(t.Context(), src, "", "gone", HarnessReset); return err },
	} {
		if refused() == nil {
			t.Error("an action the core does not offer should be refused")
		}
	}
	if !slices.Equal(src.did, []string{"set default h2", "edit h1 a.json"}) {
		t.Fatalf("did %v", src.did)
	}
}

// An edit waits for the frontend to finish with the file, and is answered
// once; a feed nobody reads declines it.
func TestNarrationEdit(t *testing.T) {
	n := NewNarration()
	done := make(chan error, 1)
	go func() { done <- n.Edit(context.Background(), "/tmp/f.json", "Editing f.json") }()
	event := <-n.Events()
	if event.Editing == nil || event.Editing.Path != "/tmp/f.json" || event.Editing.Title != "Editing f.json" {
		t.Fatalf("event %+v, want the edit", event)
	}
	event.Editing.Done(nil)
	event.Editing.Done(errors.New("late")) // a second answer is not heard
	if err := <-done; err != nil {
		t.Fatalf("edit = %v", err)
	}

	go func() { done <- n.Edit(context.Background(), "/tmp/g", "g") }()
	go n.Decline(ErrNoFrontend)
	if err := <-done; !errors.Is(err, ErrNoFrontend) {
		t.Fatalf("declined edit = %v", err)
	}
	n.Close()

	var none *Narration
	if err := none.Edit(context.Background(), "/tmp/h", "h"); !errors.Is(err, ErrNoFrontend) {
		t.Fatalf("an edit nobody watches = %v", err)
	}
}
