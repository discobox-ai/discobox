//go:build prototype

package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Fixtures exercise the real certificate pin choices without contacting a host.
func (p *approvalPreview) openTrust() {
	now := time.Now()
	ca := TrustCertificate{Subject: "CN=cluster-ca", Issuer: "CN=cluster-ca", IsCA: true, SelfSigned: true, SHA256: strings.Repeat("ca", 32), NotBefore: now.AddDate(0, -1, 0), NotAfter: now.AddDate(1, 0, 0)}
	leaf := TrustCertificate{Subject: "CN=kube-apiserver", Issuer: ca.Subject, Names: []string{"cluster.internal"}, SHA256: strings.Repeat("ab", 32), SPKISHA256: strings.Repeat("12", 32), NotBefore: now.AddDate(0, -1, 0), NotAfter: now.AddDate(0, 3, 0)}
	req := CredentialRequest{ID: "trust_sample", SandboxID: "sbx_worker", Host: "cluster.internal:443", Justification: "Deploy to the development cluster, which uses its own certificate authority.", Created: now.Add(-2 * time.Minute), AskedTTL: time.Hour, Trust: &TrustAsk{Chain: []TrustCertificate{leaf, ca}, DefaultPin: TrustPin{Kind: TrustPinCA, SHA256: ca.SHA256}}}
	if p.readOnly {
		supplied := ca
		supplied.Subject = "CN=agent-supplied-cluster-ca"
		supplied.Issuer = supplied.Subject
		supplied.SHA256 = strings.Repeat("de", 32)
		req.Trust.SuppliedCA = &supplied
		req.Trust.DefaultPin = TrustPin{Kind: TrustPinCA, SHA256: supplied.SHA256}
	}
	if p.current {
		m := &Model{secrets: newSecretList()}
		m.askAboutTrust(req)
		p.d = m.dialog
		p.d.action = func(result string) tea.Cmd { return p.receipt("Trust · " + result) }
		return
	}
	options := trustPinOptions(*req.Trust)
	choices := []choice{}
	for i, opt := range options {
		label := "Cluster CA"
		if req.Trust.SuppliedCA != nil && opt.pin.SHA256 == req.Trust.SuppliedCA.SHA256 {
			label = "Agent-supplied CA"
		}
		hint := "Accept certificates verified by this CA, for this host and box only."
		if opt.pin.Kind == TrustPinLeafSPKI {
			label = "Host public key"
			hint = "Trust this key only. Stops working when the host rotates its key."
		}
		choices = append(choices, choice{key: strconv.Itoa(i), label: label, hint: hint})
	}
	f := newForm(pickRow("pin", "Trust by", choices...), pickRow("duration", "Duration", choice{key: "1h", label: "1 hour", hint: "Requested by the box"}, choice{key: "1d", label: "1 day"}, choice{key: "1w", label: "1 week"}, choice{key: "1mo", label: "1 month"}))
	var d *dialog
	d = formDialog("Trust cluster.internal:443?", f, func(f *form) tea.Cmd {
		answer := d.formActions[d.formActionAt].key
		if answer == "deny" {
			return p.receipt("Host trust · denied")
		}
		return p.receipt("Host trust · accepted · " + f.chosenLabel("pin") + " · " + f.chosenLabel("duration"))
	})
	d.titleRight = "2m ago"
	d.sections = []section{
		{fields: []field{{label: "Box", value: "Deploy worker · sbx_worker", tone: toneAccent}, {label: "Host", value: req.Host, tone: toneAccent}, {label: "Reason", value: "“" + req.Justification + "”"}}, lines: []line{{text: "The host's certificate is currently refused. Accepting trusts a certificate for this host and box only."}}},
		{label: "Selected certificate"},
		{label: "What accepting changes", lines: []line{{text: "This box may connect to cluster.internal:443 using the selected trust.", tone: toneAccent}, {text: "Other hosts and boxes are unaffected. Trust expires; the host is then refused again."}}},
	}
	updateCertificate := func() {
		if p.d != d {
			return
		}
		index, _ := strconv.Atoi(f.chosen("pin"))
		pin := options[index].pin
		cert := ca
		if req.Trust.SuppliedCA != nil && pin.SHA256 == req.Trust.SuppliedCA.SHA256 {
			cert = *req.Trust.SuppliedCA
		}
		hashLabel := "CA SHA-256"
		if pin.Kind == TrustPinLeafSPKI {
			cert = leaf
			hashLabel = "Key SHA-256"
		}
		d.sections[1] = section{label: "Selected certificate · " + f.chosenLabel("pin"), fields: []field{
			{label: "Subject", value: cert.Subject},
			{label: "Issuer", value: cert.Issuer},
			{label: "Valid", value: fmt.Sprintf("%s to %s", cert.NotBefore.Format(time.DateOnly), cert.NotAfter.Format(time.DateOnly))},
			{label: hashLabel, value: pin.SHA256, tone: toneAccent},
		}}
	}
	d.formActions = []action{{key: "accept", label: "Accept", enabled: true}, {key: "deny", label: "Deny", enabled: true}}
	d.formActionAt = -1
	d.keys = []hint{says("Tab / ↑↓ move"), says("←→ change"), pressing("enter selects", "enter"), pressing("esc leaves waiting", "esc")}
	d.footer = "Sample only · Deny leaves the host refused · trust always expires"
	p.d = d
	p.refreshGrant = updateCertificate
	updateCertificate()
}
