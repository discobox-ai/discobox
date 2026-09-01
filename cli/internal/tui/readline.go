package tui

import (
	"slices"
	"strings"
	"unicode"

	// Aliased: the package's own tests declare a key() helper, and a file-scope
	// import named key would collide with it.
	bind "charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
)

// The composer is a readline.
//
// Anyone who has typed at a shell prompt has emacs mode in their fingers, and a
// field that answers Ctrl-A and Ctrl-K but not Ctrl-Y is worse than one that
// answers neither: it invites the muscle memory and then drops half of it. So
// the composer answers all of it.
//
// The textarea brings most of it already — the character and word motions, the
// line ends, the kills, transpose, the case keys, the buffer ends. What it does
// not bring, this file does:
//
//   - Ctrl-← and Ctrl-→ as word motion. Readline binds no such key itself, but
//     it is what every terminal sends for word motion and what the distributed
//     inputrc files bind, so it is where the expectation comes from. The
//     textarea is also the only one of the two fields in this window without
//     it: bubbles' single-line input has had it all along, which is why it
//     works in a dialog and not in the composer.
//   - The kill ring. The textarea's kills throw the text away. Readline's put
//     it where Ctrl-Y brings it back from, and a run of kills accumulates into
//     one entry rather than each one discarding what the last took.
//   - Undo, on Ctrl-_.
//   - Alt-T, transpose words.
//
// Deliberately not here: yank-pop (Alt-Y), which needs a ring deeper than one
// entry to mean anything; the history keys, since the composer has no history
// to walk — the draft is the one thing it remembers, and it comes back on its
// own; and vi mode.
const (
	yankKey = "ctrl+y"
	undoKey = "ctrl+_"
	// Ctrl-_ is Ctrl-Shift-minus on most layouts, and a terminal speaking the
	// Kitty protocol reports the Shift that was held rather than folding it
	// into the key the way a byte-oriented one has to.
	undoShiftKey      = "ctrl+shift+_"
	transposeWordsKey = "alt+t"
)

// promptKeyMap is the textarea's key map with the word keys a terminal actually
// sends added to the emacs ones it already had. Nothing is taken away: Alt-F
// and Alt-B still work for the people whose Option key is Meta.
func promptKeyMap() textarea.KeyMap {
	km := textarea.DefaultKeyMap()
	km.WordForward = bind.NewBinding(
		bind.WithKeys("alt+right", "ctrl+right", "alt+f"),
		bind.WithHelp("ctrl+→", "word forward"),
	)
	km.WordBackward = bind.NewBinding(
		bind.WithKeys("alt+left", "ctrl+left", "alt+b"),
		bind.WithHelp("ctrl+←", "word backward"),
	)
	km.DeleteWordForward = bind.NewBinding(
		bind.WithKeys("alt+delete", "ctrl+delete", "alt+d"),
		bind.WithHelp("alt+d", "delete word forward"),
	)
	km.DeleteWordBackward = bind.NewBinding(
		bind.WithKeys("alt+backspace", "ctrl+backspace", "ctrl+w"),
		bind.WithHelp("ctrl+w", "delete word backward"),
	)
	return km
}

// killDir is which side of the cursor a kill reached, which is the side its
// text joins on when a run of kills accumulates.
type killDir int

const (
	killForward killDir = iota
	killBackward
)

// killKeys are the keys whose deletion is a kill — text the ring keeps — rather
// than a plain erase. Backspace and Delete are not among them, in readline and
// here: a character taken back one at a time is a typo, not something you meant
// to move.
var killKeys = map[string]killDir{
	"ctrl+k":         killForward,
	"alt+d":          killForward,
	"alt+delete":     killForward,
	"ctrl+delete":    killForward,
	"ctrl+u":         killBackward,
	"ctrl+w":         killBackward,
	"alt+backspace":  killBackward,
	"ctrl+backspace": killBackward,
}

// promptUndoDepth is how far back Ctrl-_ reaches. A run of typing is one entry,
// so this is deep enough to cover any prompt somebody writes in this field and
// shallow enough that the window is not holding a novel in memory.
const promptUndoDepth = 200

// promptState is the composer as it stood: what was in it, and where the cursor
// was. The cursor is part of the state because readline's undo puts you back
// where the change was made, not where you have since wandered to.
type promptState struct {
	value string
	line  int
	col   int
}

// promptEditor is the readline state the textarea does not keep for itself.
type promptEditor struct {
	// kill is the kill ring: one entry, which is what Ctrl-Y yanks.
	kill string
	// killing is whether the key just handled was a kill, so that the next one
	// accumulates onto the entry rather than replacing it. Any other key ends
	// the run.
	killing bool
	// undo is what Ctrl-_ walks back through, oldest first.
	undo []promptState
	// typing is whether the entry on top was pushed by a self-inserting key, so
	// that a typed word comes back in one press rather than one per letter.
	typing bool
}

// reset throws the history away. The buffer it described is gone — run, or
// replaced by the draft the window opened with — and an undo that reached
// behind that would be putting back a prompt that has already been sent.
func (e *promptEditor) reset() { *e = promptEditor{kill: e.kill} }

func (m *Model) promptState() promptState {
	return promptState{value: m.prompt.Value(), line: m.prompt.Line(), col: m.prompt.Column()}
}

// setPromptState puts the composer back to a state, cursor included. CursorDown
// moves by displayed row, so reaching a line means walking until the line
// number changes; see onFirstPromptRow for why the two are not the same.
func (m *Model) setPromptState(s promptState) {
	m.prompt.SetValue(s.value)
	m.prompt.MoveToBegin()
	for m.prompt.Line() < s.line && m.prompt.Line() < m.prompt.LineCount()-1 {
		m.prompt.CursorDown()
	}
	m.prompt.SetCursorColumn(s.col)
}

