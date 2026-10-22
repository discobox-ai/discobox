package ahphost

import (
	"strings"
	"unicode/utf8"

	"github.com/microsoft/agent-host-protocol/clients/go/ahp"
	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
)

// URI schemes. A discobox listed from a server is `ahp-session:/<id>`; one a
// client created keeps the URI the client chose. Its chats and screens hang
// off the session's key, the path after the scheme.
const (
	sessionScheme  = "ahp-session:/"
	chatScheme     = "ahp-chat:/"
	terminalScheme = "ahp-terminal:/"
)

// primaryChatKey names the chat of a discobox's primary terminal: the one the
// session's defaultChat points at, known before its exec is.
const primaryChatKey = "primary"

func sessionKey(uri string) string { return strings.TrimPrefix(uri, sessionScheme) }

// terminalContentLimit is how much of a terminal's output the host keeps for
// a snapshot. The spec lets either side trim; a client newly subscribed is
// sent a repaint as well, so what is dropped is only the scrollback above it.
const terminalContentLimit = 256 << 10

// applyLocked runs action through the protocol's reducer for the channel's
// kind, and says who is subscribed to it. h.mu must be held.
func (h *Host) applyLocked(channel string, action ahptypes.StateAction) map[*conn]struct{} {
	if channel == ahptypes.RootResourceURI {
		ahp.ApplyActionToRoot(&h.root, action)
		return h.rootSubs
	}
	if s := h.sessions[channel]; s != nil {
		ahp.ApplyActionToSession(&s.state, action)
		return s.subs
	}
	if c := h.chats[channel]; c != nil {
		ahp.ApplyActionToChat(&c.state, action)
		return c.subs
	}
	if t := h.terminals[channel]; t != nil {
		ahp.ApplyActionToTerminal(&t.state, action)
		trimTerminal(&t.state)
		return t.subs
	}
	return nil
}

// trimTerminal drops the oldest output past terminalContentLimit, whole parts
// first, then the front of the oldest part that is left, cut where a line
// ends so the rest does not begin inside an escape sequence.
func trimTerminal(state *ahptypes.TerminalState) {
	total := 0
	for _, part := range state.Content {
		total += len(partText(part))
	}
	for total > terminalContentLimit && len(state.Content) > 1 {
		total -= len(partText(state.Content[0]))
		state.Content = state.Content[1:]
	}
	if total <= terminalContentLimit || len(state.Content) == 0 {
		return
	}
	p, ok := state.Content[0].Value.(*ahptypes.TerminalUnclassifiedPart)
	if !ok {
		return
	}
	cut := len(p.Value) - terminalContentLimit
	if i := strings.IndexByte(p.Value[cut:], '\n'); i >= 0 {
		cut += i + 1
	}
	p.Value = p.Value[cut:]
}

func partText(part ahptypes.TerminalContentPart) string {
	switch p := part.Value.(type) {
	case *ahptypes.TerminalUnclassifiedPart:
		return p.Value
	case *ahptypes.TerminalCommandPart:
		return p.Output
	}
	return ""
}

// utf8Prefix splits b into the longest prefix that does not end inside a
// UTF-8 sequence and the incomplete tail, which waits for the next read. A
// terminal's output is bytes and an action carries a string; a character
// split across two reads must not become two replacement characters.
func utf8Prefix(b []byte) (string, []byte) {
	for i := 1; i <= utf8.UTFMax && i <= len(b); i++ {
		start := len(b) - i
		if !utf8.RuneStart(b[start]) {
			continue
		}
		if !utf8.FullRune(b[start:]) {
			return string(b[:start]), append([]byte(nil), b[start:]...)
		}
		break
	}
	return string(b), nil
}

func ptr[T any](v T) *T { return &v }
