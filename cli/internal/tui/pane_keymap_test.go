package tui

import (
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/discobox-ai/discobox/termpane"
)

// keymapStream is a pane's far end, and records what was typed at it.
type keymapStream struct {
	written chan []byte
	closed  chan struct{}
}

func (s *keymapStream) Read(_ []byte) (int, error) {
	<-s.closed
	return 0, io.EOF
}

func (s *keymapStream) Write(p []byte) (int, error) {
	s.written <- append([]byte(nil), p...)
	return len(p), nil
}

func (s *keymapStream) Close() error          { close(s.closed); return nil }
func (s *keymapStream) Resize(_, _ int) error { return nil }
func (s *keymapStream) Repaint() error        { return nil }

// The panes this window opens carry its keymap, and carry it through
// paneOptions rather than only declaring it.
//
// Without it a modified Enter is a plain return: correct for a terminal with no
// keyboard protocol negotiated, and a submission in every prompt a discobox
// runs. The chord is bound here because nothing on either side of the pane can
// negotiate one; see paneKeymap.
func TestPanesCarryTheWindowsKeymap(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyPressMsg
		want string
	}{
		{"shift+enter", tea.KeyPressMsg{Code: uv.KeyEnter, Mod: uv.ModShift}, "\x1b\r"},
		{"ctrl+enter", tea.KeyPressMsg{Code: uv.KeyEnter, Mod: uv.ModCtrl}, "\n"},
		// An unbound key is still the encoder's.
		{"enter", tea.KeyPressMsg{Code: uv.KeyEnter}, "\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(t, newFakeSource(testSandboxes()...))
			pane := termpane.New(m.paneOptions(paneWorkspace, false)...)
			pane.SetSize(40, 5)
			stream := &keymapStream{written: make(chan []byte, 16), closed: make(chan struct{})}
			pane.Attach(stream)
			t.Cleanup(func() { _ = pane.Close() })

			pane.SendKey(tc.key)
			var got string
			deadline := time.After(2 * time.Second)
			for !strings.Contains(got, tc.want) {
				select {
				case b := <-stream.written:
					got += string(b)
				case <-deadline:
					t.Fatalf("%s sent %q, want %q", tc.name, got, tc.want)
				}
			}
			if got != tc.want {
				t.Fatalf("%s sent %q, want exactly %q", tc.name, got, tc.want)
			}
		})
	}
}
