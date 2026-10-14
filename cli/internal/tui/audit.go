package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// The audit screen is one discobox's audit trails as a single timeline,
// followed: what `discobox admin audit list --follow` prints, as a list to move
// through. The leader opens it from the workspace and it is drawn over the
// workspace the way a tool window is — the terminals underneath keep running,
// unresized — and Enter opens the highlighted record in full, as
// `discobox admin audit get` prints it, on the scrolling card the help is read
// on.
//
// The timeline reads the way the command's does: oldest at the top, newest at
// the bottom, and the cursor riding the newest record while it is following.
// Moving off the newest stops the cursor there — a record arriving must never
// move the row being read — and moving back down to it, or End, follows again.
//
// Consecutive records that have the same Group — http exchanges with one
// method, status and origin, which is what an agent polling or paging an API
// looks like — fold into one row that counts them. Enter or → opens the run to
// its records and ← folds it again; Enter on a record opens it in full. A run
// is only ever consecutive records: one from another trail in between is part
// of what happened, and folding across it would reorder the timeline.
//
// Which trails, and how they are read, is the command's answer rather than
// this screen's (DataSource.FollowAudit): the two are one reader, so what the
// console shows and what the command prints cannot drift apart.

const (
	// auditKey opens the screen behind the leader. A for audit: the workspace
	// carries the list's action keys, and none of them is A — on the list it
	// only shows the archived rows, which is not a key the workspace has.
	auditKey = "A"
	// auditHistory is how many records the screen holds before it lets go of
	// the oldest. A followed trail grows for as long as the screen is up, and
	// a busy discobox records a request a second; the record itself is still
	// on the server, and `audit list` reaches back past this.
	auditHistory = 10_000
	// auditFeedCapacity is how many updates may wait for the window. The
	// reader never drops one — a record dropped here is one the follower has
	// already moved past — so this is only slack for a burst.
	auditFeedCapacity = 16
)

// auditTitle names the card a record is opened on, so a record arriving after
// the card it was for has gone can tell.
const auditTitle = "Audit record"

// auditScreen is the audit screen's state while it is up.
type auditScreen struct {
	// gen is which opening of the screen this is. A reader still in flight
	// from one that was closed must not land on the next.
	gen     int
	sandbox string
	name    string
	stop    context.CancelFunc

	// records are oldest first, as they are drawn. shown is which of them the
	// search lets through, by index — all of them when there is none — and
	// rows are what is drawn of those: a record, a run folded into one, or a
	// record of an opened run. cursor and offset count rows. expanded is the
	// opened runs, by the ID of their first record, which does not change as
	// the run grows.
	records  []AuditRecord
	shown    []int
	rows     []auditRow
	expanded map[string]bool
	cursor   int
	offset   int

	// query is the / search, and typing whether it is still being typed:
	// while it is, the letters are the query's and ↑↓ still move, the way
	// they do in fzf.
	query  string
	search fuzzyQuery
	typing bool
	// following is whether the cursor rides the newest record.
	following bool
	// polled is whether any read of the trails has finished, which is what
	// tells "nothing recorded" from "not read yet".
	polled bool
	// missing is what the last read could not reach; err is the whole read
	// having failed, after which nothing more arrives.
	missing []string
	err     string

	// page is how many rows the last draw had room for.
	page  int
	drawn drawn
}

// auditRow is one row of the timeline: shown[first] alone, or the run
// shown[first..last] — folded, or as the head of its opened records — or one
// record inside an opened run.
type auditRow struct {
	first, last int
	member      bool
}

func (r auditRow) run() bool { return r.last > r.first }

// auditRowKey is a row's identity across rebuilds: the record it starts with,
// and whether it is a member of an opened run. A record alone and the head of
// a run are the same row, so a record the next one arrives to join keeps the
// cursor.
type auditRowKey struct {
	id     string
	member bool
}

