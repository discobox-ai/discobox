package tui

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// openAuditScreen opens the workspace on the first discobox and the audit
// screen over it, waiting until it is following.
func openAuditScreen(t *testing.T, ds *fakeSource) (*driver, *Model, *fakeTerminal) {
	t.Helper()
	ds.audit = make(chan AuditUpdate)
	d, m, term := openWorkspace(t, ds, "enter")
	d.key("ctrl+a")
	d.key(auditKey)
	d.wait("the audit screen", func() bool {
		follows, _, _ := ds.auditState()
		return m.audit != nil && len(follows) == 1
	})
	return d, m, term
}

// deliverAudit hands the screen one update, the way the follow does.
func deliverAudit(d *driver, ds *fakeSource, update AuditUpdate) {
	ds.audit <- update
	d.settle()
}

// auditIDs numbers the records the tests make, so every one is its own: the
// screen finds the row it was on again by its record's ID.
var auditIDs atomic.Int64

func auditRecords(summaries ...string) []AuditRecord {
	at := time.Now().Add(-time.Minute)
	out := make([]AuditRecord, 0, len(summaries))
	for i, summary := range summaries {
		out = append(out, AuditRecord{
			ID:      fmt.Sprintf("http_%d", auditIDs.Add(1)),
			Time:    at.Add(time.Duration(i) * time.Second),
			Source:  "http",
			Summary: summary,
		})
	}
	return out
}

// The screen is the discobox's own timeline, followed: it is opened from the
// workspace on the box on screen, it says it is reading before anything has
// arrived, and it rides the newest record as records arrive — the equivalent
// of `audit ls --follow`.
func TestTheAuditScreenFollowsTheBoxOnScreen(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openAuditScreen(t, ds)

	if follows, _, _ := ds.auditState(); follows[0] != "sbx_one" {
		t.Fatalf("followed %v, want the discobox on screen", follows)
	}
	frame := frameText(m)
	for _, want := range []string{"Audit · fix flaky pool reaper tests", "reading the audit trails"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("the screen should say %q before anything arrives:\n%s", want, frame)
		}
	}

	deliverAudit(d, ds, AuditUpdate{Polled: true})
	if frame := frameText(m); !strings.Contains(frame, "nothing recorded yet") {
		t.Fatalf("a read that found nothing should say so:\n%s", frame)
	}

	records := auditRecords("GET 200 https://api.github.com/user", "POST 201 https://api.anthropic.com/v1/messages")
	deliverAudit(d, ds, AuditUpdate{Records: records})
	frame = frameText(m)
	for _, want := range []string{"GET 200 https://api.github.com/user", "❯ " + auditTime(records[1].Time, time.Now()), "following"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("the timeline should show %q, with the cursor on the newest:\n%s", want, frame)
		}
	}

	deliverAudit(d, ds, AuditUpdate{Records: []AuditRecord{{ID: "cvd_x", Time: time.Now(), Source: "creds", Summary: "use github (at use): approved"}}})
	if m.audit.cursor != 2 {
		t.Fatalf("cursor = %d, want it to follow onto the newest record", m.audit.cursor)
	}
}

// Moving off the newest record stops following it — a record arriving must
// not move the row being read — and the screen says how much has arrived
// below. End, or moving back down to the newest, follows again.
func TestTheAuditScreenHoldsTheRowBeingRead(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openAuditScreen(t, ds)
	deliverAudit(d, ds, AuditUpdate{Records: auditRecords("first", "second", "third"), Polled: true})

	d.key("up")
	d.key("up")
	deliverAudit(d, ds, AuditUpdate{Records: auditRecords("fourth")})
	if m.audit.cursor != 0 || m.audit.following {
		t.Fatalf("cursor = %d following = %v, want it left on the first record", m.audit.cursor, m.audit.following)
	}
	if frame := frameText(m); !strings.Contains(frame, "3 newer records below") {
		t.Fatalf("the screen should say what arrived below:\n%s", frame)
	}

	d.key("end")
	if m.audit.cursor != 3 || !m.audit.following {
		t.Fatalf("cursor = %d following = %v, want End to follow the newest", m.audit.cursor, m.audit.following)
	}
	d.key("up")
	d.key("down")
	if !m.audit.following {
		t.Fatal("moving back onto the newest record should follow again")
	}
}

