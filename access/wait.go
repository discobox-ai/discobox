package access

import (
	"context"
	"flag"
	"strings"
	"time"
)

// runWait resumes an existing request. The kind is explicit because request
// IDs belong to the service, not to this portable client.
func runWait(ctx context.Context, args []string) int {
	var structured bool
	var timeout time.Duration
	flags := flag.NewFlagSet("wait", flag.ContinueOnError)
	flags.BoolVar(&structured, "json", false, "emit JSON")
	flags.DurationVar(&timeout, "timeout", time.Hour, "how long to wait for approval")
	if !parse(flags, args) {
		return exitUsage
	}
	out := newEmitter(structured)
	if flags.NArg() != 2 || (flags.Arg(0) != "request" && flags.Arg(0) != "trust") {
		return usageError(out, "use wait [--json] [--timeout 1h] request|trust REQUEST_ID")
	}
	kind, requestID := flags.Arg(0), flags.Arg(1)
	if strings.TrimSpace(requestID) == "" || requestID == "." || requestID == ".." || strings.ContainsAny(requestID, "/?#% \t\r\n") {
		return usageError(out, "request ID must be a single non-empty path segment")
	}
	if timeout <= 0 {
		return usageError(out, "timeout must be positive")
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := newClient()
	out.pending(kind, requestID, true)
	if kind == "trust" {
		status, err := client.WaitForTrustRequest(waitCtx, requestID, 2*time.Second)
		if err != nil {
			return out.report(err)
		}
		return out.trustStatus(status)
	}
	status, err := client.WaitForRequest(waitCtx, requestID, 2*time.Second)
	if err != nil {
		return out.report(err)
	}
	return out.requestStatus(status)
}
