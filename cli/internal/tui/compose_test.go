package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestPromptComposerMultiline(t *testing.T) {
	m := newPromptComposer(t.Context())
	m.Init()
	m.Update(tea.PasteMsg{Content: "Keep 'quotes' and $vars"})
	_, cmd := m.Update(keyPress("enter"))
	if cmd != nil {
		t.Fatal("Enter must add a newline without submitting")
	}
	m.Update(tea.PasteMsg{Content: "second line\n\nlast line"})
	want := "Keep 'quotes' and $vars\nsecond line\n\nlast line"
	if m.text.Value() != want {
		t.Fatalf("buffer = %q, want %q", m.text.Value(), want)
	}
	_, cmd = m.Update(keyPress("ctrl+d"))
	if cmd == nil || m.submitted != want {
		t.Fatalf("Ctrl-D: submitted=%q, quit=%v", m.submitted, cmd != nil)
	}
}

func TestPromptComposerLongPasteAndResize(t *testing.T) {
	m := newPromptComposer(t.Context())
	m.Init()
	want := strings.Repeat("a long prompt line\n", 200)
	m.Update(tea.PasteMsg{Content: want})
	m.Update(tea.WindowSizeMsg{Width: 30, Height: 10})
	m.Update(keyPress("ctrl+d"))
	if m.submitted != want {
		t.Fatalf("long paste lost content: got %d bytes, want %d", len(m.submitted), len(want))
	}
}

func TestPromptComposerExternalEdit(t *testing.T) {
	m := newPromptComposer(t.Context())
	path := filepath.Join(t.TempDir(), "prompt.md")
	const want = "Edited 'quotes'\nand $variables\n"
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	m.Update(editorDoneMsg{path: path})
	m.Update(keyPress("ctrl+d"))
	if m.submitted != want {
		t.Fatalf("external edit = %q, want %q", m.submitted, want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("editor temp file still exists: %v", err)
	}
}

func TestPromptComposerCancellation(t *testing.T) {
	for _, k := range []string{"esc", "ctrl+c"} {
		t.Run(k, func(t *testing.T) {
			m := newPromptComposer(t.Context())
			if k == "ctrl+c" {
				m.text.SetValue("do not run this")
			}
			_, cmd := m.Update(keyPress(k))
			if cmd == nil || m.submitted != "" {
				t.Fatalf("cancellation submitted %q", m.submitted)
			}
		})
	}
}

func TestPromptComposerConfirmsDiscard(t *testing.T) {
	for _, keep := range []string{"esc", "n", "ctrl+c"} {
		t.Run("keep with "+keep, func(t *testing.T) {
			m := newPromptComposer(t.Context())
			m.text.SetValue("keep this prompt\nwith two lines")
			_, cmd := m.Update(keyPress("esc"))
			if cmd != nil || !m.discard {
				t.Fatal("Esc must ask before discarding")
			}
			m.Update(tea.PasteMsg{Content: "do not insert this"})
			m.Update(keyPress("enter"))
			if !m.discard || m.text.Value() != "keep this prompt\nwith two lines" {
				t.Fatal("confirmation must hold keys and pastes")
			}
			_, cmd = m.Update(keyPress(keep))
			if cmd != nil || m.discard {
				t.Fatal("declining discard must return to editing")
			}
			m.Update(keyPress("ctrl+d"))
			if m.submitted != "keep this prompt\nwith two lines" {
				t.Fatalf("declining discard lost prompt: %q", m.submitted)
			}
		})
	}
	m := newPromptComposer(t.Context())
	m.text.SetValue("discard this")
	m.Update(keyPress("esc"))
	_, cmd := m.Update(keyPress("y"))
	if cmd == nil || m.submitted != "" {
		t.Fatal("confirming discard must exit without submitting")
	}
}
