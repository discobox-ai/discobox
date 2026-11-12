// Package agentcreds is the agent credentials protocol: the portable
// list/request contract an agent inside a sandbox uses to ask a human for a
// credential it was not provisioned with, and to learn when it has it.
//
// The package knows nothing about Discobox. It carries the wire types, a
// client, and an http.Handler over a Service interface, so the same in-sandbox
// CLI works against Discobox's sandbox-agent and against any other
// implementation of Service.
//
// A granted credential is delivered by the implementation, not through the
// protocol: Discobox puts a sentinel in the variable the request named, which
// its egress proxy exchanges for the real value on the hosts the grant covers
// (ADR 26-10-27-880), or, for a grant a person made raw, the value itself or a
// file holding it (ADR 26-10-27-840). Nothing here ever carries a value.
//
// See docs/agent-credentials-protocol.md and
// docs/adr/0031-agent-credentials-are-a-portable-protocol-with-ephemeral-sentinels.md.
package agentcreds

import (
	"encoding/json"
	"time"
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
	// PathCredentials lists granted credentials.
	PathCredentials = "/" + Version + "/credentials"
	// PathRequests creates a credential request; a request ID appended to it
	// reads that request's status.
	PathRequests = "/" + Version + "/credentials/requests"
	// PathEnvironment reads the variables credentials are delivered in now.
	PathEnvironment = "/" + Version + "/credentials/environment"
)

// Request status values. A request settles from Pending to exactly one of
// Granted or Denied and never moves again.
const (
	StatusPending = "pending"
	StatusGranted = "granted"
	StatusDenied  = "denied"
)

// Credential is one credential the caller may use, as reported by list. It
// never carries a value: EnvVar is the variable its sentinel is delivered in,
// and Hosts are where it may be sent (ADR 26-10-02-393). Purpose is
// PurposeDelegate for one the caller may give the sandboxes it creates, which
// it is not delivered itself.
//
// Raw is set for one the caller holds the value of rather than a placeholder
// for (RawEnv, RawFile).
//
// Grants are, for one delivered in a variable, each grant it is held to (ADR
// 26-11-03-329 §6): the binding that delivers it, then each addition
// stacking on it, each with its own hosts and expiry. A request is sent when
// some grant allows it, so an addition's hosts go when it lapses while the
// others' stay. Hosts and ExpiresAt are their union and the latest of them.
type Credential struct {
	Name      string            `json:"name"`
	EnvVar    string            `json:"envVar,omitempty"`
	Hosts     []string          `json:"hosts,omitempty"`
	Purpose   string            `json:"purpose,omitempty"`
	Raw       string            `json:"raw,omitempty"`
	ExpiresAt *time.Time        `json:"expiresAt,omitempty"`
	Grants    []CredentialGrant `json:"grants,omitempty"`
}

