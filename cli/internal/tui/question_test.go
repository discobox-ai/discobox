package tui

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

// gitQuestion is the question delivery asks about Git credentials, as the
// CLI's data source puts it (cli.gitCredentialQuestion).
var gitQuestion = console.Question{
	Title: "Git credentials",
	Body:  "forge.example/repo needs credentials to pull on the pool.",
	Choices: []console.Choice{
		{Key: "lend", Label: "Let the pool use my Git credentials", Hint: "Git's credential helper runs on this machine."},
		{Key: "push", Label: "Download on this machine and send to the pool", Hint: "This machine authenticates using its Git configuration."},
	},
}

// asked puts a question on a feed the way a running command does, and hands
// back the event the window reads and where the command's answer lands.
func asked(t *testing.T, feed narration, q console.Question) (narrationMsg, chan string, chan error) {
	t.Helper()
	keys, errs := make(chan string, 1), make(chan error, 1)
	go func() {
		key, err := feed.Ask(context.Background(), q)
		keys <- key
		errs <- err
	}()
	return narrationMsg{source: feed, event: <-feed.Events()}, keys, errs
}

func TestQuestionAnsweredDismissedAndStale(t *testing.T) {
	for _, choice := range []string{"lend", "push", "cancel", "stale"} {
		t.Run(choice, func(t *testing.T) {
			m := newTestModel(t, newFakeSource())
			feed, _ := m.narrate()
			msg, keys, errs := asked(t, feed, gitQuestion)
			if choice == "stale" {
				m.endNarration()
			}
			m.narrated(msg)
			if choice != "stale" {
				if m.dialog == nil || m.dialog.title != "Git credentials" || !strings.Contains(m.dialog.body, "needs credentials to pull on the pool.") {
					t.Fatal("question not shown")
				}
				if choice == "cancel" {
					m.dialog.cancel()
				} else {
					m.dialog.action(choice)
				}
			}
			select {
			case key := <-keys:
				err := <-errs
				switch choice {
				case "lend", "push":
					if key != choice || err != nil {
						t.Fatalf("answer = %q, %v", key, err)
					}
				default:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("answer = %q, %v; want declined", key, err)
					}
				}
			case <-time.After(time.Second):
				t.Fatal("question never released the command")
			}
			feed.Close()
		})
	}
}

// The question comes up over the wait on the create it is about, and however
// it is answered — a key or a click — the wait comes back. Taking the question
// down onto nothing shows the list, which a window that is one command's own
// never shows.
func TestQuestionGoesBackToTheWait(t *testing.T) {
	for _, how := range []string{"esc", "q", "enter", "click"} {
		t.Run(how, func(t *testing.T) {
			m := newTestModel(t, newFakeSource())
			wait := m.waitDialog(creatingTitle, "delivering the source")
			m.dialog = wait
			feed, _ := m.narrate()
			msg, keys, _ := asked(t, feed, gitQuestion)
			m.narrated(msg)
			if m.dialog == wait {
				t.Fatal("question not shown")
			}
			switch how {
			case "click":
				m.pressDialogItem(0)
			default:
				m.updateKey(keyPress(how))
			}
			select {
			case <-keys:
			case <-time.After(time.Second):
				t.Fatal("question never released the command")
			}
			if m.dialog != wait {
				t.Fatalf("dialog = %+v, want the wait back", m.dialog)
			}
			feed.Close()
		})
	}
}

func TestChoiceHintFollowsSelectionAndHoverBeforeAnswer(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	feed, _ := m.narrate()
	q := gitQuestion
	q.Choices = append([]console.Choice(nil), q.Choices...)
	q.Choices[0].Hint = strings.Repeat("Git's credential helper runs on this machine. ", 20)
	msg, keys, errs := asked(t, feed, q)
	m.narrated(msg)
	m.screenWidth, m.height = 70, 18
	view := m.View().Content
	if !strings.Contains(view, "forge.example/repo") || !strings.Contains(view, "Git's credential helper") {
		t.Fatalf("initial hint missing: %s", view)
	}
	m.dialog.update(keyPress("down"))
	view = m.View().Content
	if !strings.Contains(view, "This machine authenticates") {
		t.Fatalf("second hint missing: %s", view)
	}
	m.dialog.update(keyPress("up"))
	m.View()
	before := m.dialog.offset
	m.dialog.update(keyPress("pgdown"))
	if m.dialog.offset <= before {
		t.Fatal("hint cannot scroll on a small terminal")
	}
	m.View()
	for _, z := range m.zones.marks {
		if z.what.kind == hitDialogItem && z.what.idx == 1 {
			m.hover(tea.MouseMotionMsg{X: z.x, Y: z.y, Button: tea.MouseNone})
			break
		}
	}
	if m.dialog.cursor != 1 {
		t.Fatal("hover did not preview the second choice")
	}
	select {
	case <-keys:
		t.Fatal("preview answered the question")
	default:
	}
	m.dialog.cancel()
	<-keys
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel = %v", err)
	}
	feed.Close()
}

