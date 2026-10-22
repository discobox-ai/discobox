package access

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/agentcreds"
)

// runAudit reads this sandbox's own outbound requests back from the
// implementation's egress record (ADR 26-10-02-678): what was refused and why,
// for the program that failed without passing the refusal on.
func runAudit(ctx context.Context, args []string) int {
	var (
		structured       bool
		query            agentcreds.AuditQuery
		blocked, allowed bool
		since            time.Duration
	)
	flags := flag.NewFlagSet(Name+" audit", flag.ContinueOnError)
	flags.BoolVar(&structured, "json", false, "emit JSON")
	flags.StringVar(&query.Host, "host", "", "only requests to this host")
	flags.BoolVar(&blocked, "blocked", false, "only requests the egress refused")
	flags.BoolVar(&allowed, "allowed", false, "only requests the egress let through")
	flags.DurationVar(&since, "since", 0, "only requests in this long before now (e.g. 10m)")
	flags.IntVar(&query.Limit, "limit", agentcreds.DefaultAuditLimit, fmt.Sprintf("how many requests, newest first (at most %d)", agentcreds.MaxAuditLimit))
	if !parse(flags, args) {
		return exitUsage
	}
	out := newEmitter(structured)
	switch {
	case len(flags.Args()) > 0:
		return usageError(out, "unexpected argument %q", flags.Arg(0))
	case blocked && allowed:
		return usageError(out, "--blocked and --allowed exclude each other; leave both out for every request")
	case query.Limit < 1:
		return usageError(out, "--limit must be at least 1")
	case since < 0:
		return usageError(out, "--since must not be negative")
	}
	if blocked || allowed {
		query.Blocked = &blocked
	}
	if since > 0 {
		query.Since = time.Now().Add(-since)
	}

	exchanges, err := newClient().AuditHTTP(ctx, query)
	if errors.Is(err, agentcreds.ErrNotFound) {
		// The verb itself is unknown, not a record: a service from before it
		// answers an unrouted path 404, which read bare says "no such request".
		return out.report(fmt.Errorf("%w: this credentials service does not serve the audit; it predates %s audit", agentcreds.ErrNotFound, Name))
	}
	if err != nil {
		return out.report(err)
	}
	if exchanges == nil {
		exchanges = []agentcreds.AuditExchange{}
	}
	out.emit(agentcreds.AuditHTTPResponse{Exchanges: exchanges}, func(w io.Writer) {
		if len(exchanges) == 0 {
			fmt.Fprintln(w, "No outbound requests are recorded for this sandbox that match.")
			return
		}
		for _, exchange := range exchanges {
			writeExchange(w, exchange)
		}
	})
	return exitOK
}

// writeExchange is one request on one line, newest first as read, with the
// reason under a refused one: the reason is what the line is read for.
func writeExchange(w io.Writer, exchange agentcreds.AuditExchange) {
	verdict := "-  "
	if exchange.Status != 0 {
		verdict = fmt.Sprintf("%d", exchange.Status)
	}
	if exchange.Blocked {
		verdict += " BLOCKED"
	}
	method := exchange.Method
	if method == "" {
		method = "-"
	}
	fmt.Fprintf(w, "%s  %s  %s %s", exchange.CreatedAt.UTC().Format(time.RFC3339), verdict, method, exchange.URL)
	if exchange.DurationMS > 0 {
		fmt.Fprintf(w, "  (%dms)", exchange.DurationMS)
	}
	fmt.Fprintln(w)
	if exchange.BlockedReason != "" {
		fmt.Fprintf(w, "    reason: %s\n", exchange.BlockedReason)
	}
	if len(exchange.UseIDs) > 0 {
		fmt.Fprintf(w, "    uses: %s\n", strings.Join(exchange.UseIDs, ", "))
	}
}
