package agentcreds

import (
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// The egress audit: the caller's own record of what its outbound requests met
// (ADR 26-10-02-678). A program that swallows the refusal it was sent leaves
// the agent with a failure and no reason; this is where the reason is read
// back, so the agent knows whether to ask for a credential, ask for a trust, or
// narrow what it sent.

// PathAuditHTTP lists the caller's audited HTTP exchanges.
const PathAuditHTTP = "/" + Version + "/audit/http"

const (
	// DefaultAuditLimit is how many exchanges a read with no limit returns:
	// enough to find the request that just failed among the ones around it.
	DefaultAuditLimit = 20
	// MaxAuditLimit bounds one read. An implementation may return fewer than
	// asked, newest first, to keep the page within MaxResponseBytes.
	MaxAuditLimit = 200
)

// AuditQuery narrows an audit read. It never names a sandbox: whose exchanges
// these are is the transport's to say, as it is for every other verb.
type AuditQuery struct {
	// Host keeps exchanges with this destination host; named without a port,
	// at any port.
	Host string
	// Blocked keeps the exchanges the implementation refused when true and the
	// ones it let through when false; nil is both.
	Blocked *bool
	// Since keeps exchanges at or after it.
	Since time.Time
	// Limit is how many exchanges to return, newest first: DefaultAuditLimit
	// when zero, and never more than MaxAuditLimit.
	Limit int
}

// AuditExchange is one outbound HTTP request and what became of it. It is a
// summary, never the record: no headers and no bodies, which say nothing about
// why a request failed that the fields here do not.
type AuditExchange struct {
	// ID names the exchange in the implementation's own audit, where a person
	// reads the full record.
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	Method    string    `json:"method,omitempty"`
	URL       string    `json:"url"`
	Host      string    `json:"host"`
	// Status is the response status the caller was sent; zero when none was.
	Status int `json:"status,omitempty"`
	// Blocked is true when the implementation refused the request rather than
	// the destination, and BlockedReason says why.
	Blocked       bool   `json:"blocked"`
	BlockedReason string `json:"blockedReason,omitempty"`
	DurationMS    int64  `json:"durationMs,omitempty"`
	// UseIDs are the approved credential uses whose sentinels the request
	// carried: spent when it was let through, refused under when Blocked.
	UseIDs []string `json:"useIds,omitempty"`
}

// AuditHTTPResponse is the HTTP audit read's body.
type AuditHTTPResponse struct {
	Exchanges []AuditExchange `json:"exchanges"`
}

// values is the query string a client sends for q.
func (q AuditQuery) values() url.Values {
	values := url.Values{}
	if q.Host != "" {
		values.Set("host", q.Host)
	}
	if q.Blocked != nil {
		values.Set("blocked", strconv.FormatBool(*q.Blocked))
	}
	if !q.Since.IsZero() {
		values.Set("since", q.Since.UTC().Format(time.RFC3339Nano))
	}
	if q.Limit != 0 {
		values.Set("limit", strconv.Itoa(q.Limit))
	}
	return values
}

// parseAuditQuery reads a query string into an AuditQuery with its limit
// settled, so an implementation never sees zero or more than MaxAuditLimit.
func parseAuditQuery(values url.Values) (AuditQuery, error) {
	query := AuditQuery{Host: values.Get("host"), Limit: DefaultAuditLimit}
	if raw := values.Get("blocked"); raw != "" {
		blocked, err := strconv.ParseBool(raw)
		if err != nil {
			return AuditQuery{}, fmt.Errorf("%w: blocked %q is not true or false", ErrInvalid, raw)
		}
		query.Blocked = &blocked
	}
	if raw := values.Get("since"); raw != "" {
		since, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return AuditQuery{}, fmt.Errorf("%w: since %q is not an RFC 3339 time", ErrInvalid, raw)
		}
		query.Since = since
	}
	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 {
			return AuditQuery{}, fmt.Errorf("%w: limit %q is not a positive number", ErrInvalid, raw)
		}
		query.Limit = min(limit, MaxAuditLimit)
	}
	return query, nil
}
