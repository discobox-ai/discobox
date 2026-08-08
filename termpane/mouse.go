package termpane

import (
	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// MouseMode is how much of the mouse the application in the pane has asked for.
//
// It matters because a host cannot simply turn mouse reporting on and leave it
// on: while the real terminal is reporting the mouse, the user loses their own
// click-drag-to-select. Mirroring the application means they only lose it while
// something is actually using it — which is the same bargain tmux strikes, and
// the same escape applies, since most terminals bypass mouse reporting while
// Shift is held.
type MouseMode int

const (
	// MouseNone: the application has not asked for the mouse.
	MouseNone MouseMode = iota
	// MouseCellMotion: clicks, releases, and drags.
	MouseCellMotion
	// MouseAllMotion: the above plus motion with no button down.
	MouseAllMotion
)

// The DEC private modes an application sets to ask for the mouse. The encoding
// modes (1005/1006/1015) are not here: the emulator picks the encoding itself
// when it sends an event, so all a host needs to know is whether to report at
// all, and how much.
const (
	modeMouseX10         = 9
	modeMouseNormal      = 1000
	modeMouseHighlight   = 1001
	modeMouseButtonEvent = 1002
	modeMouseAnyEvent    = 1003
)

// MouseMode reports what the application has asked for. Mirror it into whatever
// your framework uses to turn mouse reporting on — in Bubble Tea that is
// [tea.View.MouseMode] — so the terminal only reports while something wants it.
func (m *Model) MouseMode() MouseMode {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.mouseModes[modeMouseAnyEvent]:
		return MouseAllMotion
	case m.mouseModes[modeMouseButtonEvent], m.mouseModes[modeMouseHighlight],
		m.mouseModes[modeMouseNormal], m.mouseModes[modeMouseX10]:
		return MouseCellMotion
	default:
		return MouseNone
	}
}

// watchMouseModes records the mouse modes the application sets and resets.
//
// The emulator tracks them internally but exposes no way to ask, so they are
// read off the stream on the way past: handlers are dispatched last-registered
// first and fall through when one returns false, so this observes the sequence
// without consuming it and the emulator still applies it.
//
// Reading the stream also means it does not matter whether a mode arrived from
// the application just now or from a reattach snapshot replaying what it set
// before this client existed. Both are the same bytes.
func (m *Model) watchMouseModes() {
	record := func(set bool) func(ansi.Params) bool {
		return func(params ansi.Params) bool {
			m.mu.Lock()
			defer m.mu.Unlock()
			params.ForEach(-1, func(_, param int, _ bool) {
				switch param {
				case modeMouseX10, modeMouseNormal, modeMouseHighlight,
					modeMouseButtonEvent, modeMouseAnyEvent:
					if m.mouseModes == nil {
						m.mouseModes = map[int]bool{}
					}
					m.mouseModes[param] = set
				}
			})
			return false // not handled here; let the emulator apply it
		}
	}
	m.emu.RegisterCsiHandler(ansi.Command('?', 0, 'h'), record(true))
	m.emu.RegisterCsiHandler(ansi.Command('?', 0, 'l'), record(false))
}

// SendMouse forwards a mouse event to the application, in cells relative to the
// pane's own grid: subtract wherever you drew it, the same origin you pass to
// [Model.Cursor].
//
// An event the application has not asked for is dropped by the emulator, so a
// host may forward unconditionally; and one outside the grid is dropped here,
// since a click on your own chrome is not the application's.
func (m *Model) SendMouse(msg tea.MouseMsg) {
	if m.emu == nil {
		return
	}
	mouse := msg.Mouse()
	if mouse.X < 0 || mouse.Y < 0 || mouse.X >= m.cols || mouse.Y >= m.rows {
		return
	}
	switch event := msg.(type) {
	case tea.MouseClickMsg:
		m.emu.SendMouse(uv.MouseClickEvent(event))
	case tea.MouseReleaseMsg:
		m.emu.SendMouse(uv.MouseReleaseEvent(event))
	case tea.MouseWheelMsg:
		m.emu.SendMouse(uv.MouseWheelEvent(event))
	case tea.MouseMotionMsg:
		m.emu.SendMouse(uv.MouseMotionEvent(event))
	}
}
