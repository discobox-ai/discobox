// Package agentcreds is the agent credentials protocol: the portable
// list/request/get contract an agent inside a sandbox uses to ask a human for a
// credential it was not provisioned with, and then to use it.
//
// The package knows nothing about Discobox. It carries the wire types, a
// client, and an http.Handler over a Service interface, so the same in-sandbox
// CLI works against Discobox's sandbox-agent and against any other
// implementation of Service.
//
// The value Get returns is deliberately unspecified: an implementation may
// return the real credential, and Discobox returns an ephemeral sentinel its
// egress proxy exchanges for the real value on the way out. Callers must treat
// it as opaque and stop using it after ExpiresAt.
//
// See docs/agent-credentials-protocol.md and
// docs/adr/0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md.
package agentcreds

import "time"

// Version is the protocol version every route is served under.
const Version = "v1"

// Client configuration. A client needs a base URL and, for implementations
// that want one, a bearer token. These names and the default address live here
// rather than with any one implementation, so a client built from this package
// alone knows how to find a server.
const (
	// DefaultAddress is the loopback address an implementation is expected to
	// serve on when it serves the protocol locally. Loopback because a server
	// that answers for whoever connects must not be reachable from a sibling
	// machine.
	DefaultAddress = "127.0.0.1:17010"
	// DefaultBaseURL is where a client looks when nothing configures it.
	DefaultBaseURL = "http://" + DefaultAddress
	// URLEnv overrides DefaultBaseURL.
	URLEnv = "DISCOBOX_CREDENTIALS_URL"
	// TokenEnv supplies a bearer token, for implementations that require one.
	TokenEnv = "DISCOBOX_CREDENTIALS_TOKEN" //nolint:gosec // Variable name, not a credential.
)

// Error codes. They are the machine-readable half of a failure, for callers
// that must decide what to do next rather than print a sentence — an agent,
// most of all.
//
// The set is deliberately coarse. It says what the caller should do, never why
// the server said no: unknown, revoked, and expired all report CodeDenied,
// because distinguishing them would tell an untrusted caller more about the
// state of an approval than it needs to know, and the remedy is the same for
// all three.
const (
	// CodeInvalid: the request was malformed. Fix it and retry.
	CodeInvalid = "invalid"
	// CodeDenied: the caller may not do this. Ask for it with `request`.
	CodeDenied = "denied"
	// CodeNotFound: the id means nothing to this server.
	CodeNotFound = "not_found"
	// CodeUnavailable: the server could not answer right now. Retry later; the
	// same call may well succeed.
	CodeUnavailable = "unavailable"
)

// Route paths, relative to the configured base URL. They are named here rather
// than spelled at each call site so client and handler cannot drift.
const (
	// PathCredentials lists granted credentials and their approved uses.
	PathCredentials = "/" + Version + "/credentials"
	// PathRequests creates a credential request; a request ID appended to it
	// reads that request's status.
	PathRequests = "/" + Version + "/credentials/requests"
	// PathUse takes a value for one declared command.
	PathUse = "/" + Version + "/credentials/use"
)

// Request status values. A request settles from Pending to exactly one of
// Granted or Denied and never moves again.
const (
	StatusPending = "pending"
	StatusGranted = "granted"
	StatusDenied  = "denied"
)

// Use is one approved way to use a credential. UseID is what Get accepts;
// Description is the human-approved sentence the use was granted for.
type Use struct {
	UseID       string     `json:"useId"`
	Description string     `json:"description"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
}

// Credential is one credential the caller may use, as reported by list. It
// never carries a value.
type Credential struct {
	Name   string `json:"name"`
	EnvVar string `json:"envVar"`
	Host   string `json:"host,omitempty"`
	Uses   []Use  `json:"uses,omitempty"`
}

// ListResponse is the list operation's body.
type ListResponse struct {
	Credentials []Credential `json:"credentials"`
}

// RequestedUse is one use an agent asks for. It has no ID: IDs are minted by
// the approval, so an agent cannot name the use it will later present.
type RequestedUse struct {
	Description string `json:"description"`
}

// RequestBody asks a human for a credential.
type RequestBody struct {
	Name          string         `json:"name"`
	EnvVar        string         `json:"envVar"`
	Host          string         `json:"host"`
	Justification string         `json:"justification,omitempty"`
	Uses          []RequestedUse `json:"uses,omitempty"`
}

// RequestStatus is what request and its poll both answer with. Uses is
// populated once granted, and is authoritative over the requested uses: the
// approver may have edited the descriptions.
type RequestStatus struct {
	RequestID string `json:"requestId"`
	Status    string `json:"status"`
	Uses      []Use  `json:"uses,omitempty"`
}

// Settled reports whether a request has reached a terminal status, which is
// what a polling client waits for.
func (s RequestStatus) Settled() bool {
	return s.Status == StatusGranted || s.Status == StatusDenied
}

// UseBody takes a value for one command. Command is the argv the caller is
// about to run, declared before the value is handed out. It narrows the window
// and gives the audit log a per-use story; it is never a trust anchor, because
// the caller could lie about it.
type UseBody struct {
	UseID   string   `json:"useId"`
	Command []string `json:"command,omitempty"`
}

// UseResponse carries the value to place in EnvVar for that one command, and
// the end of its window.
type UseResponse struct {
	EnvVar    string     `json:"envVar"`
	Value     string     `json:"value"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// ErrorResponse is the body of every non-2xx response. Code is one of the
// Code* constants; Error is the human sentence behind it.
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}
