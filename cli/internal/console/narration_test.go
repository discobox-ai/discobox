package console

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

// A step the frontend has no room for is dropped, never waited on; what
// follows from the create being taken is not, nor is a question.
func TestNarrationDropsStepsNeverAcceptanceOrQuestions(t *testing.T) {
	n := NewNarration()
	for range narrationCapacity + 4 {
		n.Report("pulling")
	}
	accepted := make(chan struct{})
	go func() {
		n.Accept(context.Background(), Sandbox{ID: "sbx_1"})
		close(accepted)
	}()
	var steps int
	for steps < narrationCapacity {
		if event := <-n.Events(); event.Progress == nil || event.Progress.Text != "pulling" {
			t.Fatalf("event %+v, want a step", event)
		}
		steps++
	}
	if event := <-n.Events(); event.Accepted == nil || event.Accepted.ID != "sbx_1" {
		t.Fatalf("event %+v, want the acceptance", event)
	}
	<-accepted

	answered := make(chan string, 1)
	go func() {
		key, err := n.Ask(context.Background(), Question{Title: "pick", Choices: []Choice{{Key: "a"}, {Key: "b"}}})
		if err != nil {
			t.Error(err)
		}
		answered <- key
	}()
	event := <-n.Events()
	if event.Asking == nil || event.Asking.Title != "pick" {
		t.Fatalf("event %+v, want the question", event)
	}
	event.Asking.Answer("b")
	event.Asking.Answer("a") // a second answer is not heard
	if key := <-answered; key != "b" {
		t.Fatalf("answer %q", key)
	}
	n.Close()
}

// An acceptance nobody reads gives up with its create's context.
func TestNarrationAcceptEndsWithItsContext(*testing.T) {
	n := NewNarration()
	for range narrationCapacity {
		n.Report("pulling")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		n.Accept(ctx, Sandbox{ID: "sbx_1"})
		close(done)
	}()
	cancel()
	<-done
	n.Close()
}

func TestNarrationWarnIsAWarning(t *testing.T) {
	n := NewNarration()
	n.Warn("left out")
	n.Close()
	if event := <-n.Events(); event.Progress == nil || *event.Progress != (Progress{Text: "left out", Warning: true}) {
		t.Fatalf("event %+v", event)
	}
}

// Nobody watching: steps go nowhere, questions and handovers are declined.
func TestNilNarrationAsksNobody(t *testing.T) {
	var n *Narration
	n.Report("step")
	n.Warn("warning")
	n.Accept(context.Background(), Sandbox{})
	if _, err := n.Ask(context.Background(), Question{}); !errors.Is(err, ErrNoFrontend) {
		t.Fatalf("ask = %v", err)
	}
	if yes, err := n.Confirm(context.Background(), Question{}); yes || !errors.Is(err, ErrNoFrontend) {
		t.Fatalf("confirm = %v, %v", yes, err)
	}
	if err := n.Handover(context.Background(), exec.CommandContext(t.Context(), "true")); !errors.Is(err, ErrNoFrontend) {
		t.Fatalf("handover = %v", err)
	}
}

// A question nobody answers lets the command go when its context ends.
func TestNarrationAskEndsWithItsContext(t *testing.T) {
	n := NewNarration()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := n.Ask(ctx, Question{})
		done <- err
	}()
	<-n.Events()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("ask = %v", err)
	}
	n.Close()
}

// A frontend that can ask nothing declines every question and handover,
// until the command is done.
func TestNarrationDecline(t *testing.T) {
	n := NewNarration()
	why := errors.New("cannot ask here")
	go n.Decline(why)
	if yes, err := n.Confirm(context.Background(), Question{}); yes || !errors.Is(err, why) {
		t.Fatalf("confirm = %v, %v", yes, err)
	}
	if err := n.Handover(context.Background(), exec.CommandContext(t.Context(), "true")); !errors.Is(err, why) {
		t.Fatalf("handover = %v", err)
	}
	n.Report("not drawn")
	n.Close()
}

// Dispatch asks a verb's question on the way in, and sends the rest.
func TestDispatchAsksBeforePurging(t *testing.T) {
	r := NewRegistry(servers{})
	archived := Sandbox{ID: "sbx_one", State: StateArchived}
	if got := Dispatch(r, Frontend[string](&recorder{}), Context{}, "purge", []Sandbox{archived}); got != "confirm purge sbx_one" {
		t.Errorf("purge dispatched to %q", got)
	}
	running := Sandbox{ID: "sbx_one", State: StateRunning, HasRuntime: true}
	if got := Dispatch(r, Frontend[string](&recorder{}), Context{}, "archive", []Sandbox{running}); got != "do archive sbx_one" {
		t.Errorf("archive dispatched to %q", got)
	}
}

func TestConfirmationAsksBeforePurgingOnly(t *testing.T) {
	one := []Sandbox{{ID: "sbx_1", Name: "alpha"}}
	q, asks := Confirmation(VerbPurge, one)
	if !asks || !q.DefaultNo || len(q.Choices) != 0 || q.Body != "Purge alpha? The disk and everything on it goes, and unarchive cannot bring it back." {
		t.Fatalf("purge asks %v: %+v", asks, q)
	}
	if q, _ := Confirmation(VerbPurge, append(one, Sandbox{ID: "sbx_2"})); q.Body[:len("Purge 2 boxes selected?")] != "Purge 2 boxes selected?" {
		t.Fatalf("two: %q", q.Body)
	}
	for _, verb := range []Verb{VerbArchive, VerbStart, VerbStop, VerbRepair, VerbUpgrade, VerbUnarchive} {
		if _, asks := Confirmation(verb, one); asks {
			t.Errorf("%s asks", verb)
		}
	}
}

// An edited file keeps a text format's extension and takes .txt for any
// other: the desktop opens it with the system's handler for its type, and the
// path is a harness bundle's or a tool declaration's to choose.
func TestEditExtensionIsATextFormats(t *testing.T) {
	for path, want := range map[string]string{
		".claude.json":          ".json",
		"config/Settings.TOML":  ".toml",
		"setup.cmd":             ".txt",
		"launch.desktop":        ".txt",
		"settings.json&calc":    ".txt",
		"page.html":             ".txt",
		"notes.md":              ".txt",
		"layout.xml":            ".txt",
		"noext":                 ".txt",
		".config/opencode.yaml": ".yaml",
	} {
		if got := EditExtension(path); got != want {
			t.Errorf("EditExtension(%q) = %q, want %q", path, got, want)
		}
	}
}
