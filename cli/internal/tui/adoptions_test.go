package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/discobox-ai/discobox/cli/internal/console"
)

func TestAdoptionCardNamesAuthority(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	m := newTestModel(t, ds)
	req := console.CredentialRequest{ID: "adopt_1", Justification: "lead the feature", Adoption: &console.AdoptionAsk{ChildID: "child", ChildName: "feature", RequesterID: "lead", CurrentParentID: "old-lead"}}
	m.openCredentialRequest(req)
	text := dialogText(m)
	for _, want := range []string{"child", "lead", "old-lead", "lead the feature", "terminals", "deliver its source", "credentials stay its own"} {
		if !strings.Contains(text, want) {
			t.Fatalf("card missing %q: %s", want, text)
		}
	}
}

// Adoption is an approval request, but it never asks for a credential.
func TestAdoptionInboxAndBannerNameTheChild(t *testing.T) {
	req := console.CredentialRequest{ID: "adopt_1", SandboxID: "sbx_one", Adoption: &console.AdoptionAsk{ChildID: "sbx_child", ChildName: "feature"}}
	if got := requestSubject(req); got != "adoption of feature" {
		t.Fatalf("inbox subject = %q", got)
	}
	m := newTestModel(t, newFakeSource(testSandboxes()...))
	m.Box = testSandboxes()[0]
	m.requests = map[string][]console.CredentialRequest{"sbx_one": {req}}
	banner := ansi.Strip(m.viewCredentialBanner(150))
	if !strings.Contains(banner, "adoption request") || !strings.Contains(banner, "feature") || strings.Contains(banner, "credential request") {
		t.Fatalf("banner = %s", banner)
	}
	for _, tc := range []struct {
		requests []console.CredentialRequest
		want     string
	}{
		{[]console.CredentialRequest{req, req}, "adoption requests"},
		{[]console.CredentialRequest{req, waitingRequest()}, "2 requests"},
	} {
		m.requests["sbx_one"] = tc.requests
		if got := ansi.Strip(m.viewCredentialBanner(150)); !strings.Contains(got, tc.want) {
			t.Fatalf("banner = %s, want %s", got, tc.want)
		}
	}
	req.Adoption.ChildName = ""
	if got := requestSubject(req); got != "adoption of sbx_child" {
		t.Fatalf("unnamed child = %q", got)
	}
}

func TestReadOnlyAdoptionOffersOnlyReadAuthority(t *testing.T) {
	m := newTestModel(t, newFakeSource())
	m.openCredentialRequest(console.CredentialRequest{Adoption: &console.AdoptionAsk{ChildID: "child", ReadOnly: true}})
	card := dialogText(m)
	if strings.Contains(card, "start, stop") || strings.Contains(card, "deliver its source") {
		t.Fatalf("read-only approval promises writes: %s", card)
	}
	for _, want := range []string{"only read", "wider egress", "credentials stay its own", "Accept", "Deny"} {
		if !strings.Contains(card, want) {
			t.Fatalf("card missing %q: %s", want, card)
		}
	}
}

func TestAdoptionDecisionFromInbox(t *testing.T) {
	for _, tc := range []struct {
		key     string
		approve bool
	}{{"enter", true}, {"d", false}} {
		t.Run(tc.key, func(t *testing.T) {
			ds := newFakeSource(testSandboxes()...)
			req := console.CredentialRequest{ID: "adopt_1", Server: "alpha", SandboxID: "sbx_one", Adoption: &console.AdoptionAsk{ChildID: "child", ChildName: "feature"}}
			ds.requests = []console.CredentialRequest{req}
			m := newTestModel(t, ds)
			drain(t, m, m.openCredentialRequest(req), 0)
			decision := "accept"
			if !tc.approve {
				decision = "deny"
			}
			answerApproval(t, m, decision)
			if got := fmt.Sprint(ds.adoptionDecisions); got != fmt.Sprintf("[adopt_1=%t]", tc.approve) {
				t.Fatalf("decisions = %s", got)
			}
			if !slices.Contains(ds.onServer, "DecideAdoptionRequest@alpha") {
				t.Fatalf("server calls = %v", ds.onServer)
			}
			if m.dialog != nil || len(ds.requests) != 0 {
				t.Fatal("answered adoption remains open")
			}
		})
	}
}

func TestFailedAdoptionStaysInInboxAndSaysWhatFailed(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	req := console.CredentialRequest{ID: "adopt_1", SandboxID: "sbx_one", Adoption: &console.AdoptionAsk{ChildID: "child"}}
	ds.requests = []console.CredentialRequest{req}
	ds.approveErr = errors.New("child is archived; restore it before adopting")
	m := newTestModel(t, ds)
	drain(t, m, m.openCredentialRequest(req), 0)
	answerApproval(t, m, "accept")
	if len(ds.requests) != 1 {
		t.Fatal("failed adoption disappeared")
	}
	card := dialogText(m)
	if !strings.Contains(card, "child is archived") || !strings.Contains(card, "adoption") || strings.Contains(card, "credential") {
		t.Fatalf("error card = %s", card)
	}
}

func TestAdoptionEscapeLeavesTheRequestWaiting(t *testing.T) {
	ds := newFakeSource(testSandboxes()...)
	req := console.CredentialRequest{ID: "adopt_1", SandboxID: "sbx_one", Adoption: &console.AdoptionAsk{ChildID: "child"}}
	ds.requests = []console.CredentialRequest{req}
	m := newTestModel(t, ds)
	drain(t, m, m.openCredentialRequest(req), 0)
	send(t, m, keyPress("esc"))
	if m.dialog != nil || len(ds.adoptionDecisions) != 0 || len(ds.requests) != 1 {
		t.Fatal("closing the card answered or removed the request")
	}
}

func TestAdoptionCardKeepsActionsWhileReadingOnASmallTerminal(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		m := newTestModel(t, newFakeSource())
		send(t, m, sizeMsg(80, 24))
		m.openCredentialRequest(console.CredentialRequest{Justification: strings.Repeat("review the feature ", 20), Adoption: &console.AdoptionAsk{ChildID: "child", RequesterID: "lead", ReadOnly: readOnly}})
		var seen strings.Builder
		for range 12 {
			card := dialogText(m)
			for _, want := range []string{"Accept", "Deny", "Esc"} {
				if !strings.Contains(card, want) {
					t.Fatalf("missing %q at 80x24:\n%s", want, card)
				}
			}
			seen.WriteString(card)
			m.dialog.update(keyPress("pgdown"))
		}
		want := "deliver its source"
		if readOnly {
			want = "only read"
		}
		if !strings.Contains(seen.String(), want) {
			t.Fatalf("authority never readable: %s", seen.String())
		}
	}
}