// A handover from an operation the window has moved on from is never run: the
// terminal is not that operation's to take.
func TestStaleHandoverIsRefused(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	feed, _ := m.narrate()
	done := make(chan error, 1)
	go func() { done <- feed.Handover(context.Background(), exec.CommandContext(t.Context(), "true")) }()
	msg := narrationMsg{source: feed, event: <-feed.Events()}
	m.endNarration()
	m.narrated(msg)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("stale handover = %v", err)
	}
	feed.Close()
}

// A choice left with q, as with Esc, tells the command it was dismissed:
// nothing is left waiting on a card nobody can see.
func TestQuestionLeftWithQIsDeclined(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	feed, _ := m.narrate()
	msg, keys, errs := asked(t, feed, gitQuestion)
	m.narrated(msg)
	m.updateKey(keyPress("q"))
	select {
	case <-keys:
		if err := <-errs; !errors.Is(err, context.Canceled) {
			t.Fatalf("q = %v, want declined", err)
		}
	case <-time.After(time.Second):
		t.Fatal("q left the command waiting")
	}
	feed.Close()
}

// A yes or no asked while a command runs is answered: yes, or no however it
// is left — never a dismissal the command would read as canceled.
func TestYesOrNoIsAnswered(t *testing.T) {
	for how, want := range map[string]string{"y": console.AnswerYes, "n": console.AnswerNo, "esc": console.AnswerNo, "enter": console.AnswerNo} {
		t.Run(how, func(t *testing.T) {
			m := newTestModel(t, newFakeSource())
			feed, _ := m.narrate()
			msg, keys, errs := asked(t, feed, console.Question{Title: "Sure?", DefaultNo: true})
			m.narrated(msg)
			key := keyPress(how)
			readFirst(m, key)
			m.Update(key)
			select {
			case key := <-keys:
				if err := <-errs; key != want || err != nil {
					t.Fatalf("%s = %q, %v; want %q", how, key, err, want)
				}
			case <-time.After(time.Second):
				t.Fatal("question never released the command")
			}
			feed.Close()
		})
	}
}

// Purging asks the core's question first (console.Dispatch → Confirm), and
// Enter answers it no.
func TestPurgeAsksTheCoresQuestion(t *testing.T) {
	ds := newFakeSource()
	m := newTestModel(t, ds)
	box := console.Sandbox{ID: "sbx_1", Name: "alpha", State: console.StateArchived}
	q, _ := console.Confirmation(console.VerbPurge, []console.Sandbox{box})
	m.Confirm(q, console.VerbPurge, []console.Sandbox{box})
	if m.dialog == nil || m.dialog.title != q.Title || m.dialog.body != q.Body || !m.dialog.defaultNo {
		t.Fatalf("dialog = %+v, want the core's purge question", m.dialog)
	}
	if cmd, _ := m.dialog.update(keyPress("enter")); cmd != nil {
		t.Fatal("Enter purged")
	}
	if cmd := m.dialog.action("yes"); cmd == nil {
		t.Fatal("yes did not purge")
	} else if msg, ok := cmd().(runVerbMsg); !ok || msg.verb != console.VerbPurge || len(msg.targets) != 1 {
		t.Fatalf("yes = %#v", cmd())
	}
}

func TestCommandPreviewPreservesExplicitAsk(t *testing.T) {
	options := newOptions(console.Session{GitCredentials: "ask"})
	if command := options.CommandFor(console.RunRequest{Source: "."}); !strings.Contains(command, "--git-credentials=ask") {
		t.Fatalf("preview drops ask override: %s", command)
	}
}

func TestPreviewReloadsSavedConsentWithoutResettingDraftOrOptions(t *testing.T) {
	ds := newFakeSource()
	ds.session.GitCredentials = "lend"
	m := newTestModel(t, ds)
	m.prompt.SetValue("keep this prompt")
	m.opts.cursor = console.OptSource
	ds.session.GitCredentials = "local"
	cmd := m.refreshGitCredentialPreview()
	if strings.Contains(m.opts.Command(""), "--git-credentials=lend") {
		t.Fatal("stale lending command remains copyable while loading")
	}
	send(t, m, cmd())
	if command := m.opts.Command(""); !strings.Contains(command, "--git-credentials=local") {
		t.Fatalf("stale preview: %s", command)
	}
	if m.prompt.Value() != "keep this prompt" || m.opts.cursor != console.OptSource {
		t.Fatal("refresh reset window state")
	}
}