// Enter opens the highlighted record in full, as `audit get` prints it, on a
// card that closes back to the timeline; Esc then goes back to the box and
// stops the follow.
func TestTheAuditScreenOpensARecordAndClosesBackToTheBox(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	records := auditRecords("GET 200 https://api.github.com/user")
	ds.auditDetails = map[string]string{
		records[0].ID: "record:   " + records[0].ID + "\nmethod:   GET\nrequest headers:\n\tAccept: */*\n",
	}
	d, m, term := openAuditScreen(t, ds)
	deliverAudit(d, ds, AuditUpdate{Records: records, Polled: true})

	d.key("enter")
	d.wait("the record's card", func() bool { return m.dialog != nil && m.dialog.title == auditTitle })
	if _, _, reads := ds.auditState(); len(reads) != 1 || reads[0] != records[0].ID {
		t.Fatalf("read %v, want the highlighted record", reads)
	}
	if m.dialog.titleRight != records[0].ID || !strings.Contains(m.dialog.body, "method:   GET") {
		t.Fatalf("card = %q %q, want the record in full", m.dialog.titleRight, m.dialog.body)
	}
	if strings.Contains(m.dialog.body, "\t") {
		t.Fatal("a tab is measured as one cell and drawn as eight; the card should expand it")
	}

	d.key("esc")
	if m.dialog != nil || m.audit == nil {
		t.Fatal("closing the card should leave the timeline up")
	}
	// Keys on the timeline are the timeline's: none reaches the terminal it
	// is drawn over.
	d.key("x")
	if typed := term.typed(""); typed != "" {
		t.Fatalf("the terminal under the timeline was typed %q", typed)
	}

	d.key("esc")
	if m.audit != nil {
		t.Fatal("Esc should take the audit screen down")
	}
	d.wait("the follow to stop", func() bool {
		_, stops, _ := ds.auditState()
		return stops == 1
	})
	if m.focus != focusPane || m.primary() == nil {
		t.Fatal("closing the audit screen should leave the workspace as it was")
	}
}

// A trail that stops answering is said on the screen, where a quiet timeline
// would otherwise look like one with nothing to say, and goes when it answers
// again.
func TestTheAuditScreenSaysWhatItCouldNotRead(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openAuditScreen(t, ds)
	gap := "hooks could not be read, so its records are missing: the discobox is stopped"
	deliverAudit(d, ds, AuditUpdate{Records: auditRecords("GET 200 https://api.github.com/user"), Polled: true, Missing: []string{gap}})
	if frame := frameText(m); !strings.Contains(frame, gap) {
		t.Fatalf("the screen should name the trail it could not read:\n%s", frame)
	}
	deliverAudit(d, ds, AuditUpdate{Polled: true})
	if frame := frameText(m); strings.Contains(frame, gap) {
		t.Fatalf("a trail that answers again should stop being reported:\n%s", frame)
	}
}

