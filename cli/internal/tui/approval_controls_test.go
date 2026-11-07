package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// Exercise the same scroll and keyboard submission path a person uses. Tests
// must never bypass the gate by calling the card's submit callback directly.
func answerApproval(t *testing.T, m *Model, key string) {
	t.Helper()
	d := m.dialog
	if d == nil {
		t.Fatal("no approval card")
	}
	plainFrame(m)
	for i := 0; d.decisionsBlocked && i < 1000; i++ {
		d.update(keyPress("pgdown"))
		plainFrame(m)
	}
	if d.decisionsBlocked {
		t.Fatal("could not reach end of approval summary")
	}
	for i, a := range d.formActions {
		if a.key == key {
			d.formActionAt = i
			send(t, m, keyPress("enter"))
			return
		}
	}
	t.Fatalf("missing decision %s", key)
}
func refreshAnswer(t *testing.T, m *Model, key string) {
	t.Helper()
	if key == "dismiss" {
		answerApproval(t, m, "dismiss")
		return
	}
	m.dialog.form.choose("permission", key)
	answerApproval(t, m, "refresh")
}

func TestApprovalScrollGateAndChangedSummary(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	send(t, m, sizeMsg(80, 24))
	called := 0
	d := approvalDialog("Review access", newForm(), func(*form) tea.Cmd { called++; return nil })
	d.sections = []section{{lines: []line{{text: strings.Repeat("Review these permissions carefully. ", 100)}}}}
	m.dialog = d
	plainFrame(m)
	for _, at := range []int{0, 1} {
		d.formActionAt = at
		_, closed := d.update(keyPress("enter"))
		if closed || called != 0 {
			t.Fatal("unread summary was answered")
		}
	}
	for d.decisionsBlocked {
		d.update(keyPress("pgdown"))
		plainFrame(m)
	}
	d.update(keyPress("pgup"))
	plainFrame(m)
	if d.decisionsBlocked {
		t.Fatal("reading remains complete after scrolling back")
	}
	d.sections[0].lines = append(d.sections[0].lines, line{text: "New permissions"})
	plainFrame(m)
	if !d.decisionsBlocked || d.offset != 0 {
		t.Fatal("changed summary retained consent gate")
	}
}

func TestApprovalCardsFitCompactWindow(t *testing.T) {
	for _, kind := range []string{"use", "delegate", "adoption", "trust", "refresh", "api"} {
		t.Run(kind, func(t *testing.T) {
			m := newTestModel(t, newFakeSource())
			send(t, m, sizeMsg(80, 24))
			req := waitingRequest()
			switch kind {
			case "use":
				m.askAboutCredential(req, []console.Secret{{ID: "sec", Name: "GitHub"}}, nil)
			case "delegate":
				req.Delegate = req.Use
				req.Use = nil
				m.askAboutCredential(req, []console.Secret{{ID: "sec", Name: "GitHub"}}, nil)
			case "adoption":
				req.Adoption = &console.AdoptionAsk{ChildID: "child", RequesterID: "requester", CurrentParentID: "parent"}
				m.askAboutAdoption(req)
			case "trust":
				m.askAboutTrust(waitingTrust())
			case "refresh":
				m.askAboutRefresh(refreshRequest(), []console.Secret{{ID: "sec_gh", Name: "GitHub", RefreshCommand: []string{"gh", "auth", "token"}}})
			case "api":
				req.APIAccess = true
				m.askAboutAPIAccess(req)
			}
			text := plainFrame(m)
			if height := strings.Count(text, "\n") + 1; height > 24 {
				t.Fatalf("%d rows: %s", height, text)
			}
			for _, row := range strings.Split(text, "\n") {
				if ansi.StringWidth(row) > 80 {
					t.Fatalf("overwide row %s", row)
				}
			}
			button := "Accept"
			if kind == "refresh" {
				button = "Refresh"
			}
			if !strings.Contains(text, "[ "+button+" ]") {
				t.Fatalf("decision offscreen: %s", text)
			}
		})
	}
}

