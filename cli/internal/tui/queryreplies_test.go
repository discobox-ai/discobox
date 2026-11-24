package tui

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// exitStep runs what one step of the way out returned and reports what it
// asked of the runtime: a DA1 written, the program ended, a timer set.
func exitStep(t *testing.T, cmd tea.Cmd) (asked, quit bool, timer *timerMsg) {
	t.Helper()
	if cmd == nil {
		return false, false, nil
	}
	var msgs []tea.Msg
	if msg := cmd(); msg != nil {
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, sub := range batch {
				if sub != nil {
					msgs = append(msgs, sub())
				}
			}
		} else {
			msgs = append(msgs, msg)
		}
	}
	for _, msg := range msgs {
		switch msg := msg.(type) {
		case tea.RawMsg:
			if msg.Msg == ansi.RequestPrimaryDeviceAttributes {
				asked = true
			}
		case timerMsg:
			timer = &msg
		case tea.QuitMsg:
			quit = true
		}
	}
	return asked, quit, timer
}

// The window ends only once its terminal has answered a DA1 asked after a
// DA1 asked as it closed — every answer it was owed is read by then — and
// nothing else that arrives meanwhile reaches the closed window.
func TestTheWindowEndsOnceItsTerminalHasAnswered(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	// The probe Init sent is still unanswered, as on a window refused at once.
	m.attributesOwed = 1

	asked, quit, timer := exitStep(t, m.closeWindow())
	if !m.quit || !asked || quit || timer == nil || timer.after != exitSettleLimit {
		t.Fatalf("closing: quit=%v asked=%v ended=%v timer=%v, want closed, a DA1 asked, a limit set and the program running", m.quit, asked, quit, timer)
	}
	da1 := uv.PrimaryDeviceAttributesEvent{61}
	// The probe's answer: one DA1 is still owed.
	if _, cmd := m.Update(da1); cmd != nil {
		t.Fatalf("the probe's answer returned %v, want nothing while a DA1 is owed", cmd)
	}
	// Anything else is not the closed window's.
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"}); cmd != nil {
		t.Fatalf("a key on the way out returned %v, want nothing", cmd)
	}
	_, cmd := m.Update(da1)
	if asked, quit, _ := exitStep(t, cmd); !asked || quit {
		t.Fatalf("first exit answer: asked=%v ended=%v, want a second DA1 and the program running", asked, quit)
	}
	_, cmd = m.Update(da1)
	if _, quit, _ := exitStep(t, cmd); !quit {
		t.Fatal("second exit answer did not end the program")
	}
}

// A terminal that answers no DA1 holds the window no longer than the limit.
func TestTheWindowEndsAtTheLimitWithoutAnAnswer(t *testing.T) {
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	_, _, timer := exitStep(t, m.closeWindow())
	if timer == nil {
		t.Fatal("closing set no limit")
	}
	_, cmd := m.Update(timer.fire(time.Time{}))
	if _, quit, _ := exitStep(t, cmd); !quit {
		t.Fatal("the limit passing did not end the program")
	}
}
