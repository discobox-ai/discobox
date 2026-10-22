package proxyagent

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
	"github.com/discobox-ai/discobox/proxy"
)

const (
	// auditFieldBytes bounds each text field of one exchange a sandbox reads
	// back, so one long URL or judge's reason does not crowd out the rows
	// around it.
	auditFieldBytes = 2 << 10
	// auditPageBytes is what a page's exchanges may encode to: the response a
	// client decodes (agentcreds.MaxResponseBytes) less room for the envelope.
	// Field bounds alone cannot promise it, since JSON escapes some bytes six
	// times over and the host is whatever the sandbox sent.
	auditPageBytes = agentcreds.MaxResponseBytes - 4<<10
)

// auditRedacted stands in for a sentinel in an exchange read back.
const auditRedacted = "<redacted>"

// AuditHTTP reads this sandbox's own exchanges from the proxy's trail, in this
// process (ADR 26-10-02-678 §2). The sandbox is the broker's, which the
// certificate named; the query has no field that could name another.
//
// A URL is recorded as the sandbox sent it, before any swap, so it can carry
// an ephemeral sentinel the proxy does not swap in a URL. A live one is a
// bearer token for its window, and this verb must not hand one back to a
// sandbox that never had it outside the command it was minted for (ADR 0092),
// so every live sentinel of this sandbox's is redacted from what is returned.
//
// Exchanges are returned newest first until the page would outgrow what a
// client decodes; the rest are left for a narrower read.
func (b *credentialBroker) AuditHTTP(ctx context.Context, query agentcreds.AuditQuery) ([]agentcreds.AuditExchange, error) {
	rows, err := b.proxy.ClientHTTPAudit(ctx, b.sandboxID, proxy.AuditQuery{
		Host:    query.Host,
		Blocked: query.Blocked,
		Since:   query.Since,
		Limit:   query.Limit,
	})
	if err != nil {
		return nil, err
	}
	redact := newSentinelRedactor(b.activations.sentinelsByClient()[b.sandboxID])
	out := make([]agentcreds.AuditExchange, 0, len(rows))
	size := 0
	for _, row := range rows {
		exchange := agentcreds.AuditExchange{
			ID:            row.ID.String(),
			CreatedAt:     row.CreatedAt,
			Method:        truncateAuditField(row.Method),
			URL:           truncateAuditField(redact.url(row.URL)),
			Host:          truncateAuditField(row.Host),
			Status:        row.Status,
			Blocked:       row.Blocked,
			BlockedReason: truncateAuditField(redact.text(row.BlockedReason)),
			DurationMS:    row.DurationMillis,
		}
		if row.SwappedUseIDs != "" {
			exchange.UseIDs = strings.Split(row.SwappedUseIDs, ",")
		}
		encoded, err := json.Marshal(exchange)
		if err != nil {
			return nil, err
		}
		if size += len(encoded) + 1; size > auditPageBytes {
			break
		}
		out = append(out, exchange)
	}
	return out, nil
}

// sentinelRedactor takes a sandbox's live sentinels out of text read back to
// it, as written or percent-encoded once: a URL builder escapes a sentinel's
// + and / (and any literal of its format), so matching the minted form alone
// would hand back the value one decode away. A sentinel the program wrapped
// further — encoded twice, or inside base64 — is not found; reading it back
// needs the program to have hidden it on purpose, from the sandbox that held
// it inside that one command.
type sentinelRedactor struct {
	sentinels []string
	forms     *strings.Replacer
}

func newSentinelRedactor(sentinels []string) sentinelRedactor {
	var pairs []string
	for _, sentinel := range sentinels {
		forms := map[string]bool{sentinel: true}
		for _, escaped := range []string{url.QueryEscape(sentinel), url.PathEscape(sentinel)} {
			forms[escaped] = true
			forms[lowerPercentHex(escaped)] = true
		}
		for form := range forms {
			pairs = append(pairs, form, auditRedacted)
		}
	}
	return sentinelRedactor{sentinels: sentinels, forms: strings.NewReplacer(pairs...)}
}

// text redacts every form of a sentinel newSentinelRedactor knows.
func (r sentinelRedactor) text(value string) string {
	if len(r.sentinels) == 0 {
		return value
	}
	return r.forms.Replace(value)
}

// url also decodes each path segment and query value on its own and redacts
// the whole of one that holds a sentinel, which catches an encoding that mixes
// cases or escapes more than it must.
func (r sentinelRedactor) url(value string) string {
	if len(r.sentinels) == 0 {
		return value
	}
	parsed, err := url.Parse(value)
	if err == nil {
		changed := false
		segments := strings.Split(parsed.EscapedPath(), "/")
		for i, segment := range segments {
			if decoded, err := url.PathUnescape(segment); err != nil || r.holds(decoded) {
				segments[i], changed = auditRedacted, true
			}
		}
		pairs := strings.Split(parsed.RawQuery, "&")
		for i, pair := range pairs {
			if decoded, err := url.QueryUnescape(pair); err != nil || r.holds(decoded) {
				key, _, _ := strings.Cut(pair, "=")
				pairs[i], changed = key+"="+auditRedacted, true
				if decodedKey, err := url.QueryUnescape(key); err != nil || r.holds(decodedKey) {
					pairs[i] = auditRedacted
				}
			}
		}
		if !changed {
			return r.text(value)
		}
		rebuilt := ""
		if parsed.Scheme != "" || parsed.Host != "" {
			rebuilt = parsed.Scheme + "://" + parsed.Host
		}
		rebuilt += strings.Join(segments, "/")
		if parsed.RawQuery != "" || parsed.ForceQuery {
			rebuilt += "?" + strings.Join(pairs, "&")
		}
		if parsed.Fragment != "" {
			rebuilt += "#" + parsed.EscapedFragment()
		}
		value = rebuilt
	}
	return r.text(value)
}

func (r sentinelRedactor) holds(value string) bool {
	for _, sentinel := range r.sentinels {
		if strings.Contains(value, sentinel) {
			return true
		}
	}
	return false
}

// lowerPercentHex spells every %XX escape in value with lowercase hex.
func lowerPercentHex(value string) string {
	out := []byte(value)
	for i := 0; i+2 < len(out); i++ {
		if out[i] == '%' {
			out[i+1] = lowerHex(out[i+1])
			out[i+2] = lowerHex(out[i+2])
		}
	}
	return string(out)
}

func lowerHex(c byte) byte {
	if 'A' <= c && c <= 'F' {
		return c + 'a' - 'A'
	}
	return c
}

// truncateAuditField cuts value to auditFieldBytes on a rune boundary, saying
// that it did.
func truncateAuditField(value string) string {
	if len(value) <= auditFieldBytes {
		return value
	}
	cut := auditFieldBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "…"
}
