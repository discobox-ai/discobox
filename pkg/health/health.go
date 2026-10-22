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
)

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
	// UptimeSeconds is how long this process has been running.
	UptimeSeconds float64 `json:"uptimeSeconds"`
}

// Starting reports whether this status describes a server that has not
// finished starting.
func (s Status) Starting() bool { return s.Status == StatusStarting }