func TestApprovalCardsOnlyNameAServerWhenKnown(t *testing.T) {
	for _, server := range []string{"", "production"} {
		for _, kind := range []string{"use", "api", "trust"} {
			t.Run(kind+"/"+server, func(t *testing.T) {
				m := newTestModel(t, newFakeSource())
				req := waitingRequest()
				req.Server = server
				switch kind {
				case "use":
					m.askAboutCredential(req, nil, nil)
				case "api":
					m.askAboutAPIAccess(req)
				case "trust":
					trust := waitingTrust()
					trust.Server = server
					m.askAboutTrust(trust)
				}
				text := dialogText(m)
				if strings.Contains(text, "Server") != (server != "") {
					t.Fatalf("server row for %q: %s", server, text)
				}
				if server != "" && !strings.Contains(text, server) {
					t.Fatalf("server name missing: %s", text)
				}
			})
		}
	}
}

func TestApprovalMouseDecisionsUseTheGate(t *testing.T) {
	for _, button := range []string{"Accept", "Deny"} {
		t.Run(button, func(t *testing.T) {
			m := newTestModel(t, newFakeSource())
			send(t, m, sizeMsg(80, 24))
			called := 0
			d := approvalDialog("Review access", newForm(), func(*form) tea.Cmd { called++; return nil })
			d.sections = []section{{lines: []line{{text: strings.Repeat("Review access. ", 100)}}}}
			m.dialog = d
			x, y := at(t, m, "[ "+button+" ]")
			send(t, m, toScreen(m, tea.MouseClickMsg{X: x + 2, Y: y, Button: tea.MouseLeft}))
			if called != 0 {
				t.Fatal("mouse bypassed unread summary")
			}
			for d.decisionsBlocked {
				d.update(keyPress("pgdown"))
				plainFrame(m)
			}
			x, y = at(t, m, "[ "+button+" ]")
			send(t, m, toScreen(m, tea.MouseClickMsg{X: x + 2, Y: y, Button: tea.MouseLeft}))
			if called != 1 {
				t.Fatalf("button did not answer after reading: point=%d,%d zones=%+v blocked=%t state=%q expected=%q", x, y, m.zones.marks, d.decisionsBlocked, d.formState(), d.reviewState)
			}
		})
	}
}

func TestCombinedApprovalStagesIndependentDecisions(t *testing.T) {
	for _, use := range []string{"accept", "deny"} {
		for _, delegate := range []string{"accept", "deny"} {
			t.Run(use+"/"+delegate, func(t *testing.T) {
				m, ds := sourceWithRequest(t)
				req := waitingRequest()
				req.Delegate = &console.RequestPart{Routes: req.Use.Routes}
				m.askAboutCredential(req, ds.projectSecrets, nil)
				m.dialog.form.choose("scope", "hosts")
				answerApproval(t, m, use)
				if len(ds.approvals) != 0 || len(ds.denials) != 0 {
					t.Fatal("first part committed before delegation review")
				}
				locked := use == "accept"
				if m.dialog.form.rows[0].locked != locked || m.dialog.form.rows[1].locked != locked {
					t.Fatal("credential/duration locks disagree with use decision")
				}
				m.dialog.form.choose("scope", "anywhere")
				answerApproval(t, m, delegate)
				if use == "deny" && delegate == "deny" {
					if len(ds.denials) != 1 || len(ds.approvals) != 0 {
						t.Fatal("both denials were not sent")
					}
					return
				}
				if len(ds.approvals) != 0 {
					t.Fatal("approval sent before replacement confirmation")
				}
				answerApproval(t, m, "accept")
				if len(ds.approvals) != 1 {
					t.Fatal("missing atomic answer")
				}
				got := ds.approvals[0]
				if (got.Use == console.PartApprove) != (use == "accept") || (got.Delegate == console.PartApprove) != (delegate == "accept") || got.UseScope != "hosts" || got.DelegateScope != "anywhere" {
					t.Fatalf("wrong answer: %#v", got)
				}
			})
		}
	}
}

