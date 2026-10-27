package proxyagent

import (
	"context"
	"encoding/json"
	"unicode/utf8"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
	"github.com/discobox-ai/discobox/proxy"
)

const (
	// auditFieldBytes bounds each text field of one exchange a sandbox reads
	// back, so one long URL or refusal's reason does not crowd out the rows
	// around it.
	auditFieldBytes = 2 << 10
	// auditPageBytes is what a page's exchanges may encode to: the response a
	// client decodes (agentcreds.MaxResponseBytes) less room for the envelope.
	// Field bounds alone cannot promise it, since JSON escapes some bytes six
	// times over and the host is whatever the sandbox sent.
	auditPageBytes = agentcreds.MaxResponseBytes - 4<<10
)

// AuditHTTP reads this sandbox's own exchanges from the proxy's trail, in this
// process (ADR 26-10-02-678 §2). The sandbox is the broker's, which the
// certificate named; the query has no field that could name another.
//
// A URL is recorded as the sandbox sent it, before any swap, so a sentinel in
// it is the sandbox's own, which it already holds.
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
	out := make([]agentcreds.AuditExchange, 0, len(rows))
	size := 0
	for _, row := range rows {
		exchange := agentcreds.AuditExchange{
			ID:            row.ID.String(),
			CreatedAt:     row.CreatedAt,
			Method:        truncateAuditField(row.Method),
			URL:           truncateAuditField(row.URL),
			Host:          truncateAuditField(row.Host),
			Status:        row.Status,
			Blocked:       row.Blocked,
			BlockedReason: truncateAuditField(row.BlockedReason),
			DurationMS:    row.DurationMillis,
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
