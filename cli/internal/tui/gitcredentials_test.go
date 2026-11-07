package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

func TestCredentialQuestionConsentCancelAndStaleFeed(t *testing.T) {
	for _, choice := range []string{"lend", "push", "cancel", "stale"} {
		t.Run(choice, func(t *testing.T) {
			m := newTestModel(t, newFakeSource())
			feed, _ := m.narrate()
			answer := make(chan bool, 1)
			go func() {
				yes, _ := feed.credentials().Confirm(context.Background(), "forge.example/repo")
				answer <- yes
			}()
			line := <-feed.ch
			msg := narrationMsg{source: feed, question: line.question}
			if choice == "stale" {
				m.endNarration()
			}
			m.narrated(msg)
			if choice != "stale" {
				if m.dialog == nil || !strings.Contains(m.dialog.body, "needs credentials to pull on the pool.") {
					t.Fatal("consent not shown")
				}
				if choice == "cancel" {
					m.dialog.cancel()
				} else {
					m.dialog.action(choice)
				}
			}
			select {
			case yes := <-answer:
				if yes != (choice == "lend") {
					t.Fatalf("consent=%v", yes)
				}
			case <-time.After(time.Second):
				t.Fatal("question never released delivery")
			}
			feed.close()
		})
	}
}

// The question comes up over the wait on the create it is about, and however
// it is answered — a key or a click — the wait comes back. Taking the question
// down onto nothing shows the list, which a window that is one command's own
// never shows.
func TestCredentialQuestionGoesBackToTheWait(t *testing.T) {
	for _, how := range []string{"esc", "enter", "click"} {
		t.Run(how, func(t *testing.T) {
			m := newTestModel(t, newFakeSource())
			wait := m.waitDialog(creatingTitle, "delivering the source")
			m.dialog = wait
			feed, _ := m.narrate()
			answer := make(chan bool, 1)
			go func() {
				yes, _ := feed.credentials().Confirm(context.Background(), "forge.example/repo")
				answer <- yes
			}()
			line := <-feed.ch
			m.narrated(narrationMsg{source: feed, question: line.question})
			if m.dialog == wait {
				t.Fatal("consent not shown")
			}
			switch how {
			case "click":
				m.pressDialogItem(0)
			default:
				m.updateKey(keyPress(how))
			}
			select {
			case <-answer:
			case <-time.After(time.Second):
				t.Fatal("question never released delivery")
			}
			if m.dialog != wait {
				t.Fatalf("dialog = %+v, want the wait back", m.dialog)
			}
			feed.close()
		})
	}
}

func TestCredentialExplanationFollowsSelectionAndHoverBeforeConfirm(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	feed, _ := m.narrate()
	question := &credentialQuestion{remote: "forge.example/repo", answer: make(chan credentialAnswer, 1)}
	m.credentialQuestion(narrationMsg{source: feed, question: question})
	m.screenWidth, m.height = 70, 18
	view := m.View().Content
	if !strings.Contains(view, "forge.example/repo") || !strings.Contains(view, "Git's credential helper") {
		t.Fatalf("initial explanation missing: %s", view)
	}
	m.dialog.update(keyPress("down"))
	view = m.View().Content
	if !strings.Contains(view, "This machine authenticates") {
		t.Fatalf("local explanation missing: %s", view)
	}
	m.dialog.update(keyPress("up"))
	m.View()
	before := m.dialog.offset
	m.dialog.update(keyPress("pgdown"))
	if m.dialog.offset <= before {
		t.Fatal("explanation cannot scroll on a small terminal")
	}
	m.View()
	for _, z := range m.zones.marks {
		if z.what.kind == hitDialogItem && z.what.idx == 1 {
			m.hover(tea.MouseMotionMsg{X: z.x, Y: z.y, Button: tea.MouseNone})
			break
		}
	}
	if m.dialog.cursor != 1 {
		t.Fatal("hover did not preview local choice")
	}
	select {
	case <-question.answer:
		t.Fatal("preview confirmed a choice")
	default:
	}
	m.dialog.cancel()
	if answer := <-question.answer; !errors.Is(answer.err, context.Canceled) {
		t.Fatalf("cancel = %+v", answer)
	}
	feed.close()
}

func TestCommandPreviewPreservesExplicitAsk(t *testing.T) {
	options := newOptions(console.Session{GitCredentials: "ask"})
	if command := options.commandFor(console.RunRequest{Source: "."}); !strings.Contains(command, "--git-credentials=ask") {
		t.Fatalf("preview drops ask override: %s", command)
	}
}

func TestPreviewReloadsSavedConsentWithoutResettingDraftOrOptions(t *testing.T) {
	ds := newFakeSource()
	ds.session.GitCredentials = "lend"
	m := newTestModel(t, ds)
	m.prompt.SetValue("keep this prompt")
	m.opts.cursor = optSource
	ds.session.GitCredentials = "local"
	cmd := m.refreshGitCredentialPreview()
	if strings.Contains(m.opts.command(""), "--git-credentials=lend") {
		t.Fatal("stale lending command remains copyable while loading")
	}
	send(t, m, cmd())
	if command := m.opts.command(""); !strings.Contains(command, "--git-credentials=local") {
		t.Fatalf("stale preview: %s", command)
	}
	if m.prompt.Value() != "keep this prompt" || m.opts.cursor != optSource {
		t.Fatal("refresh reset window state")
	}
}
