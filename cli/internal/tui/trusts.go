package tui

import (
	"fmt"
	"strconv"
	"time"

	"github.com/discobox-ai/discobox/cli/internal/lifetime"

	tea "charm.land/bubbletea/v2"
)

// The trust inbox (ADR 0149): an agent asking for a host whose certificate the
// pool's egress refuses to be trusted for its discobox. It arrives in the same
// inbox as a credential request and is marked, bannered, and listed the same
// way; only the question differs. There is no secret to choose: the answer is
// which certificate from the chain the pool observed to pin, and for how long.

// trustLifetimes are the lifetimes a trust is offered for. A trust always
// lapses, so the presets stop at a month, which is also the most the server
// allows.
var trustLifetimes = []time.Duration{time.Hour, lifetime.Day, lifetime.Week, lifetime.Month}

// trustAnswer is a trust approval being put together across the two steps.
type trustAnswer struct {
	req CredentialRequest
	pin TrustPin
	ttl time.Duration
}

// askAboutTrust is the trust card: who is asking to reach which host, why, what
// the pool was shown there, and one row per certificate that may be pinned —
// the server's default first.
func (m *Model) askAboutTrust(req CredentialRequest) tea.Cmd {
	options := trustPinOptions(*req.Trust)
	var pins []choice
	for i, opt := range options {
		pins = append(pins, choice{key: strconv.Itoa(i), label: opt.label, hint: opt.detail})
	}
	var durations []choice
	offered := false
	for _, ttl := range trustLifetimes {
		if ttl == req.AskedTTL {
			offered = true
		}
		durations = append(durations, choice{key: strconv.FormatInt(lifetime.Seconds(ttl), 10), label: lifetime.Label(ttl)})
	}
	if !offered && req.AskedTTL > 0 && req.AskedTTL <= lifetime.Month && req.AskedTTL%time.Second == 0 {
		durations = append(durations, choice{key: strconv.FormatInt(lifetime.Seconds(req.AskedTTL), 10), label: lifetime.Label(req.AskedTTL)})
	}
	f := newForm(pickRow("pin", "Trust by", pins...), pickRow("duration", "Duration", durations...))
	if req.AskedTTL > 0 {
		f.choose("duration", strconv.FormatInt(lifetime.Seconds(req.AskedTTL), 10))
	}
	var d *dialog
	d = approvalDialog("Trust "+req.Host+"?", f, func(*form) tea.Cmd {
		if d.formActions[d.formActionAt].key == "deny" {
			return m.denyCredential(req)
		}
		index, err := strconv.Atoi(f.chosen("pin"))
		if err != nil || index < 0 || index >= len(options) {
			f.err = "Choose a certificate to trust."
			return nil
		}
		seconds, _ := strconv.ParseInt(f.chosen("duration"), 10, 64)
		return m.finishTrustApproval(trustAnswer{req: req, pin: options[index].pin, ttl: time.Duration(seconds) * time.Second})
	})
	d.refreshSections = func() {
		index, _ := strconv.Atoi(f.chosen("pin"))
		if index < 0 || index >= len(options) {
			return
		}
		pin := options[index].pin
		var cert TrustCertificate
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
		if pin.Kind == TrustPinLeafSPKI {
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
		err := m.ds.ApproveTrustRequest(m.ctx, a.req.Server, TrustApproval{
			RequestID:  a.req.ID,
			Pin:        a.pin,
			TTLSeconds: lifetime.Seconds(a.ttl),
		})
		return credentialAnsweredMsg{request: a.req, approved: true, ttl: a.ttl, err: err}
	}
}

// trustPinOption is one certificate the card offers to pin.
type trustPinOption struct {
	pin    TrustPin
	label  string
	detail string
}

// trustPinOptions is every pin the request offers, the server's default first:
// the CA the agent supplied, each CA in the chain, and the leaf's own key last,
// since a key pin breaks the day the host rotates its key.
func trustPinOptions(ask TrustAsk) []trustPinOption {
	var options []trustPinOption
	seen := map[string]bool{}
	add := func(option trustPinOption) {
		key := option.pin.Kind + ":" + option.pin.SHA256
		if seen[key] {
			return
		}
		seen[key] = true
		options = append(options, option)
	}
	if ask.SuppliedCA != nil {
		add(trustPinOption{
			pin:    TrustPin{Kind: TrustPinCA, SHA256: ask.SuppliedCA.SHA256},
			label:  "Trust by the CA the agent supplied",
			detail: ask.SuppliedCA.Subject + " · " + shortFingerprint(ask.SuppliedCA.SHA256),
		})
	}
	for i := len(ask.Chain) - 1; i >= 0; i-- {
		cert := ask.Chain[i]
		if !cert.IsCA {
			continue
		}
		add(trustPinOption{
			pin:    TrustPin{Kind: TrustPinCA, SHA256: cert.SHA256},
			label:  "Trust by its CA",
			detail: cert.Subject + " · " + shortFingerprint(cert.SHA256),
		})
	}
	if len(ask.Chain) > 0 {
		leaf := ask.Chain[0]
		add(trustPinOption{
			pin:    TrustPin{Kind: TrustPinLeafSPKI, SHA256: leaf.SPKISHA256},
			label:  "Trust by its key",
			detail: "breaks when the host rotates its key · " + shortFingerprint(leaf.SPKISHA256),
		})
	}
	for i, option := range options {
		if option.pin == ask.DefaultPin {
			options[0], options[i] = options[i], options[0]
			options[0].detail = "what an approval takes by default · " + options[0].detail
			break
		}
	}
	return options
}

// shortFingerprint is a hash at a width a person compares by eye: the head and
// tail, which is what they check against the one they were told.
func shortFingerprint(hash string) string {
	if len(hash) <= 16 {
		return hash
	}
	return hash[:8] + "…" + hash[len(hash)-8:]
}
