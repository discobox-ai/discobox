package tui

import "fmt"

// A column is one side of the workspace: a strip of panes with one visible at
// a time, ordered by when their sessions were created.
//
// Both sides are the same thing now — the discobox's terminals on the left,
// its shells on the right — so the strip, its labels and its arithmetic are
// written once. What differs is what goes in them (see terminalExec) and that
// the left column's first pane is the primary, whose ending ends the
// workspace.
type column struct {
	panes []*pane
	// active is the pane being drawn, and the one taking the keys while this
	// column has the focus.
	active int
}

func (c *column) len() int { return len(c.panes) }

// visible is the pane being drawn, or nil when the column is empty. The index
// is clamped rather than trusted: a pane can leave the strip between the frame
// that chose the active one and the frame that draws it.
func (c *column) visible() *pane {
	if len(c.panes) == 0 {
		return nil
	}
	return c.panes[min(max(c.active, 0), len(c.panes)-1)]
}

// first is the pane at the head of the strip — the primary, in the terminals —
// or nil when there is none.
func (c *column) first() *pane {
	if len(c.panes) == 0 {
		return nil
	}
	return c.panes[0]
}

// byExec finds the pane drawing the given session, held panes included.
func (c *column) byExec(execID string) *pane {
	for _, p := range c.panes {
		if p.execID == execID {
			return p
		}
	}
	return nil
}

// index is where a pane sits in the strip, or -1 when it is not in this one.
func (c *column) index(p *pane) int {
	for i, s := range c.panes {
		if s == p {
			return i
		}
	}
	return -1
}

// insert puts a pane where its session's age says it goes, so the strip holds
// its order as the listing changes around it, and returns where it landed.
//
// focused says whether the keys are in this column: the pane being worked in
// stays the pane being worked in, since an arrival must not move them onto a
// different session mid-word.
func (c *column) insert(p *pane, exec Exec, focused bool) int {
	p.created = exec.CreatedAt
	at := len(c.panes)
	for i, s := range c.panes {
		if execBefore(exec, Exec{ID: s.execID, CreatedAt: s.created}) {
			at = i
			break
		}
	}
	c.panes = append(c.panes, nil)
	copy(c.panes[at+1:], c.panes[at:])
	c.panes[at] = p
	if at <= c.active && focused {
		c.active++
	}
	c.active = min(c.active, len(c.panes)-1)
	return at
}

// close ends one pane's stream and takes it out of the strip. It is how a held
// pane is dismissed and how an errored one is dropped — never how a running
// session is ended: that is the session's own to do.
func (c *column) close(i int) {
	if i < 0 || i >= len(c.panes) {
		return
	}
	_ = c.panes[i].term.Close()
	c.panes = append(c.panes[:i], c.panes[i+1:]...)
	if i < c.active {
		c.active--
	}
	c.active = min(c.active, max(len(c.panes)-1, 0))
}

// closeAll ends every stream in the strip and empties it. The sessions
// themselves keep running: closing the window onto a terminal is not the same
// as ending it.
func (c *column) closeAll() {
	for _, p := range c.panes {
		_ = p.term.Close()
	}
	c.panes, c.active = nil, 0
}

// label is one pane's tab: the number it answers to behind the leader, what it
// is running, and whether it is over. base is where this column's numbering
// starts — the terminals from 0, the shells after them — so one press of the
// leader and a digit means one pane on the whole screen.
func (c *column) label(i, base int) string {
	p := c.panes[i]
	title := p.name()
	name := fmt.Sprintf("%d %s", base+i, title)
	if p.exited {
		name += " ·done"
	}
	return name
}
