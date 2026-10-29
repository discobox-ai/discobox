package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestPromptComposerMultiline(t *testing.T) {
	m := newPromptComposer(t.Context())
	m.Init()
	m.Update(tea.PasteMsg{Content: "Keep 'quotes' and $vars"})
	_, cmd := m.Update(keyPress("shift+enter"))
	if cmd != nil {
		t.Fatal("Shift-Enter must add a newline without submitting")
	}
	m.Update(tea.PasteMsg{Content: "second line\n\nlast line"})
	want := "Keep 'quotes' and $vars\nsecond line\n\nlast line"
	if m.text.Value() != want {
		t.Fatalf("buffer = %q, want %q", m.text.Value(), want)
	}
	_, cmd = m.Update(keyPress("enter"))
	if cmd == nil || m.submitted != want {
		t.Fatalf("Enter: submitted=%q, quit=%v", m.submitted, cmd != nil)
	}
}

func TestPromptComposerSubmitKeys(t *testing.T) {
	for _, k := range []string{"enter", "ctrl+d"} {
		t.Run(k, func(t *testing.T) {
			m := newPromptComposer(t.Context())
			m.text.SetValue("run this")
			_, cmd := m.Update(keyPress(k))
			if cmd == nil || m.submitted != "run this" {
				t.Fatalf("%s: submitted=%q, quit=%v", k, m.submitted, cmd != nil)
			}
		})
	}
	for _, k := range []string{"shift+enter", "ctrl+enter", "alt+enter", "ctrl+j"} {
		t.Run(k, func(t *testing.T) {
			m := newPromptComposer(t.Context())
			m.text.SetValue("first")
			m.text.CursorEnd()
			_, cmd := m.Update(keyPress(k))
			if cmd != nil || m.submitted != "" || m.text.Value() != "first\n" {
				t.Fatalf("%s: value=%q, submitted=%q", k, m.text.Value(), m.submitted)
			}
		})
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

func TestPromptComposerGrowsInline(t *testing.T) {
	m := newPromptComposer(t.Context())
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if m.text.Height() != 2 || m.View().AltScreen {
		t.Fatal("composer must start inline with two rows")
	}
	m.Update(tea.PasteMsg{Content: "first\nsecond\nthird\nfourth"})
	if m.text.Height() != 4 {
		t.Fatalf("height = %d, want 4", m.text.Height())
	}
	m.Update(keyPress("ctrl+home"))
	m.Update(keyPress("ctrl+k"))
	// A long paste must scroll without truncation even after the viewport caps.
	want := strings.Repeat("line\n", 200)
	m.text.Reset()
	m.Update(tea.PasteMsg{Content: want})
	if m.text.Height() != 20 || m.text.Value() != want {
		t.Fatalf("capped height = %d, content bytes = %d", m.text.Height(), len(m.text.Value()))
	}
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	if m.text.Height() != 7 || m.text.Value() != want {
		t.Fatal("resizing must cap the viewport without losing text")
	}
	m.text.Reset()
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if m.text.Height() != 2 {
		t.Fatalf("empty composer did not shrink: height = %d", m.text.Height())
	}
	m.Update(tea.PasteMsg{Content: strings.Repeat("word ", 50)})
	if m.text.Height() <= 2 {
		t.Fatal("soft wrapping must grow the field")
	}
	m.Update(keyPress("ctrl+d"))
	if m.View().Content != "" {
		t.Fatal("leaving the composer must clear its rows")
	}
}

func TestPromptComposerNarrowFooter(t *testing.T) {
	m := newPromptComposer(t.Context())
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: 20, Height: 12})
	m.Update(tea.PasteMsg{Content: strings.Repeat("line\n", 100)})
	for _, confirm := range []bool{false, true} {
		if confirm {
			m.Update(keyPress("esc"))
		}
		view := m.View().Content
		if rows := strings.Count(view, "\n") + 1; rows > 12 {
			t.Fatalf("composer uses %d rows in a 12-row terminal", rows)
		}
		for _, line := range strings.Split(view, "\n") {
			if width := ansi.StringWidth(line); width > 20 {
				t.Fatalf("line is %d cells wide: %q", width, line)
			}
		}
		footer := strings.ReplaceAll(m.footer(), "\n", "")
		want := "Enter submit"
		if confirm {
			want = "n/Esc keep editing"
		}
		if !strings.Contains(footer, want) {
			t.Fatalf("footer lost %q: %q", want, footer)
		}
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
