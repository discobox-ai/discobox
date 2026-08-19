package termpane

import (
	"bytes"
	"encoding/base64"

	tea "charm.land/bubbletea/v2"
)

// oscClipboard is OSC 52, the sequence an application inside the pane writes to
// put text on the clipboard of the terminal it is running in.
const oscClipboard = 52

// clipboardTargets are the selections an OSC 52 copy may name that mean "the
// clipboard" to a host that has one: c (clipboard), p (primary), s (whichever
// the terminal is configured to use). An empty field means the terminal's
// default, which is one of those. The cut buffers (0-7) are xterm's own
// registers and are not a clipboard, so a copy naming only those is dropped.
const clipboardTargets = "cps"

// watchClipboard forwards the pane's OSC 52 copies to the host.
//
// Without this an application that copies — vim's clipboard=unnamed over
// osc52, tmux's set-clipboard, anything that has learned it is on a remote
// terminal — writes the sequence into a pane that parses it and throws it
// away, and the copy silently does nothing. The emulator has no handler of its
// own for OSC 52, so the pane is the only place this can be caught.
//
// A copy is held rather than emitted here: handlers run on whichever goroutine
// is feeding the emulator, and a Bubble Tea message has to come back out of
// Update as a command. See takeClipboard.
//
// A *read* — "52;c;?", the application asking what is on the clipboard — is
// answered with silence. Handing the clipboard to whatever is running in the
// pane means any command that scrolls text past the terminal can exfiltrate
// passwords the user copied for something else; xterm ships that disabled for
// the same reason, and the pane has no way to ask the host's permission.
func (m *Model) watchClipboard() {
	m.emu.RegisterOscHandler(oscClipboard, func(data []byte) bool {
		if text, ok := parseClipboardCopy(data); ok {
			m.mu.Lock()
			m.clip, m.hasClip = text, true
			m.mu.Unlock()
		}
		// Handled either way: the clipboard sequence is the pane's, and one it
		// declines is one it has decided about rather than one nothing knows.
		return true
	})
}

// parseClipboardCopy reads "52;<targets>;<base64>" and returns the text copied.
//
// Base64 is the whole payload's encoding, so the fields split cleanly on the
// semicolon: nothing in the alphabet is one. Anything that does not decode is
// dropped — xterm reads that as a request to clear the selection, and a pane
// has no selection of the host's to clear.
//
// An empty payload is a clear, and is dropped with it. An application emptying
// its own clipboard is not a reason to empty the user's, which holds whatever
// they last copied for something else entirely.
func parseClipboardCopy(data []byte) (string, bool) {
	parts := bytes.SplitN(data, []byte{';'}, 3)
	if len(parts) != 3 {
		return "", false
	}
	targets, payload := parts[1], parts[2]
	if len(targets) > 0 && !bytes.ContainsAny(targets, clipboardTargets) {
		return "", false
	}
	if len(payload) == 0 || bytes.Equal(payload, []byte("?")) {
		return "", false
	}
	text, err := base64.StdEncoding.DecodeString(string(payload))
	if err != nil {
		return "", false
	}
	return string(text), true
}

// takeClipboard hands off a copy the application made, as the same [CopyMsg] a
// finished selection produces: what the host does with text the pane copied
// does not depend on which of them started it.
//
// The last copy in a burst is the one delivered. Applying every copy in turn
// leaves the clipboard holding the last one anyway, and an application that
// copies twice before the host has drawn a frame has changed its mind, not
// asked for two clipboards.
func (m *Model) takeClipboard() tea.Cmd {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.hasClip {
		return nil
	}
	text := m.clip
	m.clip, m.hasClip = "", false
	return func() tea.Msg { return CopyMsg{Text: text} }
}
