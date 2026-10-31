package tui

import (
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// An attach window that is only the primary terminal draws no chrome: no
// header, no border — the terminal and one line under it saying where you are
// and the two ways on. It is still the multiplexer underneath, so the leader
// and everything behind it work as they always do.
//
// The chrome comes back the moment there is something for it to arrange: a
// second terminal, a shell, a tool, an overlay, the credential band.

// bare reports whether the window is drawn without chrome.
func (m *Model) bare() bool {
	if m.attach == nil || m.screenPane() != nil || m.audit != nil || m.wsOpening {
		return false
	}
	if m.terminals.len() != 1 || m.shells.len() != 0 || m.bannerShowing() != bannerNone {
		return false
	}
	p := m.terminals.visible()
	return p != nil && p == m.primary() && !p.exited && p.issue == nil
}

// viewBare draws the primary terminal over the whole screen but its last row,
// and the status line in that one.
func (m *Model) viewBare() string {
	m.tabSpans, m.buttonSpans = m.tabSpans[:0], m.buttonSpans[:0]
	m.banner = bannerSpan{}

	lines := m.terminals.visible().term.View()
	rows := make([]string, 0, m.height)
	for i := 0; i < m.height-1; i++ {
		line := ""
		if i < len(lines) {
			line = lines[i]
		}
		rows = append(rows, padANSI(line, m.width))
	}
	return strings.Join(append(rows, m.bareStatus()), "\n")
}

// bareStatus is the bare window's one line: discobox and where the box was
// cut from on the left, a message when there is one, the desktop and the
// forwarded ports in the middle, and the two keys on the right. The middle is
// muted like the keys: it is there to be found, not read on every glance.
func (m *Model) bareStatus() string {
	w := m.width
	leader := m.leader()
	hints := []hint{
		pressing(leader+" "+boxMenuKey+" actions", leader, boxMenuKey),
		pressing(m.detachHint()+" detach", leader, paneDetachAlt),
	}
	keysW := hintsWidth(hints, hintSep)
	x := max(w-keysW-1, 0)
	m.zones.push(0, m.height-1)
	defer m.zones.pop()
	right := viewHints(m.st, &m.zones, hints, x, hintSep) + " "

	left := " " + m.st.headerLabel.Render("discobox  ") + m.st.dimText.Render(m.boxSourceLabel())
	room := x - lipgloss.Width(left) - 3
	if msg := m.statusMessage(max(room, 1)); msg != "" && room > 4 {
		left += "  " + msg
	}
	left = ansi.Truncate(left, max(x-1, 0), "")

	var fields paneHeaderFields
	box := m.currentBox()
	for _, f := range []paneHeaderField{desktopField(m.st, box, m.forwardedPorts()), portsField(m.st, box, m.forwardedPorts())} {
		if f.empty() {
			continue
		}
		for i, span := range f.spans {
			// Redrawn muted from the bare label, a link keeping its OSC 8;
			// render marks and lights it like the header's.
			text := m.st.dimText.Render(span.label)
			if span.url != "" {
				text = hyperlink(span.url, text)
			}
			f.spans[i].text = text
		}
		fields = append(fields, f)
	}
	fields = fields.fit(centerRoom(left, right, w))
	if lipgloss.Width(fields.text()) > centerRoom(left, right, w) {
		fields = nil
	}
	return spreadCenter(left, fields.render(m, left, right, w), right, w)
}
