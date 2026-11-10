package console

import (
	"testing"
	"time"
)

// One notice shows at a time: a refusal over the request it provoked, and a
// harness change last.
func TestANoticeIsTheFirstWantedAndNotDismissed(t *testing.T) {
	t.Parallel()
	box := Sandbox{ID: "b1", HarnessID: "h1", HarnessConfigPending: true, HarnessConfigCandidate: "c1"}
	in := Inbox{
		Requests:   []CredentialRequest{{ID: "r1", SandboxID: "b1", Name: "github"}},
		Rejections: []SecretRejection{{SecretID: "s1", HarnessConfigID: "h1", HarnessConfigName: "Claude", Host: "api.anthropic.com", FirstSeen: time.Unix(1, 0)}},
	}
	var d Dismissals
	n := d.NoticeFor(in, box)
	if n.Kind != NoticeRejected || n.Subject != "Claude sign-in refused" || n.Behind != "1 request waiting behind it" || n.Command != "rejections" {
		t.Fatalf("notice %+v, want the refusal with the request behind it", n)
	}
	for _, want := range []NoticeKind{NoticeRequest, NoticeHarnessChanges, NoNotice} {
		d.Dismiss(in, box)
		if got := d.Showing(in, box); got != want {
			t.Fatalf("after a dismissal: %q, want %q", got, want)
		}
	}
	if d.Dismiss(in, box) != NoNotice {
		t.Fatal("dismissed with nothing showing")
	}
}

// A dismissal covers what it saw: something new brings the notice back, and
// what it covered is forgotten once it is over.
func TestADismissalCoversWhatItSaw(t *testing.T) {
	t.Parallel()
	box := Sandbox{ID: "b1"}
	in := Inbox{Requests: []CredentialRequest{{ID: "r1", SandboxID: "b1"}}}
	var d Dismissals
	d.Dismiss(in, box)
	if d.Showing(in, box) != NoNotice {
		t.Fatal("a dismissed request still showing")
	}
	more := in
	more.Requests = append(more.Requests, CredentialRequest{ID: "r2", SandboxID: "b1"})
	if d.Showing(more, box) != NoticeRequest {
		t.Fatal("a new request did not bring the notice back")
	}
	gone := Inbox{Requests: []CredentialRequest{{ID: "r2", SandboxID: "b1"}}}
	d.Prune(gone, []Sandbox{box})
	if len(d) != 0 {
		t.Fatalf("dismissals %v, want r1's forgotten once it is over", d)
	}
	// A box the listing does not hold keeps its dismissal: its server has
	// settled nothing.
	d.Dismiss(in, box)
	d.Prune(Inbox{}, nil)
	if len(d) != 1 {
		t.Fatalf("dismissals %v, want kept for a box not listed", d)
	}
}

// A refusal's notice is about the oldest refusal not dismissed.
func TestTheShownRejectionIsTheOldestNotDismissed(t *testing.T) {
	t.Parallel()
	box := Sandbox{ID: "b1"}
	older := SecretRejection{SecretID: "s1", SandboxID: "b1", EnvName: "GH_TOKEN", FirstSeen: time.Unix(1, 0)}
	newer := SecretRejection{SecretID: "s2", SandboxID: "b1", EnvName: "NPM_TOKEN", FirstSeen: time.Unix(2, 0)}
	rejections := []SecretRejection{newer, older}
	SortRejections(rejections)
	in := Inbox{Rejections: rejections[:1]}
	var d Dismissals
	d.Dismiss(in, box)
	in.Rejections = rejections
	if got, _ := d.ShownRejection(in, box); got.SecretID != "s2" {
		t.Fatalf("shown %q, want the one not dismissed", got.SecretID)
	}
	if n := d.NoticeFor(in, box); n.Subject != "NPM_TOKEN refused" || n.Call != "click to replace it on this machine" {
		t.Fatalf("notice %+v", n)
	}
}

// What a request's notice and row say depends on what it asks.
func TestARequestsWords(t *testing.T) {
	t.Parallel()
	cred := CredentialRequest{ID: "r1", SandboxID: "b1", Name: "github", Use: &RequestPart{}}
	trust := CredentialRequest{ID: "t1", SandboxID: "b1", Host: "git.corp", Trust: &TrustAsk{}}
	if what, subject := RequestNotice([]CredentialRequest{trust}); what != "trust request" || subject != "trust of git.corp" {
		t.Fatalf("trust notice %q %q", what, subject)
	}
	if what, subject := RequestNotice([]CredentialRequest{cred, trust}); what != "2 requests" || subject != "oldest: github" {
		t.Fatalf("mixed notice %q %q", what, subject)
	}
	if title, body := RequestArrival(cred, Sandbox{ID: "b1", GivenName: "feature"}); title != "Credential request" || body != "feature · github" {
		t.Fatalf("arrival %q %q", title, body)
	}
	if title, body := RequestArrival(CredentialRequest{ID: "f1", Name: "npm", Refresh: &RefreshAsk{}}, Sandbox{}); title != "Refresh request" || body != "control plane · npm" {
		t.Fatalf("arrival of a refresh %q %q", title, body)
	}
	if got := Answered(cred, true, 24*time.Hour); got != "approved github for 1 day" {
		t.Fatalf("answered %q", got)
	}
}

// A permission covers its token's exact command on its server, and nothing
// else.
func TestARenewalPermitIsForOneCommand(t *testing.T) {
	t.Parallel()
	var p RenewalPermits
	p.Allow("home", "sec_npm", []string{"npm", "token"})
	if !p.Permitted("home", "sec_npm", []string{"npm", "token"}) || p.Permitted("home", "sec_npm", []string{"npm", "token", "--other"}) || p.Permitted("away", "sec_npm", []string{"npm", "token"}) {
		t.Fatal("a permission reached past its token and command")
	}
	refresh := CredentialRequest{ID: "f1", Refresh: &RefreshAsk{SecretID: "sec_npm"}}
	if got := RenewableByPermit(Session{}, p, []CredentialRequest{refresh}, map[string]bool{"f1": true}); len(got) != 0 {
		t.Fatal("a request with a renewal out was renewed again")
	}
	p.Revoke("home", "sec_npm", []string{"npm", "token"})
	if p.Covers("home", "sec_npm") {
		t.Fatal("a revoked permission still covers its token")
	}
}