// openAuditMsg is the leader plus auditKey inside a pane.
type openAuditMsg struct{}

// auditUpdateMsg is one delivery from the followed timeline, with the feed it
// came from so the next read can be asked of the same one.
type auditUpdateMsg struct {
	gen    int
	feed   chan AuditUpdate
	update AuditUpdate
}

// auditEndedMsg is the follow returning: the screen was closed, or the read
// failed as a whole.
type auditEndedMsg struct {
	gen int
	err error
}

// auditDetailMsg is one record read in full, for its card.
type auditDetailMsg struct {
	gen    int
	record AuditRecord
	detail AuditRecordDetail
	err    error
}

// auditBodyMsg is one recording of an http record, read for the card it was
// asked from.
type auditBodyMsg struct {
	from   *dialog
	record AuditRecord
	part   string
	text   string
	err    error
}

// auditRecordings are the keys a record's card reads its recordings on, and
// what each is called: b for the body the service answered, which is the one
// most worth reading, r for what was sent, s for an upgraded stream.
var auditRecordings = []struct{ part, key, label string }{
	{AuditResponseBody, "b", "response body"},
	{AuditRequestBody, "r", "request body"},
	{AuditStream, "s", "stream"},
}

// openAudit puts the audit screen over the workspace and starts following the
// discobox's trails. Opening it again starts over: the records are the
// server's, and the screen holds only what it has been shown.
func (m *Model) openAudit() tea.Cmd {
	box := m.currentBox()
	m.closeAudit()
	m.auditGen++
	ctx, stop := context.WithCancel(m.ctx)
	a := &auditScreen{gen: m.auditGen, sandbox: box.ID, name: displayName(box), stop: stop, following: true,
		expanded: map[string]bool{}}
	m.audit = a
	feed := make(chan AuditUpdate, auditFeedCapacity)
	ds, id, gen := m.ds, box.ID, a.gen
	follow := func() tea.Msg {
		defer close(feed)
		err := ds.FollowAudit(ctx, id, func(update AuditUpdate) {
			// Never dropped, unlike a narrated line: the follower has
			// already moved its position past these records. Closing the
			// screen is what releases a send nobody will read.
			select {
			case feed <- update:
			case <-ctx.Done():
			}
		})
		if ctx.Err() != nil {
			// Closed, not failed.
			err = nil
		}
		return auditEndedMsg{gen: gen, err: err}
	}
	// Both wait by design: the follow for as long as the screen is open, the
	// read for the next update it brings.
	return tea.Batch(listen(follow), nextAudit(gen, feed))
}

// nextAudit waits for the feed's next update. It re-arms itself from the
// handler, so one call follows the screen from its first update to its last.
func nextAudit(gen int, feed chan AuditUpdate) tea.Cmd {
	return listen(func() tea.Msg {
		update, ok := <-feed
		if !ok {
			return nil
		}
		return auditUpdateMsg{gen: gen, feed: feed, update: update}
	})
}

// closeAudit takes the screen down and stops the follow, leaving the workspace
// it was drawn over exactly as it was.
func (m *Model) closeAudit() {
	if m.audit == nil {
		return
	}
	m.audit.stop()
	m.audit = nil
}

// auditUpdated takes one update into the screen and asks for the next.
func (m *Model) auditUpdated(msg auditUpdateMsg) tea.Cmd {
	a := m.audit
	if a == nil || a.gen != msg.gen {
		// From a screen already closed, whose follow is already stopped.
		return nil
	}
	update := msg.update
	if len(update.Records) > 0 {
		a.relayout(func() {
			a.records = append(a.records, update.Records...)
			if over := len(a.records) - auditHistory; over > 0 {
				// Copied rather than resliced, so what was let go of is freed.
				a.records = append([]AuditRecord(nil), a.records[over:]...)
			}
		})
	}
	if update.Polled {
		a.polled = true
		a.missing = update.Missing
	}
	return nextAudit(msg.gen, msg.feed)
}

