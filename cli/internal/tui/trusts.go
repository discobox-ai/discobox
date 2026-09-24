package tui

import (
	"fmt"
	"strconv"
	"strings"
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
	items := make([]action, 0, len(options)+1)
	for _, option := range options {
		items = append(items, action{
			key:     "pin:" + option.pin.Kind + ":" + option.pin.SHA256,
			label:   option.label,
			detail:  option.detail,
			enabled: true,
		})
	}
	items = append(items, action{key: "deny", press: "d", label: "Deny", detail: "answer no; the agent is waiting on one", enabled: true})

	d := actionsDialog("Trust request", "", items, func(result string) tea.Cmd {
		if result == "deny" {
			return m.denyCredential(req)
		}
		kind, sha, ok := strings.Cut(strings.TrimPrefix(result, "pin:"), ":")
		if !ok {
			return nil
		}
		return m.askTrustLifetime(trustAnswer{req: req, pin: TrustPin{Kind: kind, SHA256: sha}}, func() tea.Cmd { return m.askAboutTrust(req) })
	})
	d.sections = trustAskSections(req, m.secrets.now())
	d.answerLabel = "trust " + req.Host + " by which certificate?"
	d.footer = "every request this discobox then sends " + req.Host + " is judged against the uses above"
	d.keys = []hint{pressing("enter chooses the highlighted pin", "enter"), pressing("esc leaves it waiting", "esc")}
	m.dialog = d
	return nil
}

// askTrustLifetime is the second step: how long the trust lasts.
func (m *Model) askTrustLifetime(a trustAnswer, back func() tea.Cmd) tea.Cmd {
	asked := a.req.GrantTTL
	items := make([]action, 0, len(trustLifetimes)+1)
	offered := false
	for _, d := range trustLifetimes {
		detail := ""
		if d == asked {
			detail, offered = "what the agent asked for", true
		}
		items = append(items, action{key: strconv.FormatInt(lifetime.Seconds(d), 10), label: lifetime.Label(d), detail: detail, enabled: true})
	}
	if asked > 0 && asked <= lifetime.Month && asked%time.Second == 0 && !offered {
		items = append([]action{{key: strconv.FormatInt(lifetime.Seconds(asked), 10), label: lifetime.Label(asked), detail: "what the agent asked for", enabled: true}}, items...)
	}
	d := actionsDialog("How long?", "", items, func(result string) tea.Cmd {
		seconds, err := strconv.ParseInt(result, 10, 64)
		if err != nil {
			return nil
		}
		next := a
		next.ttl = time.Duration(seconds) * time.Second
		return m.finishTrustApproval(next)
	})
	opens := strconv.FormatInt(lifetime.Seconds(lifetime.Default), 10)
	if asked > 0 && asked <= lifetime.Month {
		opens = strconv.FormatInt(lifetime.Seconds(asked), 10)
	}
	for i, item := range items {
		if item.key == opens {
			d.cursor = i
			break
		}
	}
	d.sections = []section{{label: "the trust", fields: []field{
		{label: "host", value: a.req.Host, tone: toneAccent},
		{label: "pinned by", value: pinLabel(a.pin)},
		{label: "for", value: "this discobox alone"},
	}}}
	d.answerLabel = "how long may this discobox trust " + a.req.Host + "?"
	d.footer = "when it lapses the host is refused again, and the agent asks again"
	d.keys = []hint{pressing("enter chooses the highlighted lifetime", "enter"), pressing("esc goes back", "esc")}
	d.onCancel = back
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

// trustAskSections is the trust request as a person needs to read it.
func trustAskSections(req CredentialRequest, now time.Time) []section {
	asked := "the agent in this discobox"
	if when := ago(req.Created, now); when != "" {
		asked = when + " by " + asked
	}
	fields := []field{
		{label: "trust", value: req.Host, tone: toneAccent},
		{label: "for", value: "this discobox alone"},
	}
	if req.GrantTTL > 0 {
		fields = append(fields, field{label: "wanted for", value: lifetime.Label(req.GrantTTL)})
	}
	fields = append(fields, field{label: "asked", value: asked, tone: toneDim})
	if req.Server != "" {
		fields = append(fields, field{label: "on server", value: req.Server, tone: toneDim})
	}
	sections := []section{{label: "asked for", fields: fields}}

	what := section{label: "what it will send there"}
	for _, use := range req.Uses {
		what.lines = append(what.lines, line{text: use, bullet: true})
	}
	if req.Justification != "" {
		if len(what.lines) > 0 {
			what.lines = append(what.lines, line{})
		}
		what.lines = append(what.lines, line{text: req.Justification, tone: toneDim})
	}
	if len(what.lines) > 0 {
		sections = append(sections, what)
	}

	shown := section{label: "what the pool was shown"}
	for i, cert := range req.Trust.Chain {
		tone := toneDim
		if i == 0 {
			tone = toneAccent
		}
		shown.lines = append(shown.lines, line{text: cert.Subject, tone: tone, bullet: true})
		detail := "issued by " + cert.Issuer
		if cert.SelfSigned {
			detail = "self-signed"
		}
		if len(cert.Names) > 0 {
			detail += " · for " + strings.Join(cert.Names, ", ")
		}
		shown.lines = append(shown.lines,
			line{text: "  " + detail, tone: toneDim},
			line{text: fmt.Sprintf("  valid %s to %s · sha256 %s", cert.NotBefore.Format(time.DateOnly), cert.NotAfter.Format(time.DateOnly), shortFingerprint(cert.SHA256)), tone: toneDim})
	}
	if ca := req.Trust.SuppliedCA; ca != nil {
		shown.lines = append(shown.lines, line{},
			line{text: "the agent also supplied " + ca.Subject + ", which this chain verifies against", tone: toneDim})
	}
	return append(sections, shown)
}

func pinLabel(pin TrustPin) string {
	if pin.Kind == TrustPinLeafSPKI {
		return "its key · " + shortFingerprint(pin.SHA256)
	}
	return "its CA · " + shortFingerprint(pin.SHA256)
}

// shortFingerprint is a hash at a width a person compares by eye: the head and
// tail, which is what they check against the one they were told.
func shortFingerprint(hash string) string {
	if len(hash) <= 16 {
		return hash
	}
	return hash[:8] + "…" + hash[len(hash)-8:]
}
