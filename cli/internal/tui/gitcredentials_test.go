package tui

import (
	"context"
	"strings"
	"testing"
	"time"
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
				if m.dialog == nil || !strings.Contains(m.dialog.body, "Use your Git credentials?") {
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