// auditEnded records why a follow stopped, when it stopped on its own.
func (m *Model) auditEnded(msg auditEndedMsg) tea.Cmd {
	if a := m.audit; a != nil && a.gen == msg.gen && msg.err != nil {
		a.err = msg.err.Error()
	}
	return nil
}

// updateAudit is a key on the audit screen.
func (m *Model) updateAudit(msg tea.KeyPressMsg) tea.Cmd {
	a := m.audit
	if a.typing {
		a.typeQuery(msg)
		return nil
	}
	switch keyName(msg) {
	case "up", "k":
		a.move(-1)
	case "down", "j":
		a.move(1)
	case "pgup":
		a.move(-max(a.page, 1))
	case "pgdown":
		a.move(max(a.page, 1))
	case "home", "g":
		a.move(-len(a.rows))
	case "end", "G":
		a.move(len(a.rows))
	case "enter":
		if row, ok := a.at(); ok && row.run() && !row.member {
			a.toggle(!a.expanded[a.rec(row.first).ID])
			return nil
		}
		return m.openAuditRecord()
	case "right", "l":
		a.toggle(true)
	case "left", "h":
		a.toggle(false)
	case "/":
		a.typing = true
	case "esc":
		// A search is put away before the screen is: Esc is the way out of
		// the last thing done, and the filter is that.
		if a.query != "" {
			a.setQuery("")
			return nil
		}
		m.closeAudit()
	case "q", auditKey:
		m.closeAudit()
	}
	return nil
}

// typeQuery is a key while the search is being typed. Every character is the
// query's; the arrows still move through what it lets through, so the row
// wanted can be reached without leaving the line.
func (a *auditScreen) typeQuery(msg tea.KeyPressMsg) {
	switch keyName(msg) {
	case "esc":
		a.typing = false
		a.setQuery("")
	case "enter":
		// The filter stays: the keys go back to the rows it let through.
		a.typing = false
	case "backspace":
		query := []rune(a.query)
		if len(query) == 0 {
			a.typing = false
			return
		}
		a.setQuery(string(query[:len(query)-1]))
	case "up", "ctrl+p":
		a.move(-1)
	case "down", "ctrl+n":
		a.move(1)
	case "pgup":
		a.move(-max(a.page, 1))
	case "pgdown":
		a.move(max(a.page, 1))
	default:
		// Everything a terminal reports as text, space included; a modified
		// key is a command, not a letter.
		if msg.Text != "" && msg.Mod&^tea.ModShift == 0 {
			a.setQuery(a.query + msg.Text)
		}
	}
}

// pasteQuery takes a paste into the search while it is being typed, on one
// line: a record is one line, so a newline in a query could match nothing.
func (m *Model) pasteQuery(msg tea.PasteMsg) tea.Cmd {
	if a := m.audit; a != nil && a.typing {
		a.setQuery(a.query + strings.Join(strings.Fields(msg.Content), " "))
	}
	return nil
}

// setQuery changes the search and lays the rows out again under it.
func (a *auditScreen) setQuery(query string) {
	a.relayout(func() {
		a.query, a.search = query, parseFuzzy(query)
	})
}

// rec is the record a row position names.
func (a *auditScreen) rec(i int) AuditRecord { return a.records[a.shown[i]] }

// matchText is what the search is matched against on a row: what it says,
// its trail and its line.
func matchText(source, summary string) string { return source + " " + summary }

// matches reports whether the search lets a record through: by what its row
// says, or by its ID on its own. The ID is not part of the line it is matched
// against, because every http record's ID starts "http" — a fuzzy term would
// take its trailing letters from there, letting through rows nothing lit on
// screen explains.
func (a *auditScreen) matches(r AuditRecord) bool {
	if _, ok := a.search.match(matchText(r.Source, r.Summary)); ok {
		return true
	}
	_, ok := a.search.match(r.ID)
	return ok
}

