package tui

// The window is composed out of strings: every renderer joins what it drew to
// whatever is drawn beside it, and nothing on screen ends up knowing where it
// landed. zones is where a frame writes that down as it draws — a rectangle of
// cells, and what those cells mean — so answering a press is a lookup rather
// than the layout computed a second time. See ADR 0085 §3: the one place this
// package computed a position twice, the column header over its rows, drifted
// out of line the moment a column dropped, which is why tailColumns budgets
// both from one arithmetic.
//
// A renderer marks in the coordinates it is drawing in — its own top left is
// (0, 0) — and whatever places that block pushes the origin it is placing it
// at. So no renderer has to know where in the window it ended up, and a block
// that moves takes its controls with it.
//
// The map read by a press is the one the last frame left behind, which is
// exactly the frame the press was aimed at. parseChrome reads Model.lastFrame
// on the same reasoning.

// hitKind is what a cell means.
type hitKind int

const (
	// The zero value is "nothing here", which is what a lookup that hit no
	// mark comes back with. It is not named: a control is what it does, and a
	// hit map that could be asked to record nothing is one with a bug in it.
	_ hitKind = iota

	// hitKey is a key hint made pressable: the press is handled as that key,
	// by the same handler the keyboard reaches, so the two cannot drift. A
	// hint is drawn by the screen whose keys it names, which is what makes
	// the key land where the hint promised. See ADR 0085 §5.
	hitKey

	// hitListKey is the same key press, aimed at the discobox list wherever
	// the keyboard happens to be. The list's title band offers keys that
	// belong to the list — A for the archived, c for the marks — and it is
	// drawn whether or not the list has focus; typing them into the composer
	// is not what pressing them there meant.
	hitListKey

	// hitRow and its siblings are one row of a list, idx counting from the
	// top of what that list holds rather than from what is on screen.
	hitRow
	hitHarnessRow
	hitSecretRow
	hitRequestRow

	// hitOptionRow is a row of the run options panel; hitOptionCycle is one of
	// the ‹ › arrows on it, with delta saying which.
	hitOptionRow
	hitOptionCycle

	// hitDialogItem is one action of the menu a dialog is showing.
	hitDialogItem

	// hitFolder is the header's folder filter: the dropdown, closed.
	hitFolder

	// hitPrompt is the composer's text area, where a press places the caret
	// and a drag selects rather than meaning anything about the window.
	hitPrompt

	// hitChips is the strip under the composer that says what Enter will run:
	// the run options, named.
	hitChips
)

// hit is what one rectangle of cells means.
type hit struct {
	kind hitKind
	// idx is which row, option or item, for the kinds that have more than one.
	idx int
	// delta is the direction a cycling control moves in.
	delta int
	// keys are the keystrokes a hint names, spelled the way keyName spells
	// them, and pressed in order: an offer on the leader's line stands for two
	// presses, and a click on it is those two presses.
	keys []string
}

// keyHit is a hint made pressable where the keyboard already is.
func keyHit(keys ...string) hit { return hit{kind: hitKey, keys: keys} }

// listKeyHit is a hint the discobox list owns wherever focus happens to be.
func listKeyHit(keys ...string) hit { return hit{kind: hitListKey, keys: keys} }

// zone is one marked rectangle, in absolute cells of the composed frame.
type zone struct {
	what          hit
	x, y          int
	width, height int
}

// drawn is what a list's last render put on screen: the row its first entry
// was drawn on, counted from the top of its own block, how many followed it,
// and which entry the first was. It is recorded by the loop that draws the
// rows, because that loop is the only place that knows — a second count of
// "how many rows fit" is a second arithmetic to drift.
type drawn struct {
	top, count, first int
}

// zones is one frame's hit map.
type zones struct {
	marks  []zone
	ox, oy int
	stack  [][2]int
}

// reset empties the map for a new frame and puts the origin back in the
// window's top left corner.
func (z *zones) reset() {
	z.marks = z.marks[:0]
	z.ox, z.oy = 0, 0
	z.stack = z.stack[:0]
}

// push moves the origin to where the block about to be drawn is being placed,
// relative to wherever the origin is now. pop puts it back.
func (z *zones) push(dx, dy int) {
	z.stack = append(z.stack, [2]int{z.ox, z.oy})
	z.ox, z.oy = z.ox+dx, z.oy+dy
}

func (z *zones) pop() {
	if len(z.stack) == 0 {
		return
	}
	z.ox, z.oy = z.stack[len(z.stack)-1][0], z.stack[len(z.stack)-1][1]
	z.stack = z.stack[:len(z.stack)-1]
}

// count is how many marks have been made, which is the handle a caller takes
// before drawing a block it cannot place — or cannot answer — until it has
// drawn it.
func (z *zones) count() int { return len(z.marks) }

// drop forgets everything marked since count returned from, for a frame whose
// controls cannot be pressed: the opening prompt is printed inline, where a
// mouse coordinate is the terminal's screen rather than this frame, so marks
// made there would be read against the wrong rows if they were kept.
func (z *zones) drop(from int) {
	if from >= 0 && from <= len(z.marks) {
		z.marks = z.marks[:from]
	}
}

// shift moves everything marked since count returned from, for the blocks that
// are centered: a modal is rendered before anything knows how big it is, so
// where it lands is only settled afterwards.
func (z *zones) shift(from, dx, dy int) {
	for i := from; i < len(z.marks); i++ {
		z.marks[i].x += dx
		z.marks[i].y += dy
	}
}

// mark records what a rectangle of cells means, in the coordinates its caller
// is drawing in.
func (z *zones) mark(what hit, x, y, width, height int) {
	if width <= 0 || height <= 0 {
		return
	}
	z.marks = append(z.marks, zone{what: what, x: z.ox + x, y: z.oy + y, width: width, height: height})
}

// markRow is one full-width row of a list: the common case, and the one worth
// not spelling out at every call.
func (z *zones) markRow(what hit, y, width int) { z.mark(what, 0, y, width, 1) }

// find is where a control was drawn, for the code that has to point at one
// between presses: a drag inside the composer needs the field's own origin,
// and the press that started it is over.
func (z *zones) find(kind hitKind) (zone, bool) {
	for i := len(z.marks) - 1; i >= 0; i-- {
		if z.marks[i].what.kind == kind {
			return z.marks[i], true
		}
	}
	return zone{}, false
}

// markList marks a list's block: the whole of it as the list itself — which is
// what the wheel scrolls and what a press anywhere in it focuses — and then
// one row per row the draw actually put on screen, over the top of it.
//
// A row is idx from the top of what the list holds; the block is idx -1, which
// is "this list, no row in particular".
func (z *zones) markList(kind hitKind, d drawn, width, height int) {
	z.mark(hit{kind: kind, idx: -1}, 0, 0, width, height)
	for i := range d.count {
		z.markRow(hit{kind: kind, idx: d.first + i}, d.top+i, width)
	}
}

// at is the control under a press, and whether the cell means anything. The
// last mark wins: a control drawn over another is the one actually on screen.
// The whole zone comes back rather than only its meaning, because a control
// that takes a coordinate — the composer, which turns a press into a caret —
// needs the origin it was drawn at to subtract.
func (z *zones) at(x, y int) (zone, bool) {
	for i := len(z.marks) - 1; i >= 0; i-- {
		m := z.marks[i]
		if x >= m.x && x < m.x+m.width && y >= m.y && y < m.y+m.height {
			return m, true
		}
	}
	return zone{}, false
}