// q closes a card nobody waits on without answering it: on the uncommitted
// changes card Esc is an answer — create without them — and q is not.
func TestQOnACardNobodyWaitsOnDoesNotAnswerIt(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	answered := false
	d := actionsDialog("Uncommitted changes", "", []action{{key: "true", label: "carry them", enabled: true}}, nil)
	d.onCancel = func() tea.Cmd { answered = true; return nil }
	m.dialog = d
	m.updateKey(keyPress("q"))
	if answered {
		t.Fatal("q answered a card nobody waits on")
	}
}

// A create's question is asked even once another operation has taken the busy
// line: the window has not moved on from a create that waits, and declining
// its question would answer it (leaving the uncommitted changes out) without
// anybody having seen it.
func TestACreatesQuestionIsAskedWhateverHasTheBusyLine(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	feed, _ := m.narrate()
	feed.create = &createWatch{}
	msg, keys, errs := asked(t, feed, gitQuestion)
	m.endNarration()
	m.narrated(msg)
	if m.dialog == nil || m.dialog.title != "Git credentials" {
		t.Fatalf("dialog = %s, want the create's question asked", describe(m.dialog))
	}
	m.dialog.action("push")
	if key, err := <-keys, <-errs; key != "push" || err != nil {
		t.Fatalf("answered %q, %v; want push", key, err)
	}
}

// Once a create is being sent, what its feed asks — Git's credential consent,
// delivering the source — is consent, not the create's question: from a feed
// the window has moved on from it is declined, never put up over whatever the
// person is typing into now.
func TestACreatesConsentOnceSentIsDeclinedWhenStale(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	feed, _ := m.narrate()
	feed.create = &createWatch{}
	m.createSent(feed.create)
	msg, keys, errs := asked(t, feed, gitQuestion)
	m.endNarration()
	m.narrated(msg)
	if m.dialog != nil && m.dialog.title == "Git credentials" {
		t.Fatal("a sent create's consent was asked from a stale feed")
	}
	if key, err := <-keys, <-errs; key != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("answered %q, %v; want it declined", key, err)
	}
}

// A key typed the moment a question comes up — typing carried on from what
// was in front a moment before — answers nothing, and nor does the run of
// typing it is part of; once the run pauses for console.AnswerQuiet the
// question has settled and the same key answers it (console.AnswerSettled).
func TestATypedAnswerWaitsForTheQuestionToSettle(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	clock := time.Unix(1_000_000, 0)
	m.now = func() time.Time { return clock }
	press := func(after time.Duration) {
		clock = clock.Add(after)
		m.Update(keyPress("1"))
	}
	// Typing in the prompt as the question comes up.
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	press(0)
	feed, _ := m.narrate()
	msg, keys, errs := asked(t, feed, gitQuestion)
	m.narrated(msg)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	for range 4 {
		press(console.AnswerQuiet / 2)
	}
	select {
	case key := <-keys:
		t.Fatalf("typing carried into the question answered it: %q", key)
	default:
	}
	if m.dialog == nil {
		t.Fatal("the question went")
	}
	press(console.AnswerQuiet)
	select {
	case key := <-keys:
		if err := <-errs; err != nil || key == "" {
			t.Fatalf("1 once it had settled = %q, %v", key, err)
		}
	case <-time.After(time.Second):
		t.Fatal("1 did not answer the settled question")
	}
	feed.Close()
}

// Typing begun just after a question comes up — by someone who has not looked
// up yet, the question having come a moment after their last key — holds it
// too; an arrow does not, so one before the answer never does; and an answer
// pressed too soon is held, which the card says, and taken once pressed again
// after a pause (console.AnswerSettled).
func TestTypingUnderAQuestionHoldsItAndArrowsDoNot(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	clock := time.Unix(1_000_000, 0)
	m.now = func() time.Time { return clock }
	key := func(after time.Duration, k string) {
		clock = clock.Add(after)
		m.Update(keyPress(k))
	}
	feed, _ := m.narrate()
	msg, keys, errs := asked(t, feed, gitQuestion)
	m.narrated(msg)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	q := console.AnswerQuiet
	// Typing that starts under the question, past the time it has been up.
	key(q/5, "x")
	for range 4 {
		key(q*3/5, "1")
	}
	select {
	case key := <-keys:
		t.Fatalf("typing begun under the question answered it: %q", key)
	default:
	}
	if !strings.Contains(m.View().Content, console.AnswerHeld[:20]) {
		t.Error("the card does not say a key was held")
	}
	// A pause, an arrow, then the answer.
	key(q, "down")
	key(q/10, "up")
	key(q/10, "1")
	select {
	case key := <-keys:
		if err := <-errs; err != nil || key == "" {
			t.Fatalf("1 after a pause and an arrow = %q, %v", key, err)
		}
	case <-time.After(time.Second):
		t.Fatal("1 after a pause and an arrow did not answer")
	}
	feed.Close()
}