// move moves the cursor, and follows again once it is back on the newest.
func (a *auditScreen) move(delta int) {
	if len(a.rows) == 0 {
		a.following = true
		return
	}
	a.cursor = min(max(a.cursor+delta, 0), len(a.rows)-1)
	a.following = a.cursor == len(a.rows)-1
}

// at is the row under the cursor.
func (a *auditScreen) at() (auditRow, bool) {
	if a.cursor < 0 || a.cursor >= len(a.rows) {
		return auditRow{}, false
	}
	return a.rows[a.cursor], true
}

// relayout makes a change to what the rows are drawn from — records arriving,
// a run opening or folding, the search — and lays them out again, keeping the
// cursor on the record it was on, or on the newest while following.
//
// The cursor is named before the change rather than after: a change can move
// every index (the history letting go of its oldest records), and a row
// looked up afterwards by its old position is some other record.
func (a *auditScreen) relayout(change func()) {
	var keep auditRowKey
	row, held := a.at()
	if held {
		keep = auditRowKey{id: a.rec(row.first).ID, member: row.member}
	}
	change()
	a.shown = a.shown[:0]
	for i, r := range a.records {
		if a.search.empty() {
			a.shown = append(a.shown, i)
		} else if a.matches(r) {
			a.shown = append(a.shown, i)
		}
	}
	a.rows = a.rows[:0]
	live := map[string]bool{}
	for i := 0; i < len(a.shown); {
		j := i
		if group := a.rec(i).Group; group != "" {
			for j+1 < len(a.shown) && a.rec(j+1).Group == group {
				j++
			}
		}
		a.rows = append(a.rows, auditRow{first: i, last: j})
		if id := a.rec(i).ID; j > i && a.expanded[id] {
			live[id] = true
			for k := i; k <= j; k++ {
				a.rows = append(a.rows, auditRow{first: k, last: k, member: true})
			}
		}
		i = j + 1
	}
	if a.search.empty() {
		// A run whose head the history let go of is not coming back. Under a
		// search a run can start at a different record, so what was opened
		// is kept for when the search is put away.
		a.expanded = live
	}
	if a.following || !held {
		a.cursor = max(len(a.rows)-1, 0)
		return
	}
	for i := len(a.rows) - 1; i >= 0; i-- {
		if r := a.rows[i]; r.member == keep.member && a.rec(r.first).ID == keep.id {
			a.cursor = i
			return
		}
	}
	// The row is gone — the search hid it, or the history let it go — so the
	// cursor goes where a search result's cursor goes in fzf, to the best
	// match, which in a timeline is the newest.
	a.cursor = max(len(a.rows)-1, 0)
	a.following = true
}

// toggle opens or folds the run under the cursor, or the one the record
// under it belongs to. Folding from inside a run puts the cursor on the run.
func (a *auditScreen) toggle(open bool) {
	row, ok := a.at()
	if !ok {
		return
	}
	if row.member {
		if open {
			return
		}
		for a.cursor > 0 && a.rows[a.cursor].member {
			a.cursor--
		}
		row = a.rows[a.cursor]
	}
	id := a.rec(row.first).ID
	if !row.run() || a.expanded[id] == open {
		return
	}
	following := a.following
	a.following = false
	a.relayout(func() {
		if open {
			a.expanded[id] = true
		} else {
			delete(a.expanded, id)
		}
	})
	a.following = following && a.cursor == len(a.rows)-1
}

// openAuditRecord reads the highlighted record in full.
func (m *Model) openAuditRecord() tea.Cmd {
	a := m.audit
	if a == nil {
		return nil
	}
	row, ok := a.at()
	if !ok || (row.run() && !row.member) {
		return nil
	}
	record := a.rec(row.first)
	m.busy = "reading " + record.ID + "…"
	ctx, ds, id, gen := m.ctx, m.ds, a.sandbox, a.gen
	return func() tea.Msg {
		detail, err := ds.AuditDetail(ctx, id, record.ID)
		return auditDetailMsg{gen: gen, record: record, detail: detail, err: err}
	}
}

