package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"
)

// A cluster's API server asking to be trusted: a leaf signed by the cluster's
// own self-signed CA, as GKE and kubeadm both present.
func waitingTrust() console.CredentialRequest {
	ca := console.TrustCertificate{Subject: "CN=cluster-ca", Issuer: "CN=cluster-ca", SHA256: strings.Repeat("c", 64), SPKISHA256: strings.Repeat("d", 64), IsCA: true, SelfSigned: true,
		NotBefore: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)}
	leaf := console.TrustCertificate{Subject: "CN=kube-apiserver", Issuer: "CN=cluster-ca", Names: []string{"34.70.64.109"}, SHA256: strings.Repeat("a", 64), SPKISHA256: strings.Repeat("b", 64),
		NotBefore: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(2027, 9, 1, 0, 0, 0, 0, time.UTC)}
	return console.CredentialRequest{
		ID:            "treq_1",
		SandboxID:     "sbx_one",
		Host:          "34.70.64.109:443",
		Justification: "the user's GKE cluster uses its own CA",
		Created:       time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC),
		Trust: &console.TrustAsk{
			Chain:      []console.TrustCertificate{leaf, ca},
			DefaultPin: console.TrustPin{Kind: console.TrustPinCA, SHA256: ca.SHA256},
		},
	}
}

func sourceWithTrust(t *testing.T) (*Model, *fakeSource) {
	t.Helper()
	ds := newFakeSource(testSandboxes()...)
	ds.requests = []console.CredentialRequest{waitingTrust()}
	return newTestModel(t, ds), ds
}

// A trust request is marked and answered from the same key as a credential
// request, and its card says what the pool was shown and what it will be used
// for — the two things a pin is decided on.
func TestATrustRequestAsksWhichCertificateToPin(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithTrust(t)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	if m.dialog == nil || !strings.HasPrefix(m.dialog.title, "Trust ") {
		t.Fatalf("dialog = %s, want the trust card", describe(m.dialog))
	}
	body := dialogText(m)
	for _, want := range []string{"34.70.64.109:443", "host and box only", "its own CA", "CN=cluster-ca"} {
		if !strings.Contains(body, want) {
			t.Fatalf("card = %q, want it to carry %q", body, want)
		}
	}
	// The server's default is offered first, and the key pin last: it breaks
	// the day the host rotates its key.
	if len(m.dialog.form.rows[0].choices) != 2 {
		t.Fatal("expected CA and key choices")
	}

}

func TestApprovingATrustPinsForTheChosenLifetime(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithTrust(t)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	for _, c := range m.dialog.form.rows[1].choices {
		if c.key == "0" {
			t.Fatal("forever is offered for a trust")
		}
	}
	m.dialog.form.choose("duration", "86400")
	answerApproval(t, m, "accept")

	if len(ds.trustApprovals) != 1 {
		t.Fatalf("trust approvals = %#v", ds.trustApprovals)
	}
	got := ds.trustApprovals[0]
	if got.RequestID != "treq_1" || got.Pin.Kind != console.TrustPinCA || got.Pin.SHA256 != strings.Repeat("c", 64) || got.TTLSeconds != 86400 {
		t.Fatalf("approval = %+v", got)
	}
	if len(ds.approvals) != 0 {
		t.Fatal("a trust was approved as a credential")
	}
	if !strings.Contains(strings.Join(frame(m), "\n"), "approved trust of 34.70.64.109:443 for 1 day") {
		t.Fatalf("the window did not say what it trusted:\n%s", strings.Join(frame(m), "\n"))
	}
}

func TestDenyingATrustAnswersTheTrustRequest(t *testing.T) {
	t.Parallel()
	m, ds := sourceWithTrust(t)

	send(t, m, keyPress("tab"), keyPress(credentialsKey))
	answerApproval(t, m, "deny")

	if len(ds.trustDenials) != 1 || ds.trustDenials[0] != "treq_1" || len(ds.denials) != 0 {
		t.Fatalf("trust denials %#v, credential denials %#v", ds.trustDenials, ds.denials)
	}
}

func TestTheBannerNamesATrustRequest(t *testing.T) {
	t.Parallel()
	m, _ := sourceWithTrust(t)
	m.Box = console.Sandbox{ID: "sbx_one", Name: "one"}
	banner := m.viewCredentialBanner(120)
	if !strings.Contains(banner, "trust request") || !strings.Contains(banner, "34.70.64.109:443") {
		t.Fatalf("banner = %q", banner)
	}
}

// The supplied CA is offered first when the agent gave one, since it is what
// the server pins by default.
func TestASuppliedCAIsOfferedFirst(t *testing.T) {
	t.Parallel()
	req := waitingTrust()
	supplied := console.TrustCertificate{Subject: "CN=gke-ca", SHA256: strings.Repeat("f", 64), IsCA: true}
	req.Trust.SuppliedCA = &supplied
	req.Trust.DefaultPin = console.TrustPin{Kind: console.TrustPinCA, SHA256: supplied.SHA256}
	options := trustPinOptions(*req.Trust)
	if options[0].pin.SHA256 != supplied.SHA256 || len(options) != 3 {
		t.Fatalf("options = %+v", options)
	}
}
