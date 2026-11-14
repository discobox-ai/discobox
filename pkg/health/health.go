// Package health is the wire contract for the server's readiness endpoint.
//
// Both sides of it live in different modules — the server answers, the CLI
// polls while waiting for a server it just launched — so the shape belongs to
// neither of them.
package health

import "time"

// Path is where the server serves Status.
const Path = "/healthz"

// The values Status.Status takes.
const (
	// StatusStarting is served with 503: the process is up and has bound its
	// listeners, but is still initializing. Phase says what it is doing.
	StatusStarting = "starting"
	// StatusUpgradingPools is served with 503: the server has started and its
	// API serves, but a pool it had before this start has not yet been brought
	// onto this server's agent. PoolsUpgrading counts them. It lasts at most
	// PoolUpgradeWait; after that the server reports ready, with a warning
	// saying how many pools are still upgrading, until none is.
	StatusUpgradingPools = "upgrading_pools"
	// StatusReady is served with 200.
	StatusReady = "ready"
)

// PoolUpgradeWait is how long after it starts serving the server reports
// StatusUpgradingPools before it reports ready anyway, warning of the pools it is
// still waiting on: the same ceiling an explicit start waits on its pool.
const PoolUpgradeWait = 5 * time.Minute

// Status is what the server reports about itself.
//
// It is served from the moment the listener binds rather than only once the
// server is up, because the interesting question — why is this taking so long
// — can only be answered while the answer is still "it is taking a while".
type Status struct {
	Status string `json:"status"`
	// Warnings are what the launching client should show once: startup
	// decisions, which remain present when ready so a client cannot miss a
	// fast startup, and pools still upgrading once the wait for them ran out,
	// which leaves once none is.
	Warnings []string `json:"warnings,omitempty"`
	// Phase is the step in progress, present only while starting or
	// upgrading pools.
	Phase string `json:"phase,omitempty"`
	// PoolsUpgrading is how many pools this server has not yet brought onto
	// its agent: present while upgrading pools, and on a ready server whose
	// wait for them ran out. A ready server with none has settled.
	//
	// A count and never the pools: this endpoint answers anyone, before any
	// authentication, and which pools a server runs and why one is failing
	// are the projects' own. The server's log names them.
	PoolsUpgrading int    `json:"poolsUpgrading,omitempty"`
	Version        string `json:"version,omitempty"`
	// UptimeSeconds is how long this process has been running.
	UptimeSeconds float64 `json:"uptimeSeconds"`
}

// Starting reports whether this status describes a server that has not
// finished starting.
func (s Status) Starting() bool { return s.Status == StatusStarting }

// UpgradingPools reports whether this status describes a server that has
// started and is bringing its pools onto its agent.
func (s Status) UpgradingPools() bool { return s.Status == StatusUpgradingPools }

// Waiting reports whether this is a 503 a client should wait out: a server
// starting, or upgrading its pools.
func (s Status) Waiting() bool { return s.Starting() || s.UpgradingPools() }

// Settled reports whether the server is ready and has no pool still
// upgrading: what a test that has just started or upgraded a server waits
// for before it uses a box.
func (s Status) Settled() bool { return s.Status == StatusReady && s.PoolsUpgrading == 0 }