// auditDetail opens a record's card, if the screen it was asked from is still
// the one up and nothing else has taken the window since.
func (m *Model) auditDetail(msg auditDetailMsg) tea.Cmd {
	m.busy = ""
	if a := m.audit; a == nil || a.gen != msg.gen || m.dialog != nil {
		return nil
	}
	if msg.err != nil {
		return m.report(true, "%s: %v", msg.record.ID, msg.err)
	}
	d := m.readableDialog(auditTitle, cardText(msg.detail.Text))
	d.titleRight = msg.record.ID
	for _, rec := range auditRecordings {
		if slices.Contains(msg.detail.Recordings, rec.part) {
			d.offers = append(d.offers, cardKey{key: rec.key, label: rec.label, act: func() tea.Cmd {
				return m.openAuditBody(d, msg.record, rec.part, rec.label)
			}})
		}
	}
	m.dialog = d
	return nil
}

// cardText is text read from a discobox, readied for a card: the card measures
// its body to wrap it, and a tab is one cell to the measure and eight to the
// terminal.
func cardText(text string) string {
	return strings.ReplaceAll(strings.TrimRight(text, "\n"), "\t", "    ")
}

// openAuditBody reads one recording of the record on the card, saying so on
// the card while it does: the card is the whole window, so the status line is
// not there to say it.
func (m *Model) openAuditBody(from *dialog, record AuditRecord, part, label string) tea.Cmd {
	a := m.audit
	if a == nil {
		return nil
	}
	from.emphasis = "reading the " + label + "…"
	ctx, ds, id := m.ctx, m.ds, a.sandbox
	return func() tea.Msg {
		text, err := ds.AuditBody(ctx, id, record.ID, part)
		return auditBodyMsg{from: from, record: record, part: part, text: text, err: err}
	}
}

// auditBody puts a recording up over the card it was asked from, and closing
// it goes back to that card. One that arrives after the card was closed is
// for nobody.
func (m *Model) auditBody(msg auditBodyMsg) tea.Cmd {
	if m.dialog != msg.from {
		return nil
	}
	label := msg.part
	for _, rec := range auditRecordings {
		if rec.part == msg.part {
			label = rec.label
		}
	}
	if msg.err != nil {
		msg.from.emphasis = label + ": " + msg.err.Error()
		return nil
	}
	msg.from.emphasis = ""
	d := m.readableDialog(strings.ToUpper(label[:1])+label[1:], cardText(msg.text))
	d.titleRight = msg.record.ID
	d.back = msg.from
	m.dialog = d
	return nil
}

// pressAuditRow puts the cursor on a row and, on the second press, does what
// Enter does there: opens a run, or a record in full.
func (m *Model) pressAuditRow(idx, clicks int) tea.Cmd {
	a := m.audit
	if a == nil || idx < 0 || idx >= len(a.rows) {
		return nil
	}
	a.move(idx - a.cursor)
	if clicks > 1 {
		return m.updateAudit(keyPress("enter"))
	}
	return nil
}

// auditHints is the audit screen's key line.
func (m *Model) auditHints() []hint {
	a := m.audit
	hints := []hint{says("↑↓ move")}
	if row, ok := a.at(); ok {
		switch {
		case row.member:
			hints = append(hints, keyed("Enter", "enter", "open"), keyed("←", "left", "fold"))
		case row.run() && a.expanded[a.rec(row.first).ID]:
			hints = append(hints, keyed("Enter", "enter", "fold"))
		case row.run():
			hints = append(hints, keyed("Enter", "enter", "expand"))
		default:
			hints = append(hints, keyed("Enter", "enter", "open"))
		}
	}
	if a.typing {
		// The line is the query's: every letter is typed into it.
		return []hint{says("type to filter"), says("↑↓ move"), keyed("Enter", "enter", "keep"), keyed("Esc", "esc", "clear")}
	}
	if !a.following {
		hints = append(hints, keyed("End", "end", "follow"))
	}
	if a.query != "" {
		return append(hints, keyed("/", "/", "edit search"), keyed("Esc", "esc", "clear search"))
	}
	return append(hints,
		keyed("/", "/", "search"),
		keyed("Esc", "esc", "back to the box"),
		pressing(m.leader()+" "+paneQuitKey+" quit", m.leader(), paneQuitKey),
	)
}

