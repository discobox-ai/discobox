package bridge

import (
	"errors"
	"slices"
	"testing"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// The window opens on the appearance the last one was picked to draw in. The
// theme command asks the core's question, opening on a change from the one
// drawn now, keeps the answer for the next window and hands it to the app;
// the question asked again opens on a change from the answer.
func TestThemeIsAskedKeptAndHandedToTheApp(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	core.appearance = console.AppearanceDark
	a := dial(t, core)
	if w := a.hello(); w.Session.Appearance != console.AppearanceDark {
		t.Fatalf("the window opens on %q", w.Session.Appearance)
	}

	theme := a.command(Command{Command: "theme"})
	q := a.expect(TypeQuestion).Question
	if !offersEveryAppearance(q.Choices) {
		t.Fatalf("the question %+v", q)
	}
	if q.Choices[1].Why == "" || q.Choices[0].Why != "" || q.Choices[2].Why != "" {
		t.Errorf("the question does not open on a change from dark: %+v", q.Choices)
	}
	a.send(Message{Type: TypeAnswer, Answer: &Answer{Question: q.ID, Key: "light"}})
	if r := a.result(theme); r.Status != StatusDone || r.Appearance != console.AppearanceLight || r.Note != "" {
		t.Fatalf("theme %+v", r)
	}
	if !slices.Contains(core.called(), "appearance light") {
		t.Errorf("the answer was not kept: %v", core.called())
	}

	again := a.command(Command{Command: "theme"})
	q = a.expect(TypeQuestion).Question
	if q.Choices[2].Why == "" {
		t.Errorf("the question asked again does not open on a change from light: %+v", q.Choices)
	}
	a.send(Message{Type: TypeAnswer, Answer: &Answer{Question: q.ID, Declined: true}})
	if r := a.result(again); r.Status != StatusCanceled || r.Appearance != "" {
		t.Fatalf("theme declined %+v", r)
	}
}

// An answer that cannot be kept is still drawn in, and says why it will not
// last.
func TestAThemeNotKeptIsStillDrawn(t *testing.T) {
	core := newFakeCore("home", running("b1"))
	core.appearanceErr = errors.New("read-only state")
	a := dial(t, core)
	a.hello()
	theme := a.command(Command{Command: "theme"})
	q := a.expect(TypeQuestion).Question
	a.send(Message{Type: TypeAnswer, Answer: &Answer{Question: q.ID, Key: "system"}})
	r := a.result(theme)
	if r.Status != StatusDone || r.Appearance != console.AppearanceSystem || r.Note != "the theme is not kept for the next window: read-only state" {
		t.Fatalf("theme %+v", r)
	}
}

func offersEveryAppearance(choices []Choice) bool {
	var keys []string
	for _, c := range choices {
		keys = append(keys, c.Key)
	}
	return slices.Equal(keys, []string{"system", "dark", "light"})
}
