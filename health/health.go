// Package health is the wire contract for the server's readiness endpoint.
//
// Both sides of it live in different modules — the server answers, the CLI
// polls while waiting for a server it just launched — so the shape belongs to
// neither of them.
package health

// Path is where the server serves Status.
const Path = "/healthz"

// The values Status.Status takes.
const (
	// StatusStarting is served with 503: the process is up and has bound its
	// listeners, but is still initializing. Phase says what it is doing.
	StatusStarting = "starting"
	// StatusReady is served with 200.
	StatusReady = "ready"
	// StatusNeedsChoice is served with 503: startup is held until someone
	// makes the choice Choice describes (ADR 0148 §2). It is not starting —
	// waiting on it will not end on its own — so a client that launched the
	// server stops waiting and asks.
	StatusNeedsChoice = "needs-choice"
)

// SetupDefaultProviderPath is where a server holding at StatusNeedsChoice takes
// the answer: a POST of DefaultProviderChoice. It is served only while the
// server is holding, and only over the local IPC endpoint.
const SetupDefaultProviderPath = "/setup/default-provider"

// The reasons a first start cannot install the provider it would by default.
const (
	// ReasonKVMUnavailable is a host whose /dev/kvm cannot be opened or does
	// not speak the KVM API.
	ReasonKVMUnavailable = "kvm-unavailable"
	// ReasonArchUnsupported is a host the VM launcher is not built for.
	ReasonArchUnsupported = "arch-unsupported"
	// ReasonRuntimeUnloadable is a VM runtime that was fetched and will not
	// load on this host — a glibc older than it was built for, for one. A
	// runtime that could not be fetched is not a reason: that says nothing
	// about the host, and the start fails with it instead.
	ReasonRuntimeUnloadable = "runtime-unloadable"
)

// Choice is the question a server holding at StatusNeedsChoice is waiting on:
// the provider it would have installed, why it cannot, and what it will accept
// instead.
type Choice struct {
	// Provider is the provider type the server would have installed.
	Provider string `json:"provider"`
	// Reason is one of the Reason constants.
	Reason string `json:"reason"`
	// Detail is the error that decided it, for a human.
	Detail string `json:"detail,omitempty"`
	// Alternatives are the provider types the server will install instead.
	Alternatives []string `json:"alternatives"`
}

// DefaultProviderChoice is the body posted to SetupDefaultProviderPath.
type DefaultProviderChoice struct {
	Provider string `json:"provider"`
}

// Status is what the server reports about itself.
//
// It is served from the moment the listener binds rather than only once the
// server is up, because the interesting question — why is this taking so long
// — can only be answered while the answer is still "it is taking a while".
type Status struct {
	Status string `json:"status"`
	// Phase is the startup step in progress, present only while starting.
	Phase   string `json:"phase,omitempty"`
	Version string `json:"version,omitempty"`
	// Choice is present only at StatusNeedsChoice.
	Choice *Choice `json:"choice,omitempty"`
	// UptimeSeconds is how long this process has been running.
	UptimeSeconds float64 `json:"uptimeSeconds"`
}

// Starting reports whether this status describes a server that has not
// finished starting.
func (s Status) Starting() bool { return s.Status == StatusStarting }

// NeedsChoice reports whether this status describes a server holding its
// startup until a choice is made.
func (s Status) NeedsChoice() bool { return s.Status == StatusNeedsChoice && s.Choice != nil }