// viewAuditBox draws the audit screen in the box a tool window would have: the
// whole window under the header, the same size a pane there would be.
func (m *Model) viewAuditBox(width int) string {
	a := m.audit
	edge := m.st.frame
	inner := max(width-2, 1)
	cols, rows := m.paneCells(width)
	side := edge.Render("│")
	pad := strings.Repeat(" ", boxPad)

	m.zones.push(m.paneOrigin(nil))
	lines := a.view(m.st, &m.zones, cols, rows, time.Now())
	m.zones.pop()

	out := []string{titledEdge(m.st, edge, "Audit · "+a.name, "", inner)}
	for _, line := range lines {
		out = append(out, side+pad+padANSI(line, cols)+pad+side)
	}
	out = append(out, edge.Render("╰"+strings.Repeat("─", inner)+"╯"))
	return strings.Join(out, "\n")
}

// auditTimeWidth and auditSourceWidth are the timeline's fixed columns: a
// clock time, or a date and time for a record from another day, and the
// longest trail name.
const (
	auditTimeWidth   = 12
	auditSourceWidth = 7
)

// view is the screen's body at width by height: the column heads, the
// records, and under them what could not be read. The rows are marked in the
// coordinates the caller pushed.
func (a *auditScreen) view(st *styles, z *zones, width, height int, now time.Time) []string {
	// What is missing is said under the records, and outranks them: a quiet
	// timeline and a trail that stopped answering look the same without it.
	var foot []string
	for _, gap := range a.missing {
		foot = append(foot, st.statusER.Render(truncate(gap, width)))
	}
	if a.err != "" {
		foot = append(foot, st.statusER.Render(truncate("the audit trails could not be read: "+a.err, width)))
	}
	foot = foot[:min(len(foot), max(height-2, 0))]
	// The search line is the last row of the list, where fzf keeps its
	// prompt: under the records it narrows, with how many it let through.
	if a.typing || a.query != "" {
		prompt := st.key.Render("/") + a.query
		if a.typing {
			prompt += st.key.Render("▏")
		}
		tally := st.dimText.Render(fmt.Sprintf("%d of %d", len(a.shown), len(a.records)))
		foot = append([]string{spread(truncate(prompt, max(width-lipgloss.Width(tally)-2, 1)), tally, width)}, foot...)
	}

	state := st.statusOK.Render("following")
	if row, ok := a.at(); ok && !a.following {
		// A folded run holds its own newer records; they are not below it.
		through := row.first
		if row.run() && !a.expanded[a.rec(row.first).ID] {
			through = row.last
		}
		one, many := "newer record", "newer records"
		if a.query != "" {
			one, many = "newer match", "newer matches"
		}
		state = st.statusWA.Render(plural(len(a.shown)-1-through, one, many) + " below")
	}
	heads := st.dimText.Render(pad("  TIME", 2+auditTimeWidth+2) + pad("SOURCE", auditSourceWidth+2) + "RECORD")
	out := []string{spread(heads, state, width)}

	a.page = max(height-1-len(foot), 0)
	a.clamp()
	a.drawn = drawn{top: len(out), first: a.offset}
	switch {
	case len(a.records) == 0 && a.err == "":
		note := "reading the audit trails…"
		if a.polled {
			note = "nothing recorded yet — records appear here as they are"
		}
		if a.page > 0 {
			out = append(out, st.dimText.Render(truncate("  "+note, width)))
		}
	case len(a.rows) == 0 && a.query != "":
		if a.page > 0 {
			out = append(out, st.dimText.Render(truncate("  no records match — Esc clears the search", width)))
		}
	default:
		for i := a.offset; i < len(a.rows) && len(out)-1 < a.page; i++ {
			out = append(out, a.row(st, i, width, now))
			a.drawn.count++
		}
	}
	for len(out) < height-len(foot) {
		out = append(out, "")
	}
	z.markList(hitAuditRow, a.drawn, width, len(out))
	return append(out, foot...)
}

