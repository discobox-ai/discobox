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

import (
	"time"

	"github.com/discobox-ai/discobox/pkg/policy"
)

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

// MaxReportedBytes is the most any one field of Reported may be. An
// implementation may refuse a use whose report has a longer one. It is small
// because JSON may write a byte as six, and the four fields share one body
// (MaxBodyBytes) with the most of stdin a caller shows.
const MaxReportedBytes = 256

// UseTimeout is how long a use call may take. An implementation may judge the
// command before it hands out a value, and a judge that has to be brought up
// first takes minutes rather than seconds, so a client waits this long for
// the answer rather than the few seconds every other call gets.
const UseTimeout = 4 * time.Minute

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
// never carries a value. Hosts are where it may be sent (ADR 26-10-02-393).
type Credential struct {
	Name   string   `json:"name"`
	EnvVar string   `json:"envVar"`
	Hosts  []string `json:"hosts,omitempty"`
	Uses   []Use    `json:"uses,omitempty"`
	// Policy is what the grant authorizes, as approved, when it carries one
	// (ADR 26-10-20-782 §1).
	Policy *policy.Policy `json:"policy,omitempty"`
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

// MaxGrantTTLSeconds is the longest lifetime an agent may ask for: thirty days.
//
// An ask is bounded because of what it is for. It is shown to a human as the
// answer already chosen, so it has to be an answer — one an approval can offer
// as a row and a person can read as a span of time. Without a ceiling "forever"
// arrives under another name: a ten-year ask is one Enter away from a
// credential that outlives the project, and a number large enough to overflow a
// duration lands back near zero, which is the one answer that never comes back
// to be asked again.
const MaxGrantTTLSeconds = 30 * 24 * 60 * 60

// DefaultGrantTTL is what an approval lasts when nobody said otherwise: the
// approver named no lifetime, and the agent asked for nothing in particular.
// The server answers an approval that names none with it, the window opens on
// it, and the CLI says it, so the same act mints the same grant wherever it is
// made.
//
// An hour: the shortest answer still long enough to finish the task the
// credential was asked for, and the one that costs nothing to be wrong about —
// a grant that outlives its task is a credential nobody remembers handing out.
const DefaultGrantTTL = time.Hour

// AskedGrantTTL is the lifetime an agent asked for, as RequestBody carries it.
// Zero is "asked for nothing in particular", which is what an ask outside what
// an agent may ask for (MaxGrantTTLSeconds) also becomes.
//
// Out of range is treated as no ask rather than clamped into one. The number
// arrives from inside a sandbox, and the value of an ask is that a person is
// shown what the agent said it needed; a number nobody could have meant is not
// that, and quietly rewriting it to thirty days would put words in the agent's
// mouth. The range check is also what keeps the multiplication below from
// overflowing, which would land a huge ask back near zero — indistinguishable
// from forever once rounded to whole seconds.
func AskedGrantTTL(seconds int64) time.Duration {
	if seconds <= 0 || seconds > MaxGrantTTLSeconds {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// What a request asks the credential for. A grant is one or the other, never
// both, so a person approving one agrees to one thing.
const (
	// PurposeUse asks to use the credential: the approval carries uses the
	// caller runs commands under. It is what a request naming no purpose asks.
	PurposeUse = "use"
	// PurposeDelegate asks to hand the credential on to other sandboxes. The
	// approval authorizes nothing the caller sends itself; its uses say what
	// the caller may delegate the credential for.
	PurposeDelegate = "delegate"
)

// RequestBody asks a human for a credential.
//
// GrantTTLSeconds is how long the agent asks the approval to last. It is a
// suggestion, not a term: the approver sees it as the answer already chosen and
// may pick any other. Zero asks for nothing in particular, and the ceiling is
// MaxGrantTTLSeconds, so forever is not something an agent can ask for — the
// human may still grant it, but the one answer that never comes back to be
// asked again is never the default.
//
// Purpose is PurposeUse or PurposeDelegate; empty asks for PurposeUse.
//
// Hosts are where the credential will be sent, one or several
// (ADR 26-10-02-393).
type RequestBody struct {
	// ID names a well-known credential — a reverse-DNS ID such as
	// "com.github.api" — in place of Name, EnvVar, and the hosts, which an
	// implementation fills from what it knows the ID to mean. An
	// implementation that knows no such ID refuses the request as invalid.
	ID              string         `json:"id,omitempty"`
	Name            string         `json:"name"`
	EnvVar          string         `json:"envVar"`
	Hosts           []string       `json:"hosts,omitempty"`
	Justification   string         `json:"justification,omitempty"`
	Uses            []RequestedUse `json:"uses,omitempty"`
	GrantTTLSeconds int64          `json:"grantTTLSeconds,omitempty"`
	Purpose         string         `json:"purpose,omitempty"`
	// Policy is what the agent asks the credential to authorize, as
	// statements, beside its uses (ADR 26-10-20-782 §1). An implementation
	// refuses one that does not validate.
	Policy *policy.Policy `json:"policy,omitempty"`
}

// RequestStatus is what request and its poll both answer with. Uses is
// populated once granted, and is authoritative over the requested uses: the
// approver may have edited the descriptions.
//
// Purpose is what the request asks the credential for, as the implementation
// recorded it. For PurposeUse the granted uses carry the IDs the use operation
// accepts; for PurposeDelegate they say what the caller may delegate the
// credential for, and none of them takes a value. Empty is an implementation
// that predates purposes, which only ever records an ask to use.
type RequestStatus struct {
	RequestID string `json:"requestId"`
	Status    string `json:"status"`
	Purpose   string `json:"purpose,omitempty"`
	Uses      []Use  `json:"uses,omitempty"`
	// Policy is the approved policy once granted, authoritative over the one
	// asked for as Uses is over the requested uses.
	Policy *policy.Policy `json:"policy,omitempty"`
}

// Settled reports whether a request has reached a terminal status, which is
// what a polling client waits for.
func (s RequestStatus) Settled() bool {
	return s.Status == StatusGranted || s.Status == StatusDenied
}

// UseBody takes a value for one command. Command is the argv the caller is
// about to run, declared before the value is handed out; Stdin and Reported
// are what that command will read on standard input and where the caller says
// it runs.
//
// An implementation may judge the command before it hands out anything, and
// Discobox does: a value is handed out only when its judge allows the command
// for the use, and a refusal is CodeDenied with the judge's reason (ADR
// 26-09-22-838 §3). Everything here is the caller's word, so it is evidence
// for that judgement and never authority; the use is what was approved.
type UseBody struct {
	UseID    string    `json:"useId"`
	Command  []string  `json:"command,omitempty"`
	Stdin    *Stdin    `json:"stdin,omitempty"`
	Reported *Reported `json:"reported,omitempty"`
}

// Stdin is what a command will read on standard input, as much of it as the
// caller shows: text, and a sentence for whatever of it is not shown.
type Stdin struct {
	// Content is the input shown, which is text. Empty when none of it is
	// shown, and Missing then says why.
	Content string `json:"content"`
	// Missing says why Content is not the whole input: longer than the caller
	// shows, still arriving, not text, or a read that failed.
	Missing string `json:"missing,omitempty"`
}

// Reported is what the caller says about where a command runs. Every field is
// optional, and at most MaxReportedBytes: they are a path or a line.
type Reported struct {
	WorkingDirectory string `json:"workingDirectory,omitempty"`
	RepositoryRoot   string `json:"repositoryRoot,omitempty"`
	// RefCommit and RefSubject are the commit a git ref the command names
	// resolves to, and that commit's subject line.
	RefCommit  string `json:"refCommit,omitempty"`
	RefSubject string `json:"refSubject,omitempty"`
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
