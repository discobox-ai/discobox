package tui

import (
	"strings"
	"time"

	"charm.land/lipgloss/v2"
)

// The requests table: the second half of the secrets screen. Credentials are
// what the project holds; requests are what is waiting on a person, and the two
// are read together — which secret answers this is the question, and the answer
// is on the table above.
//
// It is a table rather than a count in the title because a count is something
// you dismiss and a row is something you act on. Enter on one opens the same
// question the discobox list's mark and the workspace's banner open.

// requestList is the screen's lower table and where the cursor is in it.
type requestList struct {
	all    []CredentialRequest
	cursor int
	offset int

	width, height int

	now func() time.Time
}

func newRequestList() *requestList { return &requestList{now: time.Now} }

// setAll takes a refreshed listing, keeping the cursor on the request it was
// on: answering one is a decision about that request, and a cursor that moved
// under it is how the wrong one gets answered.
func (l *requestList) setAll(all []CredentialRequest) {
	var onID string
	if r := l.current(); r != nil {
		onID = r.ID
	}
	l.all = all
	if onID != "" {
		for i := range l.all {
			if l.all[i].ID == onID {
				l.cursor = i
				break
			}
		}
	}
	l.clamp()
}

func (l *requestList) current() *CredentialRequest {
	if l.cursor < 0 || l.cursor >= len(l.all) {
		return nil
	}
	return &l.all[l.cursor]
}

func (l *requestList) move(delta int) { l.cursor += delta; l.clamp() }

func (l *requestList) moveTo(i int) { l.cursor = i; l.clamp() }

func (l *requestList) clamp() {
	if l.cursor >= len(l.all) {
		l.cursor = len(l.all) - 1
	}
	if l.cursor < 0 {
		l.cursor = 0
	}
	if l.height <= 0 {
		l.offset = 0
		return
	}
	if l.cursor < l.offset {
		l.offset = l.cursor
	}
	if l.cursor >= l.offset+l.height {
		l.offset = l.cursor - l.height + 1
	}
	if l.offset < 0 {
		l.offset = 0
	}
}

func (l *requestList) view(st *styles, focused bool) string {
	titleStyle := st.titleList
	if !focused {
		titleStyle = st.titleDim
	}
	right := ""
	if len(l.all) > 0 {
		// In the error color, because every row is somebody blocked.
		right = st.statusER.Render(plural(len(l.all), "waiting", "waiting"))
	}
	blank := strings.Repeat(" ", max(l.width, 0))
	out := []string{renderTitle(titleStyle, "Requests", right, l.width)}

	body := make([]string, 0, l.height)
	if len(l.all) == 0 {
		body = append(body, st.dimText.Render(pad("  nothing is waiting", l.width)))
	}
	for i := l.offset; i < len(l.all) && len(body) < l.height; i++ {
		body = append(body, l.row(st, l.all[i], i, focused))
	}
	for len(body) < l.height {
		body = append(body, blank)
	}
	return lipgloss.JoinVertical(lipgloss.Left, append(out, body...)...)
}

// row draws one request: what is being asked for, where it may go, who is
// asking, and how long they have been waiting.
func (l *requestList) row(st *styles, r CredentialRequest, i int, focused bool) string {
	atCursor := i == l.cursor && focused

	tail := ""
	addCol := func(text string, w int) {
		if l.width-lipgloss.Width(tail)-w < 20 {
			return
		}
		tail += padANSI(text, w)
	}
	// The host is what the credential would be allowed to reach, which is the
	// first thing an approver reads after what is being asked for.
	addCol("  "+st.name.Render(pad(r.Host, 24)), 26)
	addCol(st.dimText.Render(pad(r.EnvVar, 18)), 19)
	addCol(st.dimText.Render(pad(requesterLabel(r), 22)), 23)
	addCol(st.dimText.Render(pad(requestAge(r, l.now()), 8)), 9)

	marker := "  "
	if atCursor {
		marker = st.key.Render("❯") + " "
	}
	what := requestSubject(r)
	nameW := max(l.width-lipgloss.Width(marker)-lipgloss.Width(tail), 4)
	nameStyle := st.name
	if atCursor {
		nameStyle = st.cursorName
	}
	line := padANSI(marker+padANSI(nameStyle.Render(truncate(what, nameW)), nameW)+tail, l.width)
	if atCursor {
		return highlight(st, line, colHighlightBG)
	}
	return line
}

// requestSubject is what is being asked for. An agent names the credential it
// wants; the proxy's reactive path names nothing, because all it saw was a
// sentinel it could not resolve — and saying so is more use than a blank.
func requestSubject(r CredentialRequest) string {
	switch {
	case r.Name != "":
		return r.Name
	case r.EnvVar != "":
		return r.EnvVar
	case r.SandboxID != "":
		return "an unresolvable sentinel"
	default:
		return "a " + r.Type + " credential"
	}
}

// requesterLabel says who is waiting, in the words the server records: an agent
// inside a discobox, the proxy on that discobox's behalf, or a person.
func requesterLabel(r CredentialRequest) string {
	switch {
	case r.FromAgent() && r.SandboxID != "":
		return "agent · " + shortID(r.SandboxID)
	case r.SandboxID != "":
		return "proxy · " + shortID(r.SandboxID)
	default:
		return "a person"
	}
}

// shortID is an identifier at the width a column has for one.
func shortID(id string) string {
	if _, rest, ok := strings.Cut(id, "_"); ok {
		id = rest
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func requestAge(r CredentialRequest, now time.Time) string {
	age := since(r.Created, now)
	if age == "" {
		return ""
	}
	return age + " ago"
}