// A row is pointed at with one press and opened with two, the way the
// discobox list's rows are.
func TestTheAuditScreenAnswersThePointer(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	records := auditRecords("GET 200 https://api.github.com/user", "POST 201 https://api.anthropic.com/v1/messages")
	ds.auditDetails = map[string]string{records[0].ID: "record: " + records[0].ID}
	d, m, _ := openAuditScreen(t, ds)
	deliverAudit(d, ds, AuditUpdate{Records: records, Polled: true})

	x, y := at(t, m, "GET 200 https://api.github.com/user")
	d.dispatch(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	if m.audit.cursor != 0 || m.audit.following {
		t.Fatalf("cursor = %d following = %v, want a press to point at the row", m.audit.cursor, m.audit.following)
	}
	d.dispatch(tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft})
	d.dispatch(tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	d.wait("the record's card", func() bool { return m.dialog != nil && m.dialog.title == auditTitle })
}

// Leaving the workspace takes the audit screen with it, and its follow.
func TestDetachingEndsTheAuditFollow(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openAuditScreen(t, ds)
	m.closeWorkspace()
	if m.audit != nil {
		t.Fatal("the audit screen should close with the workspace")
	}
	d.wait("the follow to stop", func() bool {
		_, stops, _ := ds.auditState()
		return stops == 1
	})
}

// httpRun is n exchanges that fold together: one method, status and origin.
func httpRun(n int, path string) []AuditRecord {
	summaries := make([]string, n)
	for i := range summaries {
		summaries[i] = fmt.Sprintf("GET 200 https://api.github.com/%s/%d", path, i)
	}
	records := auditRecords(summaries...)
	for i := range records {
		records[i].Group = "GET 200 https://api.github.com"
		records[i].GroupSummary = "GET 200 https://api.github.com/..."
	}
	return records
}

// Consecutive exchanges with one method, status and origin fold into a row
// that counts them, and only consecutive ones: a record from another trail in
// between is part of what happened. A record that arrives to join the run is
// counted in it, with the cursor still on it.
func TestTheAuditScreenFoldsARunOfExchanges(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openAuditScreen(t, ds)
	records := httpRun(3, "user")
	records = append(records, AuditRecord{ID: "cvd_between", Time: time.Now(), Source: "creds", Summary: "use github (at use): approved"})
	records = append(records, httpRun(2, "repos")...)
	deliverAudit(d, ds, AuditUpdate{Records: records, Polled: true})

	frame := plainFrame(m)
	for _, want := range []string{"▸ GET 200 https://api.github.com/... (count 3)", "use github (at use): approved", "▸ GET 200 https://api.github.com/... (count 2)"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("the timeline should read %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "/user/0") {
		t.Fatalf("a folded run should not draw its records:\n%s", frame)
	}

	deliverAudit(d, ds, AuditUpdate{Records: httpRun(1, "repos")})
	if frame := plainFrame(m); strings.Count(frame, "(count 3)") != 2 {
		t.Fatalf("an arrival that matches the run should join it:\n%s", frame)
	}
	if len(m.audit.rows) != 3 || m.audit.cursor != 2 || !m.audit.following {
		t.Fatalf("rows = %d cursor = %d following = %v, want the joined run to stay the followed row", len(m.audit.rows), m.audit.cursor, m.audit.following)
	}
}

// Enter opens a run to its records and folds it again; Enter on one of them
// opens it in full, and ← from inside the run folds it with the cursor on it.
func TestTheAuditScreenOpensARun(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	records := httpRun(3, "user")
	ds.auditDetails = map[string]string{records[1].ID: "record: " + records[1].ID}
	d, m, _ := openAuditScreen(t, ds)
	deliverAudit(d, ds, AuditUpdate{Records: records, Polled: true})

	d.key("enter")
	frame := plainFrame(m)
	for _, want := range []string{"▾ GET 200 https://api.github.com/... (count 3)", "/user/0", "/user/1", "/user/2"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("an opened run should show %q:\n%s", want, frame)
		}
	}
	if m.dialog != nil {
		t.Fatal("Enter on a run opens it, not a record")
	}
	if _, _, reads := ds.auditState(); len(reads) != 0 {
		t.Fatalf("read %v, want nothing read to open a run", reads)
	}

	d.key("down")
	d.key("down")
	d.key("enter")
	d.wait("the record's card", func() bool { return m.dialog != nil && m.dialog.title == auditTitle })
	if m.dialog.titleRight != records[1].ID {
		t.Fatalf("opened %q, want the record under the cursor", m.dialog.titleRight)
	}
	d.key("esc")

	// Arrivals below do not move the cursor off the record being read.
	deliverAudit(d, ds, AuditUpdate{Records: httpRun(1, "user")})
	if row, _ := m.audit.at(); !row.member || m.audit.records[row.first].ID != records[1].ID {
		t.Fatal("the cursor should stay on the record it was on as the run grows")
	}

	d.key("left")
	if row, _ := m.audit.at(); row.member || !row.run() || m.audit.cursor != 0 {
		t.Fatalf("← should fold the run with the cursor on it, got row %+v at %d", row, m.audit.cursor)
	}
	if frame := plainFrame(m); !strings.Contains(frame, "▸ GET 200 https://api.github.com/... (count 4)") {
		t.Fatalf("the folded run should count every record in it:\n%s", frame)
	}
}

// An http record's card offers the bodies the pool kept, on keys of their own,
// and a body opens on a card that goes back to the record when it is closed —
// by Esc or by any of the keys that close a card.
func TestTheAuditScreenReadsARecordsBodies(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	records := auditRecords("POST 201 https://api.anthropic.com/v1/messages")
	id := records[0].ID
	ds.auditDetails = map[string]string{id: "record:   " + id + "\nmethod:   POST"}
	ds.auditBodies = map[string]string{
		id + "/" + AuditResponseBody: "{\n  \"id\": \"msg_01\"\n}",
		id + "/" + AuditRequestBody:  "{\"model\": \"claude\"}",
	}
	d, m, _ := openAuditScreen(t, ds)
	deliverAudit(d, ds, AuditUpdate{Records: records, Polled: true})
	d.key("enter")
	d.wait("the record's card", func() bool { return m.dialog != nil && m.dialog.title == auditTitle })
	record := m.dialog

	frame := plainFrame(m)
	for _, want := range []string{"b response body", "r request body"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("the card should offer %q:\n%s", want, frame)
		}
	}
	if strings.Contains(frame, "s stream") {
		t.Fatalf("a recording the pool did not keep should not be offered:\n%s", frame)
	}

	d.key("b")
	d.wait("the response body", func() bool { return m.dialog != nil && m.dialog.title == "Response body" })
	if m.dialog.titleRight != id || !strings.Contains(m.dialog.body, `"id": "msg_01"`) {
		t.Fatalf("body card = %q %q", m.dialog.titleRight, m.dialog.body)
	}
	d.key("esc")
	if m.dialog != record {
		t.Fatal("closing a body should go back to its record")
	}

	d.key("r")
	d.wait("the request body", func() bool { return m.dialog != nil && m.dialog.title == "Request body" })
	d.key("q")
	if m.dialog != record {
		t.Fatal("any key that closes a body should go back to its record")
	}
	d.key("esc")
	if m.dialog != nil || m.audit == nil {
		t.Fatal("closing the record should go back to the timeline")
	}
}

