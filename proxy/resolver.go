package proxy

import "github.com/discobox-ai/discobox/proxy/internal/secrets"

// SecretResolver resolves a sentinel placeholder to its real credential value.
// The pool agent implements it (pool-agent/proxyagent, calling the control
// plane) and always passes one; the proxy stays server-agnostic. A nil
// resolver passed to NewServer disables secret swapping.
type SecretResolver = secrets.Resolver

// SecretResolveRequest is the input to a SecretResolver.
type SecretResolveRequest = secrets.ResolveRequest

// SecretResolveResult is the output of a SecretResolver.
type SecretResolveResult = secrets.ResolveResult

// ErrSecretResolveDenied signals that a sentinel is unknown, unapproved, or not
// permitted for the requested host. The proxy leaves the sentinel in place.
var ErrSecretResolveDenied = secrets.ErrDenied

// SecretReportRequest tells a SecretResolver what an upstream made of a value
// it resolved: the sentinel, the destination, and the verdict. It never carries
// the credential (ADR 0132 §1).
type SecretReportRequest = secrets.ReportRequest

// SecretReportOutcome is what the response path saw.
type SecretReportOutcome = secrets.Outcome

// The three outcomes a swapped credential can produce that a resolver is told
// about. A credential that simply works is never reported.
const (
	// SecretRejected is a 401 with nothing different to retry with.
	SecretRejected = secrets.OutcomeRejected
	// SecretRejectedAfterRetry is a 401 on the retry too, sent with a
	// different credential than the one first refused.
	SecretRejectedAfterRetry = secrets.OutcomeRejectedAfterRetry
	// SecretAccepted clears a rejection this proxy reported earlier.
	SecretAccepted = secrets.OutcomeAccepted
)
