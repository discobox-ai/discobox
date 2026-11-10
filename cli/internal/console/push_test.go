package console

import (
	"context"
	"errors"
	"testing"
)

func TestAutoPushPushedHolds(t *testing.T) {
	var w AutoPush
	refused := errors.New("lease moved")
	// A refusal outranks what moved, and is a warning.
	said := w.Pushed([]SourcePush{
		{Slug: "a", Commit: "c1", Err: refused},
		{Slug: "b", Commit: "c2", Pushed: true, Branch: "main"},
	})
	if said != (Progress{Text: "lease moved", Warning: true}) || w.Held["a"] != "c1" {
		t.Fatalf("said %+v held %v", said, w.Held)
	}
	if said := w.Pushed([]SourcePush{{Slug: "b", Commit: "c3", Pushed: true, Branch: "main"}}); said != (Progress{Text: "pushed c3 to origin/main in the discobox"}) {
		t.Errorf("pushed %+v", said)
	}
	if said := w.Pushed([]SourcePush{{Slug: "a", UpToDate: true}}); said != (Progress{}) {
		t.Errorf("a look that moved nothing says %+v", said)
	}
	if _, held := w.Held["a"]; held {
		t.Error("a source already in the origin lets its hold go")
	}
	two := w.Pushed([]SourcePush{{Slug: "a", Err: refused}, {Slug: "b", Err: refused}})
	if two.Text != "could not push a, b to the discobox's origin; `discobox push` says why" || !two.Warning {
		t.Errorf("two refused %+v", two)
	}
}

type pushSource struct {
	DataSource
	held map[string]string
	err  error
}

func (p *pushSource) PushSources(_ context.Context, _ string, held map[string]string) ([]SourcePush, error) {
	p.held = held
	return []SourcePush{{Slug: "a", Pushed: true}}, p.err
}

func TestAutoPushLook(t *testing.T) {
	w := AutoPush{Held: map[string]string{}}
	ds := &pushSource{}
	if w.Look(Sandbox{ID: "sbx"}, ds) != nil {
		t.Fatal("a box this window does not push gets no look")
	}
	w.Held["a"] = "c1"
	look := w.Look(Sandbox{ID: "sbx", Pushable: true}, ds)
	w.Held["a"] = "c2"
	if got := look(context.Background()); len(got) != 1 || ds.held["a"] != "c1" {
		t.Fatalf("got %v with holds %v, want the holds as they stood when the look was made", got, ds.held)
	}
	ds.err = errors.New("unreachable")
	if got := w.Look(Sandbox{ID: "sbx", Pushable: true}, ds)(context.Background()); got != nil {
		t.Fatalf("a whole-call failure answers nothing, got %v", got)
	}
}

func TestPushedElsewhereNamesTheBox(t *testing.T) {
	said := Progress{Text: "pushed c3 to origin/main in the discobox"}
	if got := PushedElsewhere(Sandbox{ID: "sbx", Name: "api"}, said); got.Text != "api: pushed c3 to origin/main in the discobox" {
		t.Errorf("named %q", got.Text)
	}
	if got := PushedElsewhere(Sandbox{ID: "sbx"}, Progress{Text: "lease moved", Warning: true}); got != (Progress{Text: "sbx: lease moved", Warning: true}) {
		t.Errorf("unnamed %+v, want the ID and the warning kept", got)
	}
	if got := PushedElsewhere(Sandbox{ID: "sbx", Name: "api"}, Progress{}); got != (Progress{}) {
		t.Errorf("nothing said says %+v", got)
	}
}
