package tui

import (
	"fmt"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"

	tea "charm.land/bubbletea/v2"
)

// The trust inbox (ADR 0149): an agent asking for a host whose certificate the
// pool's egress refuses to be trusted for its discobox. It arrives in the same
// inbox as a credential request and is marked, bannered, and listed the same
// way; only the question differs. There is no secret to choose: the answer is
// which certificate from the chain the pool observed to pin, and for how long.

// trustAnswer is a trust approval being put together across the two steps.
type trustAnswer struct {
	req console.CredentialRequest
	pin console.TrustPin
	ttl time.Duration
}

// askAboutTrust is the trust card: who is asking to reach which host, why, what
// the pool was shown there, and one row per certificate that may be pinned —
// the server's default first.
func (m *Model) askAboutTrust(req console.CredentialRequest) tea.Cmd {
	q := console.TrustForm(req)
	f := newForm(q.Fields...)
	var d *dialog
	d = approvalDialog("Trust "+req.Host+"?", f, func(*form) tea.Cmd {
		if d.formActions[d.formActionAt].key == "deny" {
			return m.denyCredential(req)
		}
		trust, ok := answer(f, q)
		if !ok {
			return nil
		}
		return m.finishTrustApproval(trustAnswer{req: req, pin: trust.Pin, ttl: trust.TTL})
	})
	d.refreshSections = func() {
		trust, ok := console.ReadTrust(f.answers())
		if !ok {
			return
		}
		pin := trust.Pin
		var cert console.TrustCertificate
		if req.Trust.SuppliedCA != nil && req.Trust.SuppliedCA.SHA256 == pin.SHA256 {
			cert = *req.Trust.SuppliedCA
		} else {
			for _, c := range req.Trust.Chain {
				if c.SHA256 == pin.SHA256 || c.SPKISHA256 == pin.SHA256 {
					cert = c
					break
				}
			}
		}
		meaning := "Trust certificates verified by this CA for this host and box only."
		hash := "CA SHA-256"
		if pin.Kind == console.TrustPinLeafSPKI {
			meaning = "Trust this host's public key only. Key rotation requires new consent."
			hash = "Key SHA-256"
		}
		d.sections = []section{
			{fields: []field{{label: "Box", value: req.SandboxID, tone: toneAccent}, {label: "Host", value: req.Host, tone: toneAccent}, {label: "Reason (box's words)", value: req.Justification}}},
			{label: "Selected certificate", fields: []field{{label: "Subject", value: cert.Subject}, {label: "Issuer", value: cert.Issuer}, {label: "Valid", value: fmt.Sprintf("%s to %s", cert.NotBefore.Format(time.DateOnly), cert.NotAfter.Format(time.DateOnly))}, {label: hash, value: pin.SHA256, tone: toneAccent}}, lines: []line{{text: meaning}}},
			{label: "What accepting changes", lines: []line{{text: "This box may connect to " + req.Host + " using this trust for " + f.chosenLabel("duration") + "."}, {text: "Trust always expires. Deny leaves this host refused; other hosts and boxes are unaffected."}}},
		}
		if req.Server != "" {
			d.sections[0].fields = append(d.sections[0].fields, field{label: "Server", value: req.Server})
		}
	}
	d.refreshSections()
	m.dialog = d
	return nil
}

func (m *Model) finishTrustApproval(a trustAnswer) tea.Cmd {
	m.dialog = statusDialog("Trust request", "trusting "+a.req.Host+" for "+lifetime.Label(a.ttl)+"…")
	return func() tea.Msg {
		err := m.ds.ApproveTrustRequest(m.ctx, a.req.Server, console.TrustApproval{
			RequestID:  a.req.ID,
			Pin:        a.pin,
			TTLSeconds: lifetime.Seconds(a.ttl),
		})
		return credentialAnsweredMsg{request: a.req, approved: true, ttl: a.ttl, err: err}
	}
}
