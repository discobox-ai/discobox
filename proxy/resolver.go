package proxy

import (
	"context"

	"github.com/discobox-ai/discobox/proxy/internal/secrets"
)

// SecretResolver is the proxy's way to the control plane for every value it
// must not hold itself: a sentinel's real credential, and the header values a
// gateway takes from the server's environment (ADR 26-10-16-470 §4). The pool
// agent implements it (pool-agent/proxyagent, calling the control plane) and
// always passes one; the proxy stays server-agnostic. A nil resolver passed to
// NewServer disables secret swapping, and refuses every request whose gateway
// takes a header from the environment.
type SecretResolver interface {
	secrets.Resolver
	// ResolveGatewayHeaders returns the value of each variable req names, as
	// the server's environment has it. A variable the environment does not
	// set is absent from the map; an error is no answer at all.
	ResolveGatewayHeaders(ctx context.Context, req GatewayHeadersRequest) (map[string]string, error)
}

// SecretResolveRequest is the input to a SecretResolver.
type SecretResolveRequest = secrets.ResolveRequest

// SecretResolveResult is the output of a SecretResolver.
type SecretResolveResult = secrets.ResolveResult

// SecretRawRequest asks a SecretResolver for the value of a client's raw
// binding, by its sentinel (ADR 26-10-27-840).
type SecretRawRequest = secrets.RawRequest

// SecretRawResult is a raw binding's value as a SecretResolver answers it.
type SecretRawResult = secrets.RawResult

// ErrSecretResolveDenied signals that a sentinel is unknown, unapproved, or not
// permitted for the requested host. The proxy leaves the sentinel in place.
var ErrSecretResolveDenied = secrets.ErrDenied

// SecretGateRequest is a request for the gate host, as a SecretResolver
// answers it.
type SecretGateRequest = secrets.GateRequest

// SecretGateAdmission is a request a SecretResolver's gate let in.
type SecretGateAdmission = secrets.GateAdmission

// SecretGateRefusal is a SecretResolver's gate declining a request, and why.
type SecretGateRefusal = secrets.GateRefusal

// SecretAuthorizeRequest is a request carrying sentinels, as a SecretResolver
// authorizes it before resolving any of them.
type SecretAuthorizeRequest = secrets.AuthorizeRequest

// SecretVerdict is a SecretResolver's answer to a SecretAuthorizeRequest.
type SecretVerdict = secrets.Verdict

// SecretReportScope names the requests that answer for a resolved credential;
// SecretReportRule is one host of it, and the paths on that host.
type (
	SecretReportScope = secrets.ReportScope
	SecretReportRule  = secrets.ReportRule
)

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