// A body that cannot be read says so on the card it was asked from, which is
// the whole window while it is up.
func TestTheAuditScreenSaysWhenABodyCannotBeRead(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	records := auditRecords("GET 200 https://example.com/")
	id := records[0].ID
	ds.auditDetails = map[string]string{id: "record:   " + id}
	ds.auditBodies = map[string]string{id + "/" + AuditResponseBody: ""}
	d, m, _ := openAuditScreen(t, ds)
	deliverAudit(d, ds, AuditUpdate{Records: records, Polled: true})
	d.key("enter")
	d.wait("the record's card", func() bool { return m.dialog != nil && m.dialog.title == auditTitle })
	delete(ds.auditBodies, id+"/"+AuditResponseBody)
	d.key("b")
	d.wait("the error", func() bool { return strings.Contains(plainFrame(m), "response body: no response recorded") })
	if m.dialog.title != auditTitle {
		t.Fatal("a body that could not be read should leave the record up")
	}
}

// / filters the timeline as it is typed, fzf-style, against what each record
// says and its ID; the arrows still move while typing; Enter keeps the filter
// and gives the keys back; Esc clears it before it closes anything.
func TestTheAuditScreenSearchesAsYouType(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openAuditScreen(t, ds)
	records := auditRecords(
		"GET 200 https://api.github.com/user",
		"POST 201 https://api.anthropic.com/v1/messages",
		"GET 200 https://registry.npmjs.org/react",
	)
	records = append(records, AuditRecord{ID: "cvd_needle", Time: time.Now(), Source: "creds", Summary: "use github (at use): approved"})
	deliverAudit(d, ds, AuditUpdate{Records: records, Polled: true})

	d.key("/")
	for _, key := range []string{"g", "h", "u", "b"} {
		d.key(key)
	}
	frame := plainFrame(m)
	if !strings.Contains(frame, "/ghub") || !strings.Contains(frame, "2 of 4") {
		t.Fatalf("the search line should show the query and how many it let through:\n%s", frame)
	}
	for _, want := range []string{"api.github.com/user", "use github"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("%q matches and should be shown:\n%s", want, frame)
		}
	}
	for _, gone := range []string{"anthropic", "npmjs"} {
		if strings.Contains(frame, gone) {
			t.Fatalf("%q does not match and should be filtered out:\n%s", gone, frame)
		}
	}

	// Letters are the query's while typing, including the screen's own keys.
	d.key("q")
	if m.audit == nil || m.audit.query != "ghubq" {
		t.Fatal("q while typing should be typed, not close the screen")
	}
	d.key("backspace")

	// The arrows move through what matched without leaving the line.
	d.key("up")
	if row, _ := m.audit.at(); m.audit.rec(row.first).ID != records[0].ID || !m.audit.typing {
		t.Fatal("↑ while typing should move to the older match and keep typing")
	}

	d.key("enter")
	if m.audit.typing || m.audit.query != "ghub" {
		t.Fatal("Enter should keep the filter and give the keys back")
	}
	d.key("esc")
	if m.audit == nil || m.audit.query != "" || len(m.audit.rows) != 4 {
		t.Fatal("Esc should clear the search before it closes the screen")
	}

	// The ID is searched too, though it is not a column.
	d.key("/")
	for _, key := range []string{"n", "e", "e", "d", "l", "e"} {
		d.key(key)
	}
	if len(m.audit.shown) != 1 || m.audit.rec(0).ID != "cvd_needle" {
		t.Fatalf("shown %v, want the record found by its ID", m.audit.shown)
	}
	d.key("x")
	d.key("z")
	if frame := plainFrame(m); !strings.Contains(frame, "no records match") {
		t.Fatalf("a query nothing matches should say so:\n%s", frame)
	}
}

