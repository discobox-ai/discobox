package agentlayer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

// A snapshot is a box provisioned for no sandbox, kept so a box wanting the
// same plan starts from it (ADR 26-10-16-423 §8). Whatever takes one — a
// pool's Docker runtime commits a container; a machine provider would take its
// own kind of snapshot — boots the provider's image with the agent placed as
// Archive places it, and puts a SnapshotPlan at SnapshotPlanFile instead of a
// sandbox's bootstrap. The agent's snapshot command, started by
// discobox-snapshot.service, then provisions the platform and harness its pins
// name with every step blocking, leaves out what a feature keeps out of a
// snapshot, cleans the box of what is any one machine's, writes a
// SnapshotResult to SnapshotResultFile, and powers the box off. The taker
// reads the result, takes everything under SnapshotDir out of the box — a
// Docker runtime binds it, so it is never in the container's own filesystem
// — and snapshots what is left.
//
// The plan is anonymous: it names no sandbox, account, source, secret or
// credential, and its proxy identity is the build's own, so nothing of the
// sandbox whose create asked for it reaches the snapshot.
const (
	// SnapshotDir holds the plan, the result, and what the plan's runtime
	// config lays: the directory a sandbox's bootstrap is in.
	SnapshotDir = sandboxconfig.SandboxConfigDir
	// SnapshotPlanFile is the plan. A box with it is taking a snapshot, and
	// one with none never is.
	SnapshotPlanFile = SnapshotDir + "/snapshot.json"
	// SnapshotResultFile is the result, written last.
	SnapshotResultFile = SnapshotDir + "/snapshot-result.json"
	// SnapshotStateFile is where the snapshot's intake keeps the runtime
	// config it applied: under SnapshotDir, never on the root filesystem the
	// snapshot is taken of.
	SnapshotStateFile = SnapshotDir + "/runtime-config.json"
)

// SnapshotPlan is what a snapshot provisions, and how it reaches its pool.
type SnapshotPlan struct {
	// ID is the build's own identity, which its proxy client certificate
	// names: never a sandbox's.
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	PoolID    string `json:"poolId"`
	// Pool is where the pool serves the build: its proxy and DNS alone, so
	// the build has no credentials endpoint to ask.
	Pool sandboxconfig.PoolEndpoints `json:"pool"`
	// Env is the proxy-trust environment the build provisions under, as a
	// sandbox's bootstrap names it in Env and ProxyEnvs.
	Env map[string]string `json:"env,omitempty"`
	// RuntimeConfig carries the build's proxy material, applied by the
	// agent's intake as a delivery would be. It holds nothing else.
	RuntimeConfig sandboxconfig.RuntimeConfig `json:"runtimeConfig"`
	// Features turns features off by name, as a sandbox's bootstrap does; it
	// is part of what the snapshot is keyed by.
	Features map[string]sandboxconfig.Feature `json:"features,omitempty"`
}

// Validate refuses a plan that is not anonymous or names no pool to reach.
func (p SnapshotPlan) Validate() error {
	switch {
	case strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.ProjectID) == "" || strings.TrimSpace(p.PoolID) == "":
		return errors.New("snapshot plan: id, projectId and poolId are required")
	case p.Pool.Proxy == "":
		return errors.New("snapshot plan: names no pool proxy")
	case p.Pool.Credentials != "":
		return errors.New("snapshot plan: names a credentials endpoint, and a snapshot is built with no credentials")
	case p.RuntimeConfig.Proxy == nil:
		return errors.New("snapshot plan: carries no proxy material")
	case len(p.RuntimeConfig.SecretEnv) > 0 || len(p.RuntimeConfig.Sources) > 0:
		return errors.New("snapshot plan: its runtime config carries secrets or sources, and a snapshot is built for no sandbox")
	}
	return nil
}

// ParseSnapshotPlan reads SnapshotPlanFile.
func ParseSnapshotPlan(data []byte) (SnapshotPlan, error) {
	var plan SnapshotPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return SnapshotPlan{}, fmt.Errorf("decode %s: %w", SnapshotPlanFile, err)
	}
	return plan, plan.Validate()
}

// SnapshotResult is how a snapshot's provisioning ended: an empty Error is a
// box ready to be snapshotted.
type SnapshotResult struct {
	Error string `json:"error,omitempty"`
}