// promptKey is the composer's last stop, past the keys updatePrompt spends on
// leaving the field and launching. The key is one of readline's own or it is
// the textarea's, and either way what it did to the buffer is recorded, so that
// Ctrl-Y and Ctrl-_ have something to give back.
func (m *Model) promptKey(msg tea.KeyPressMsg) tea.Cmd {
	before := m.promptState()

	switch keyName(msg) {
	case yankKey:
		m.edits.killing = false
		if m.edits.kill == "" {
			return status("nothing to yank")
		}
		m.pushUndo(before, false)
		m.prompt.InsertString(m.edits.kill)
		return nil

	case undoKey, undoShiftKey:
		m.edits.killing, m.edits.typing = false, false
		if len(m.edits.undo) == 0 {
			return status("nothing to undo")
		}
		m.setPromptState(m.edits.undo[len(m.edits.undo)-1])
		m.edits.undo = m.edits.undo[:len(m.edits.undo)-1]
		return nil

	case transposeWordsKey:
		m.edits.killing = false
		after, ok := transposeWords(before)
		if !ok {
			return nil
		}
		m.pushUndo(before, false)
		m.setPromptState(after)
		return nil
	}

	var cmd tea.Cmd
	m.prompt, cmd = m.prompt.Update(msg)

	after := m.prompt.Value()
	if after == before.value {
		// A motion is not an edit, and it does not end a run of kills either:
		// readline lets you move to the next word and go on killing onto the
		// same entry.
		if _, isKill := killKeys[keyName(msg)]; !isKill {
			m.edits.killing = false
		}
		return cmd
	}
	m.pushUndo(before, inserting(msg))
	if dir, isKill := killKeys[keyName(msg)]; isKill {
		m.remember(before.value, after, dir)
	} else {
		m.edits.killing = false
	}
	return cmd
}

// inserting is whether the key typed a character. Those are coalesced into one
// undo entry, because undoing a word a letter at a time is not an undo anybody
// wants.
func inserting(msg tea.KeyPressMsg) bool {
	return msg.Text != "" && msg.Mod&^tea.ModShift == 0
}

func (m *Model) pushUndo(before promptState, typing bool) {
	if typing && m.edits.typing && len(m.edits.undo) > 0 {
		return
	}
	m.edits.undo = append(m.edits.undo, before)
	if len(m.edits.undo) > promptUndoDepth {
		m.edits.undo = slices.Delete(m.edits.undo, 0, len(m.edits.undo)-promptUndoDepth)
	}
	m.edits.typing = typing
}

// remember puts what a kill key took onto the ring. A run of kills accumulates
// into one entry — that is how readline lets you take a line apart a word at a
// time and put the whole of it back with one Ctrl-Y — each kill joining on the
// side it reached from, so the text comes back in the order it was in.
func (m *Model) remember(before, after string, dir killDir) {
	text := removed(before, after)
	if text == "" {
		m.edits.killing = false
		return
	}
	switch {
	case !m.edits.killing:
		m.edits.kill = text
	case dir == killBackward:
		m.edits.kill = text + m.edits.kill
	default:
		m.edits.kill += text
	}
	m.edits.killing = true
}

// removed is the run of text that turning before into after took out. Every key
// that reaches here deletes one span and changes nothing else, so the span is
// whatever lies between the ends the two still share.
func removed(before, after string) string {
	b, a := []rune(before), []rune(after)
	if len(a) >= len(b) {
		return ""
	}
	head := 0
	for head < len(a) && b[head] == a[head] {
		head++
	}
	tail := 0
	for tail < len(a)-head && b[len(b)-1-tail] == a[len(a)-1-tail] {
		tail++
	}
	return string(b[head : len(b)-tail])
}

// transposeWords drags the word before the cursor past the word after it and
// leaves the cursor beyond the pair, which is what Alt-T does at a shell. At the
// end of a line it takes the last two words, since there is no word ahead to be
// dragged past. It reports whether there were two words to swap.
//
// Words are runs of non-space, the same definition the textarea's own word keys
// use, so Alt-T moves exactly what Alt-F would have stepped over.
func transposeWords(s promptState) (promptState, bool) {
	lines := strings.Split(s.value, "\n")
	if s.line >= len(lines) {
		return s, false
	}
	line := []rune(lines[s.line])
	space := func(i int) bool { return unicode.IsSpace(line[i]) }

	// Forward to the end of the word the cursor is in or before.
	i := min(s.col, len(line))
	for i < len(line) && space(i) {
		i++
	}
	for i < len(line) && !space(i) {
		i++
	}
	secondEnd := i
	for i > 0 && !space(i-1) {
		i--
	}
	secondStart := i
	// Back over the gap, then over the word before it.
	for i > 0 && space(i-1) {
		i--
	}
	firstEnd := i
	for i > 0 && !space(i-1) {
		i--
	}
	firstStart := i

	if firstStart == firstEnd || secondStart == secondEnd || firstEnd > secondStart {
		return s, false
	}
	swapped := slices.Concat(
		line[:firstStart],
		line[secondStart:secondEnd],
		line[firstEnd:secondStart],
		line[firstStart:firstEnd],
		line[secondEnd:],
	)
	lines[s.line] = string(swapped)
	// The two words traded places around a gap that did not move, so nothing
	// past them shifted and the end of the pair is still where it was.
	return promptState{value: strings.Join(lines, "\n"), line: s.line, col: secondEnd}, true
}