func TestQueuedChoiceCannotApproveUnseenSummary(t *testing.T) {
	m, ds := sourceWithRequest(t)
	m.askAboutCredential(waitingRequest(), ds.projectSecrets, nil)
	d := m.dialog
	for {
		plainFrame(m)
		if !d.decisionsBlocked {
			break
		}
		d.update(keyPress("pgdown"))
	}
	d.form.choose("scope", "anywhere") // Input arrives before a new frame.
	d.formActionAt = 0
	if _, closed := d.update(keyPress("enter")); closed {
		t.Fatal("unseen scope accepted")
	}
	if len(ds.approvals) != 0 {
		t.Fatal("unseen scope sent to server")
	}
	plainFrame(m)
	if !d.decisionsBlocked || d.offset != 0 {
		t.Fatal("wider scope did not restart review")
	}
}

func TestCredentialDurationDefaultsLimitsAndCustomInput(t *testing.T) {
	for _, asked := range []time.Duration{0, time.Nanosecond, -time.Hour} {
		m, ds := sourceWithRequest(t)
		req := waitingRequest()
		req.AskedTTL = asked
		m.askAboutCredential(req, ds.projectSecrets, nil)
		if got := m.dialog.form.chosen("duration"); got != "3600" {
			t.Fatalf("ask=%v selected %s", asked, got)
		}
	}
	m, ds := sourceWithRequest(t)
	ds.projectSecrets[0].MaxTTL = time.Hour
	m.askAboutCredential(waitingRequest(), ds.projectSecrets, nil)
	m.dialog.form.choose("duration", "0")
	answerApproval(t, m, "accept")
	if len(ds.approvals) != 0 || m.dialog.form.err == "" {
		t.Fatal("credential ceiling ignored")
	}
	m.dialog.form.choose("duration", "custom")
	m.dialog.form.set("customDuration", "90m")
	answerApproval(t, m, "accept")
	if len(ds.approvals) != 0 || m.dialog.form.err == "" {
		t.Fatal("custom duration ignored ceiling")
	}
	m.dialog.form.set("customDuration", "45m")
	answerApproval(t, m, "accept")
	answerApproval(t, m, "accept")
	if len(ds.approvals) != 1 || ds.approvals[0].TTLSeconds != 2700 {
		t.Fatalf("custom approval=%#v", ds.approvals)
	}
}

func TestOpenApprovalPickersFitAndMouseCanReachLastChoice(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	send(t, m, sizeMsg(80, 24))
	var secrets []console.Secret
	for i := range 50 {
		secrets = append(secrets, console.Secret{ID: fmt.Sprintf("sec_%d", i), Name: fmt.Sprintf("Credential %d", i)})
	}
	m.askAboutCredential(waitingRequest(), secrets, nil)
	for _, row := range []int{0, 1, 3} {
		m.dialog.form.moveTo(row)
		m.dialog.form.openList()
		for _, position := range []string{"home", "end"} {
			m.dialog.update(keyPress(position))
			text := plainFrame(m)
			if strings.Count(text, "\n")+1 > 24 || !strings.Contains(text, "[ Accept ]") || !strings.Contains(text, "[ Deny ]") {
				t.Fatalf("picker row %d/%s overflows: %s", row, position, text)
			}
		}
		m.dialog.form.closeList()
	}
	m.dialog.form.moveTo(0)
	m.dialog.form.openList()
	m.dialog.form.listAt = 0
	plainFrame(m)
	for range 30 {
		x, y := at(t, m, "Credential 0")
		send(t, m, toScreen(m, tea.MouseWheelMsg{X: x, Y: y, Button: tea.MouseWheelDown}))
	}
	// Keyboard End also establishes the last choice and its mouse hit target.
	m.dialog.update(keyPress("end"))
	plainFrame(m)
	x, y := at(t, m, "New credential…")
	tap(t, m, x+2, y)
	if m.dialog.form.chosen("credential") != "new" || m.dialog.form.listing {
		t.Fatal("last windowed choice was not clickable")
	}
}