// CredentialGrant is one grant a credential is held to: where it may be sent
// under it, and until when. Adds is an addition (ADR 26-11-03-329 §1).
type CredentialGrant struct {
	Hosts     []string   `json:"hosts"`
	Adds      bool       `json:"adds,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// ListResponse is the list operation's body.
type ListResponse struct {
	Credentials []Credential `json:"credentials"`
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

// What a binding lets the caller do with a credential, as list reports it:
// use it, or give it to the sandboxes it creates. A request asks for either or
// both, as its use and delegate parts (ADR 26-11-02-022 §1).
const (
	// PurposeUse is a credential delivered in its variable.
	PurposeUse = "use"
	// PurposeDelegate is a credential the caller may give the sandboxes it
	// creates. It authorizes nothing the caller sends itself.
	PurposeDelegate = "delegate"
)

// How a credential is delivered when the caller holds its value rather than
// a placeholder for it, as list and a granted request report it. Only a
// person delivers one so; a request never asks for it. Empty is a placeholder.
const (
	// RawEnv puts the value in the variable.
	RawEnv = "env"
	// RawFile puts the path of a file holding the value in the variable.
	RawFile = "file"
)

// RequestBody asks a human for a credential: to use it, to delegate it, or
// both, in one request a person answers at once (ADR 26-11-02-022 §1).
//
// GrantTTLSeconds is how long the agent asks the approval to last, for both
// parts. It is a suggestion, not a term: the approver sees it as the answer
// already chosen and may pick any other. Zero asks for nothing in particular,
// and the ceiling is MaxGrantTTLSeconds, so forever is not something an agent
// can ask for — the human may still grant it, but the one answer that never
// comes back to be asked again is never the default.
//
// The use part is EnvVar with Hosts, Routes or Policies: where the
// credential will be sent, delivered in the variable. Delegate is the
// delegate part, which names no variable. A request has at least one part.
//
// Hosts and Routes name destinations. A host is shorthand for one route to
// that exact host and no host beneath it (ADR 26-10-02-393). A route is the
// typed form `[METHODS ]HOSTS[PATHS][ graphql=OPS[:FIELDS]][ mcp=TOOLS][ body=PLACES][=blocked]`
// (ADR 26-10-27-880 §4), such as `GET api.github.com/repos/o/r/**` or
// `api.github.com/graphql graphql=query`; the protocol carries it as written
// and the implementation reads it. Policies name credential policy files
// instead (ADR 26-10-30-584); on one part they are exclusive with routes and
// hosts.
//
// Add asks for the use part's routes in addition to what the caller holds
// (ADR 26-11-03-329 §4): granted so, they are an addition beside the binding
// delivering EnvVar, with a lifetime of their own, and that binding is left
// as it is. Without it the use part is the caller's whole use of the
// credential, replacing what it holds. The approver may grant either way.
//
// Justification is shown to the person answering; nothing evaluates it.
type RequestBody struct {
	Name            string        `json:"name"`
	EnvVar          string        `json:"envVar,omitempty"`
	Add             bool          `json:"add,omitempty"`
	Hosts           []string      `json:"hosts,omitempty"`
	Routes          []string      `json:"routes,omitempty"`
	Policies        []Policy      `json:"policies,omitempty"`
	Delegate        *DelegatePart `json:"delegate,omitempty"`
	Justification   string        `json:"justification,omitempty"`
	GrantTTLSeconds int64         `json:"grantTTLSeconds,omitempty"`
}

// Uses reports whether the request has a use part.
func (b RequestBody) Uses() bool {
	return b.EnvVar != "" || len(b.Hosts) > 0 || len(b.Routes) > 0 || len(b.Policies) > 0
}

// DelegatePart is the part of a request that asks to give the credential to
// the sandboxes the caller creates. Routes or Policies, never both.
type DelegatePart struct {
	Routes   []string `json:"routes,omitempty"`
	Policies []Policy `json:"policies,omitempty"`
}

// Policy names one credential policy file in a request (ADR 26-11-02-022 §2).
// File is how the caller named it, and what an approver is shown; Args are
// its arguments. Document is the file's JSON object, carried as it was read:
// the implementation validates and resolves it, never the client, which reads
// the file and fills Document in before sending.
type Policy struct {
	File     string            `json:"file"`
	Args     map[string]string `json:"args,omitempty"`
	Document json.RawMessage   `json:"document,omitempty"`
}

// Limits on what a request may carry (ADR 26-11-02-022 §2). A client refuses
// a document over MaxPolicyBytes naming its file; the implementation refuses
// a request over any of them.
const (
	// MaxPolicyBytes bounds one policy document.
	MaxPolicyBytes = 64 << 10
	// MaxRequestPolicies bounds the policies of one request, across its parts.
	MaxRequestPolicies = 32
	// MaxPartRoutes bounds the routes one part resolves to.
	MaxPartRoutes = 512
	// MaxRequestRouteBytes bounds what a whole request resolves to: the JSON
	// encoding of every route it stores, each part's and each policy's. A
	// count of routes does not bound their size: a small document can
	// expand its arguments into a route of a megabyte, so a request's limit
	// is on what it resolves to, not only on what it sent.
	MaxRequestRouteBytes = 256 << 10
)

// RequestStatus is what request and its poll both answer with. Status is
// pending until a person answers; then granted when any part was, denied
// when none was. Use and Delegate report each part the request has (ADR
// 26-11-02-022 §4).
type RequestStatus struct {
	RequestID string      `json:"requestId"`
	Status    string      `json:"status"`
	Use       *PartStatus `json:"use,omitempty"`
	Delegate  *PartStatus `json:"delegate,omitempty"`
}

// PartStatus is one part of a request. Once granted, Credential is what the
// caller now holds under it, as the approver chose it: the hosts and lifetime
// may differ from the ones asked for. A credential granted for use is in its
// variable for every command started from then on.
//
// GrantedAs is, once the use part is granted, how (ADR 26-11-03-329 §4):
// GrantedAsAdd, beside what the caller held in the variable, which stays as
// it was; GrantedAsReplace, its whole use of the credential, in place of what
// it held. It may differ from what was asked.
type PartStatus struct {
	Status     string      `json:"status"`
	Credential *Credential `json:"credential,omitempty"`
	GrantedAs  string      `json:"grantedAs,omitempty"`
}

// How a granted use part was granted.
const (
	GrantedAsAdd     = "add"
	GrantedAsReplace = "replace"
)

// Settled reports whether a request has reached a terminal status, which is
// what a polling client waits for.
func (s RequestStatus) Settled() bool {
	return s.Status == StatusGranted || s.Status == StatusDenied
}

// Complete reports whether every part the request has was granted: what a
// waiting client exits zero on, since a shell-driven agent reads zero as
// having what it asked for.
func (s RequestStatus) Complete() bool {
	if s.Status != StatusGranted {
		return false
	}
	for _, part := range []*PartStatus{s.Use, s.Delegate} {
		if part != nil && part.Status != StatusGranted {
			return false
		}
	}
	return true
}

// EnvironmentResponse is what the environment read answers with: every
// variable a credential is delivered in, and what is delivered there now, for
// a process that started before a grant to take it up — a harness's tool
// shell inherits the harness's launch environment, never a later one. In
// Discobox each value is the credential's sentinel, a placeholder only this
// sandbox's egress exchanges, never the credential itself.
type EnvironmentResponse struct {
	Variables map[string]string `json:"variables"`
}

// ErrorResponse is the body of every non-2xx response. Code is one of the
// Code* constants; Error is the human sentence behind it.
type ErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}