// clamp keeps the cursor on screen, scrolling as little as it takes.
func (a *auditScreen) clamp() {
	a.cursor = min(max(a.cursor, 0), max(len(a.rows)-1, 0))
	if a.page <= 0 {
		return
	}
	if a.cursor < a.offset {
		a.offset = a.cursor
	}
	if a.cursor >= a.offset+a.page {
		a.offset = a.cursor - a.page + 1
	}
	a.offset = min(max(a.offset, 0), max(len(a.rows)-a.page, 0))
}

// row draws one row: when, which trail, and what it says. A run says what
// its records have in common and how many there are, at the time of the
// newest, behind a ▸ that opens it; an opened run's records hang under it.
func (a *auditScreen) row(st *styles, i, width int, now time.Time) string {
	row := a.rows[i]
	r := a.rec(row.first)
	summary, fold := r.Summary, ""
	switch {
	case row.member:
		fold = "  "
	case row.run():
		r = a.rec(row.last)
		summary = fmt.Sprintf("%s (count %d)", r.GroupSummary, row.last-row.first+1)
		fold = "▸ "
		if a.expanded[a.rec(row.first).ID] {
			fold = "▾ "
		}
	}
	marker, text := "  ", st.name
	if i == a.cursor {
		marker, text = st.key.Render("❯")+" ", st.cursorName
	}
	// What the search found is lit where it is drawn: matched against the
	// row as it reads, so a run is lit where its own line matches.
	hits, _ := a.search.match(matchText(r.Source, summary))
	source := litRunes(pad(r.Source, auditSourceWidth), hits, 0, lipgloss.NewStyle(), st.match)
	lead := marker + st.dimText.Render(pad(auditTime(r.Time, now), auditTimeWidth)) + "  " +
		source + "  " + st.dimText.Render(fold)
	summary = truncate(summary, max(width-lipgloss.Width(lead), 1))
	return padANSI(lead+litRunes(summary, hits, len([]rune(r.Source))+1, text, st.match), width)
}

// litRunes draws text in base, with the runes a search found in hit. hits
// count runes of the text that was matched, and text starts at rune from in
// it. A run of one style is rendered once, not a rune at a time.
func litRunes(text string, hits map[int]bool, from int, base, hit lipgloss.Style) string {
	if len(hits) == 0 {
		return base.Render(text)
	}
	var b strings.Builder
	runes := []rune(text)
	for i := 0; i < len(runes); {
		lit := hits[from+i]
		j := i + 1
		for j < len(runes) && hits[from+j] == lit {
			j++
		}
		if lit {
			b.WriteString(hit.Render(string(runes[i:j])))
		} else {
			b.WriteString(base.Render(string(runes[i:j])))
		}
		i = j
	}
	return b.String()
}

// auditTime is when a record was recorded, as a clock time: the command
// prints one while following for the reason this screen does, that a relative
// time is stale the moment it is drawn. A record from another day says which.
func auditTime(at, now time.Time) string {
	at, now = at.Local(), now.Local()
	if y, mo, d := at.Date(); y == now.Year() && mo == now.Month() && d == now.Day() {
		return at.Format("15:04:05")
	}
	return at.Format("Jan 02 15:04")
}
