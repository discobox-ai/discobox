package sandboxruntime

import (
	"context"
	"io"
	"maps"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"
)

// What the pool reports of its snapshots (ADR 26-11-17-316). Every figure is
// the pool's own record — its images or done records, its failure records,
// its builds in memory, and the containers or machines it made — never what a
// box says of itself, but for a build's progress: the build's own word, which
// nothing acts on (ADR 26-11-23-004 §3). The JSON is the resource report's `snapshots` and the
// state channel's `startedFrom` as server.yaml has them.

// SnapshotReport is what the pool holds, builds and has failed to build. Its
// lists are never nil: an empty report is a pool that has none, which a
// reader tells from one that did not report.
type SnapshotReport struct {
	Held     []HeldSnapshot     `json:"held"`
	Building []BuildingSnapshot `json:"building"`
	Failed   []FailedSnapshot   `json:"failed"`
}

// HeldSnapshot is a snapshot the pool would start a box from.
type HeldSnapshot struct {
	Key string `json:"key"`
	// Plan is absent for a snapshot built before its record kept one.
	Plan    *SnapshotPlanName `json:"plan,omitempty"`
	BuiltAt time.Time         `json:"builtAt"`
	// SizeBytes is absent where the pool cannot measure it: a machine pool's
	// driver does not say how large a committed VM is.
	SizeBytes *int64 `json:"sizeBytes,omitempty"`
	// LastUsedAt is absent where the pool does not keep it: a Docker pool.
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	// Boxes is how many of the pool's boxes have a container or machine made
	// from it now.
	Boxes int64 `json:"boxes"`
}

// BuildingSnapshot is a build asked for and not yet ended.
type BuildingSnapshot struct {
	Key         string           `json:"key"`
	Plan        SnapshotPlanName `json:"plan"`
	RequestedAt time.Time        `json:"requestedAt"`
	// StartedAt is absent while it waits for another build to end.
	StartedAt *time.Time `json:"startedAt,omitempty"`
	// Box is what it runs in, as its runtime names it: a container's name
	// or a machine's. Absent until its box is up.
	Box string `json:"box,omitempty"`
	// Progress is the latest status it reported. Absent until it has, and
	// from a build whose agent reports none.
	Progress *SnapshotProgress `json:"progress,omitempty"`
}

// SnapshotProgress is a build's provisioning status, as it reported it (ADR
// 26-11-23-004 §1): the build's own word, which the pool and the server
// only show. Every string in it was written by the build's box.
type SnapshotProgress struct {
	State    string                             `json:"state"`
	Error    string                             `json:"error,omitempty"`
	Features map[string]SnapshotFeatureProgress `json:"features,omitempty"`
}

// SnapshotFeatureProgress is one feature's line of a build's progress.
type SnapshotFeatureProgress struct {
	State string `json:"state"`
	// Step is the step running now.
	Step  string `json:"step,omitempty"`
	Error string `json:"error,omitempty"`
}

// clone is a copy of p that shares nothing with it.
func (p *SnapshotProgress) clone() *SnapshotProgress {
	if p == nil {
		return nil
	}
	out := *p
	out.Features = maps.Clone(p.Features)
	return &out
}

// FailedSnapshot is a plan whose last build failed within
// snapshotRetryAfter. Plan and Error are absent from a failure recorded
// before the record kept them.
type FailedSnapshot struct {
	Key        string            `json:"key"`
	Plan       *SnapshotPlanName `json:"plan,omitempty"`
	FailedAt   time.Time         `json:"failedAt"`
	RetryAfter time.Time         `json:"retryAfter"`
	Error      string            `json:"error,omitempty"`
	// Progress is the last status the build reported before it failed:
	// which feature failed, and at which step. Absent from a failure
	// recorded before it was kept, and from a build that reported none.
	Progress *SnapshotProgress `json:"progress,omitempty"`
}

// StartedFrom is what a sandbox's current container or machine was made
// from: the snapshot keyed Snapshot, or the base, provisioned from scratch,
// when Snapshot is empty. A *StartedFrom that is nil is a box the pool cannot
// say of, as a container made before it recorded it.
type StartedFrom struct {
	Snapshot string
}

// snapshotReport is the part of a report every runtime builds the same way —
// the builds and the failures — around held, which is the runtime's own.
func (r *sandboxCore) snapshotReport(held []HeldSnapshot) (*SnapshotReport, error) {
	failed, err := r.snapshotFailuresReport(time.Now())
	if err != nil {
		return nil, err
	}
	if held == nil {
		held = []HeldSnapshot{}
	}
	return &SnapshotReport{Held: held, Building: r.snapshotBuildsReport(), Failed: failed}, nil
}

// Snapshots of an in-memory pool: it builds none.
func (r *MemorySandboxRuntime) Snapshots(context.Context) (*SnapshotReport, error) {
	return &SnapshotReport{Held: []HeldSnapshot{}, Building: []BuildingSnapshot{}, Failed: []FailedSnapshot{}}, nil
}

// SnapshotBuildLogs of an in-memory pool: it builds none.
func (r *MemorySandboxRuntime) SnapshotBuildLogs(context.Context, string, SnapshotLogsRequest, io.Writer) error {
	return ErrSnapshotBuildNotFound
}

// SnapshotBuildExec of an in-memory pool: it builds none.
func (r *MemorySandboxRuntime) SnapshotBuildExec(context.Context, string, machine.ExecRequest) (machine.Process, context.Context, error) {
	return nil, nil, ErrSnapshotBuildNotFound
}
