package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
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
			case "use", "delegate":
				if kind == "delegate" {
					req.Delegate, req.Use = req.Use, nil
				}
				m.askAboutCredential(req, []Secret{{ID: "sec", Name: "GitHub", Routes: req.routes()}}, nil)
				pickItem(t, m, "sec")
			case "adoption":
				req.Adoption = &AdoptionAsk{ChildID: "child", RequesterID: "requester", CurrentParentID: "parent"}
				m.askAboutAdoption(req)
			case "trust":
				m.askAboutTrust(waitingTrust())
			case "refresh":
				m.askAboutRefresh(refreshRequest(), []Secret{{ID: "sec_gh", Name: "GitHub", RefreshCommand: []string{"gh", "auth", "token"}}})
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
	sec := approvalRouteSection("Routes", &RequestPart{Routes: []secretroute.Route{route}})
	if len(sec.lines) != 2 || sec.lines[1].text != secretroute.Format(route) {
		t.Fatal("fallback changed route restrictions")
	}
	route = secretroute.Route{Hosts: hosts[:100], MCP: &secretroute.MCP{Tools: []string{strings.Repeat("tool", 10000)}}}
	if approvalTableFits([]secretroute.Route{route}) {
		t.Fatal("repeated protocol bytes exceeded output bound")
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

// Step 1 with more secrets than the window has rows still fits it, and the
// pointer reaches the last row, a new credential.
func TestStepOneFitsACompactWindowAndTheMouseReachesItsLastRow(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	send(t, m, sizeMsg(80, 24))
	var secrets []Secret
	for i := range 50 {
		secrets = append(secrets, Secret{ID: fmt.Sprintf("sec_%d", i), Name: fmt.Sprintf("Credential %d", i)})
	}
	m.askAboutCredential(waitingRequest(), secrets, nil)
	text := plainFrame(m)
	if strings.Count(text, "\n")+1 > 24 {
		t.Fatalf("step 1 overflows: %s", text)
	}
	m.dialog.update(keyPress("end"))
	plainFrame(m)
	x, y := at(t, m, "Enter new credential…")
	tap(t, m, x+2, y)
	if !onStep(m, "new credential") {
		t.Fatalf("dialog = %s, want the new credential", describe(m.dialog))
	}
}

// The new credential's form fits a compact window, and Enter on its value
// goes on to the summary.
func TestTheNewCredentialFormFitsACompactWindow(t *testing.T) {
	m, ds := sourceWithRequest(t)
	send(t, m, sizeMsg(80, 24))
	m.askAboutCredential(waitingRequest(), ds.projectSecrets, nil)
	pickItem(t, m, "new")
	text := plainFrame(m)
	if strings.Count(text, "\n")+1 > 24 || !strings.Contains(text, "Sentinel") || !strings.Contains(text, "Enter continues") {
		t.Fatalf("form overflows: %s", text)
	}
	send(t, m, typeString("dummy-new-635")...)
	send(t, m, keyPress("enter"))
	if !onStep(m, "summary") {
		t.Fatalf("dialog = %s, want the summary", describe(m.dialog))
	}
}