func TestRequestJSONPreservesIncompleteCustomDuration(t *testing.T) {
	for _, value := range []string{"", "90x", "1h?"} {
		m, ds := sourceWithRequest(t)
		req := waitingRequest()
		req.Raw = `{"request":"original"}`
		m.askAboutCredential(req, ds.projectSecrets, nil)
		original := m.dialog
		original.form.choose("duration", "custom")
		original.form.set("customDuration", value)
		answerApproval(t, m, "raw")
		send(t, m, keyPress("esc"))
		if m.dialog != original || m.dialog.form.chosen("duration") != "custom" || m.dialog.form.value("customDuration") != value {
			t.Fatalf("custom input changed after JSON: %q", value)
		}
	}
}

func TestRouteTableBoundsCartesianOutputAndPreservesCompactRoutes(t *testing.T) {
	hosts, paths := make([]string, 2000), make([]string, 2000)
	for i := range hosts {
		hosts[i] = fmt.Sprintf("host%d.example.com", i)
		paths[i] = fmt.Sprintf("/path%d", i)
	}
	route := secretroute.Route{Hosts: hosts, Paths: paths}
	if approvalTableFits([]secretroute.Route{route}) {
		t.Fatal("unbounded Cartesian product admitted")
	}
	sec := approvalRouteSection("Routes", &console.RequestPart{Routes: []secretroute.Route{route}})
	if len(sec.lines) != 2 || sec.lines[1].text != secretroute.Format(route) {
		t.Fatal("fallback changed route restrictions")
	}
	route = secretroute.Route{Hosts: hosts[:100], MCP: &secretroute.MCP{Tools: []string{strings.Repeat("tool", 10000)}}}
	if approvalTableFits([]secretroute.Route{route}) {
		t.Fatal("repeated protocol bytes exceeded output bound")
	}
}

func TestNewCredentialEditorScrollsFieldsOnCompactTerminal(t *testing.T) {
	m, ds := sourceWithRequest(t)
	send(t, m, sizeMsg(80, 24))
	m.askAboutCredential(waitingRequest(), ds.projectSecrets, nil)
	m.dialog.form.choose("credential", "new")
	answerApproval(t, m, "accept")
	if !onNewCredentialForm(m) {
		t.Fatal("missing new credential editor")
	}
	for _, key := range []string{"name", "token", "format"} {
		f := m.dialog.form
		for i, r := range f.rows {
			if r.key == key {
				f.moveTo(i)
				break
			}
		}
		text := plainFrame(m)
		if strings.Count(text, "\n")+1 > 24 || !strings.Contains(text, "enter stores and approves") {
			t.Fatalf("editor overflows at %s: %s", key, text)
		}
	}
	f := m.dialog.form
	for i, r := range f.rows {
		if r.key == "token" {
			f.moveTo(i)
		}
	}
	x, y := at(t, m, "the value itself")
	tap(t, m, x+1, y)
	send(t, m, typeString("dummy-new-635")...)
	send(t, m, keyPress("enter"))
	if len(ds.createdSecrets) != 1 || len(ds.approvals) != 1 {
		t.Fatalf("new credential was not saved: %d/%d", len(ds.createdSecrets), len(ds.approvals))
	}
}

func TestScrolledFormHoverMatchesVisibleMouseRegion(t *testing.T) {
	f := newForm(textRow("one", "One", "", "1"), textRow("two", "Two", "", "2"), textRow("three", "Three", "", "3"), textRow("four", "Four", "", "4"), textRow("five", "Five", "", "5"))
	f.rowRoom = 3
	f.moveTo(4)
	st := newStyles(false)
	// Use distinguishable plain marker styles so this checks the hovered row,
	// independently of a terminal's color-profile detection.
	st.hover = lipgloss.NewStyle().SetString("!")
	z := zones{hoverX: -1, hoverY: -1}
	f.view(st, &z, 0, 70)
	var target zone
	for _, mark := range z.marks {
		if mark.what.kind == hitFormRow && mark.what.idx == 3 {
			target = mark
		}
	}
	z.reset()
	z.hoverX, z.hoverY = 1, target.y
	text := f.view(st, &z, 0, 70)
	row := strings.Split(text, "\n")[target.y]
	if !strings.Contains(row, "!") || strings.Count(text, "!") != 1 {
		t.Fatalf("wrong hovered row: %s", text)
	}
}
