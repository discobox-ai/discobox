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