// Records that arrive while a search is up are filtered as they arrive, and a
// run folds over what the search lets through.
func TestTheAuditSearchFiltersWhatArrives(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openAuditScreen(t, ds)
	d.key("/")
	for _, key := range []string{"g", "i", "t"} {
		d.key(key)
	}
	d.key("enter")
	deliverAudit(d, ds, AuditUpdate{Records: auditRecords("POST 201 https://api.anthropic.com/v1/messages"), Polled: true})
	deliverAudit(d, ds, AuditUpdate{Records: httpRun(3, "user")})
	frame := plainFrame(m)
	if strings.Contains(frame, "anthropic") || !strings.Contains(frame, "(count 3)") || !strings.Contains(frame, "3 of 4") {
		t.Fatalf("arrivals should be filtered, and the matches fold:\n%s", frame)
	}
}

// A paste while typing goes into the query, on one line.
func TestTheAuditSearchTakesAPaste(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openAuditScreen(t, ds)
	d.key("/")
	d.dispatch(tea.PasteMsg{Content: "api.github\ncom"})
	if m.audit.query != "api.github com" {
		t.Fatalf("query = %q, want the paste on one line", m.audit.query)
	}
}

// What a search found is lit inside the text, a run of one style at a time.
func TestLitRunesLightsTheMatches(t *testing.T) {
	t.Parallel()
	hit := lipgloss.NewStyle().Bold(true)
	got := litRunes("GET api.github.com", map[int]bool{12: true, 13: true, 15: true}, 4, lipgloss.NewStyle(), hit)
	want := "GET api." + hit.Render("gi") + "t" + hit.Render("h") + "ub.com"
	if got != want {
		t.Fatalf("litRunes = %q, want %q", got, want)
	}
}

// A row is let through by what it says, or by its ID on its own — never by
// letters borrowed from an ID nothing on the row shows. Every http record's
// ID starts "http", which a fuzzy term would otherwise end in.
func TestTheAuditSearchDoesNotBorrowFromTheID(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	d, m, _ := openAuditScreen(t, ds)
	deliverAudit(d, ds, AuditUpdate{Records: auditRecords("GET 200 https://api.github.com/user", "GET 200 https://registry.npmjs.org/react"), Polled: true})
	d.key("/")
	for _, key := range []string{"g", "i", "t", "h"} {
		d.key(key)
	}
	if len(m.audit.shown) != 1 || !strings.Contains(m.audit.rec(0).Summary, "github") {
		t.Fatalf("shown %d records, want only the one whose line says github", len(m.audit.shown))
	}
}

// The workspace's banner is still drawn over the audit screen, and the key it
// names behind the leader answers it there too — it does not move the list.
func TestTheBannerKeysAnswerOverTheAuditScreen(t *testing.T) {
	t.Parallel()
	ds := newFakeSource(testSandboxes()...)
	ds.requests = []CredentialRequest{waitingRequest()}
	ds.projectSecrets = []Secret{{ID: "sec_gh", Name: "GitHub token", Type: "bearer", Routes: []secretroute.Route{{Hosts: []string{"api.github.com"}}}}}
	d, m, _ := openAuditScreen(t, ds)
	deliverAudit(d, ds, AuditUpdate{Records: auditRecords("first", "second", "third"), Polled: true})
	d.wait("the banner", func() bool { return m.bannerShowing() != bannerNone })

	d.key("ctrl+a")
	d.key(credentialsLeaderKey)
	d.wait("the request card", func() bool { return onRequestCard(m) })
	if !m.audit.following || m.audit.cursor != 2 {
		t.Fatalf("cursor = %d following = %v: the banner's key should not have moved the list", m.audit.cursor, m.audit.following)
	}
}
