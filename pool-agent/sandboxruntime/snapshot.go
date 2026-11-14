package sandboxruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/vm/pkg/machine"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/fsdurable"
	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pool-agent/assetcache"
	"github.com/discobox-ai/discobox/pool-agent/provisioner"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
	"github.com/discobox-ai/discobox/proxy/egress"
)

// A snapshot is a box provisioned for no sandbox, kept so a sandbox wanting
// the same plan starts from it (ADR 26-10-16-423 §8, ADR 26-11-03-986). Every
// runtime builds one the same way: it starts its base with the agent placed
// and a snapshot plan (agentlayer.SnapshotPlan) where a sandbox's bootstrap
// would be, runs the agent's snapshot command in it as one blocking exec —
// which provisions with every step blocking, checks every feature is done,
// and cleans and seals the box — then stops the box in order and commits it.
// On a Docker pool that is an image committed from a build container
// (snapshotdocker.go); on a machine pool whose driver commits, a committed
// disco-vm VM (machinesnapshot.go). It is only ever a cache: a delivered
// create finding no snapshot for its plan starts from the base and provisions
// from scratch, as before, and asks for the snapshot to be built behind it,
// so the next create of the plan finds it. The control plane never knows
// which a box started from.
//
// A build is anonymous. Its proxy identity is its own (layout.SnapshotIDPrefix), its
// egress is the create's provisionEgress — every level but the sandbox's own
// — and its plan names no sandbox, account, source, secret, gateway or
// credentials endpoint, so nothing of the create that asked for it is in it.

const (
	// snapshotBuildTimeout bounds a build, which provisions everything a
	// fresh box does in the background — Docker, Chromium, Nix, Go,
	// Homebrew — with every step blocking.
	snapshotBuildTimeout = 45 * time.Minute
	// snapshotRetryAfter is how long a plan whose build failed waits before
	// a create asks for it again.
	snapshotRetryAfter = time.Hour
	// snapshotStopTimeout is how long a build's box gets to shut down in
	// order before it is forced off.
	snapshotStopTimeout = time.Minute
	// snapshotFailuresName is the directory, in the pool's durable state,
	// that holds when each plan's build last failed, by key. It is on disk so
	// an agent that keeps restarting does not keep building (ADR 26-11-03-986
	// §5).
	snapshotFailuresName = "snapshot-failures"
)

// snapshotBuilds is the builds under way, and the creates holding a snapshot.
type snapshotBuilds struct {
	mu sync.Mutex
	// active is the builds asked for and not yet ended, by key, each with
	// what ends it early: a cache clear does.
	active map[string]context.CancelFunc
	// using counts, by key, the creates making a box from that plan's
	// snapshot: nothing removes one while a create reads it.
	using map[string]int
	// clearing counts the clears under way, which no build starts during.
	clearing int
	// one runs a build at a time: each is as heavy as a fresh box. A sweep
	// holds it too, so nothing a build makes appears while a sweep decides
	// what is left over, and a clear waits on it for the build it ended.
	one sync.Mutex
	// records serializes writes of the records a snapshot is kept by.
	records sync.Mutex
}

// errSnapshotHeld ends a build that would replace a snapshot a create is
// still making a box from. It is not the plan's failure.
var errSnapshotHeld = errors.New("a create is still making a box from the snapshot it would replace")

// snapshotPlan is what a delivered create is provisioned with, as a snapshot
// is keyed: everything a snapshot of it holds depends on, and nothing of the
// sandbox.
type snapshotPlan struct {
	// Base is what a build starts from: the sandbox base image's ID on a
	// Docker pool, the driver and its configured base on a machine pool.
	Base    string          `json:"base"`
	Project string          `json:"project"`
	Pool    string          `json:"pool"`
	Pins    agentlayer.Pins `json:"pins"`
	// Agent is the agent bundle placed in the build, which the snapshot
	// holds (ADR 26-10-24-953 §2).
	Agent     string        `json:"agent"`
	Off       []string      `json:"off,omitempty"`
	Egress    egress.Policy `json:"egress"`
	features  map[string]sandboxconfig.Feature
	delivered assetcache.Delivery
}

// Key is the plan's digest's hex: what its snapshot is found by.
func (p snapshotPlan) Key() string {
	data, err := json.Marshal(p)
	if err != nil {
		panic(err) // A struct of strings and slices always marshals.
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// buildID is the build's identity, which its proxy client certificate names.
func (p snapshotPlan) buildID() string { return snapshotBuildID(p.Key()) }

// snapshotBuildID is the build identity of the plan keyed key.
func snapshotBuildID(key string) string { return layout.SnapshotIDPrefix + key[:24] }

// newSnapshotPlan is the plan of a delivered create on base.
func (r *sandboxCore) newSnapshotPlan(base string, req *workerapimodel.PoolSandboxCreateRequest) (snapshotPlan, error) {
	delivered := delivery(req)
	pins, _, err := applied(delivered)
	if err != nil {
		return snapshotPlan{}, err
	}
	plan := snapshotPlan{
		Base:      base,
		Project:   r.projectID,
		Pool:      r.poolID,
		Pins:      pins,
		Agent:     delivered.Agent.Digest,
		Egress:    egress.Merge(egressPolicy(req.ProvisionEgress)),
		features:  map[string]sandboxconfig.Feature{},
		delivered: delivered,
	}
	if features, ok := req.Config.Features.Get(); ok {
		for _, name := range slices.Sorted(maps.Keys(features)) {
			plan.features[name] = sandboxconfig.Feature{Enabled: features[name].Enabled}
			if !features[name].Enabled {
				plan.Off = append(plan.Off, name)
			}
		}
	}
	return plan, nil
}

// requestSnapshot builds plan's snapshot in the background with build,
// unless one is being built or failed within snapshotRetryAfter.
func (r *sandboxCore) requestSnapshot(plan snapshotPlan, build func(context.Context, snapshotPlan) error) {
	key := plan.Key()
	b := &r.snapshots
	b.mu.Lock()
	if b.active == nil {
		b.active = map[string]context.CancelFunc{}
	}
	if _, building := b.active[key]; building || b.clearing > 0 || r.snapshotFailedRecently(key, time.Now()) {
		b.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), snapshotBuildTimeout)
	b.active[key] = cancel
	b.mu.Unlock()
	go func() {
		defer cancel()
		started := time.Now()
		err := func() error {
			b.one.Lock()
			defer b.one.Unlock()
			slog.Info("building a snapshot", "snapshot", key)
			return build(ctx, plan)
		}()
		// The failure is recorded before the build stops being one, so a
		// create in between neither builds it again nor finds it failed.
		switch {
		case errors.Is(ctx.Err(), context.Canceled), errors.Is(err, errSnapshotHeld):
			// Ended by a clear, or by a create holding what it would
			// replace: the next create asks again.
		case err != nil:
			if recordErr := r.recordSnapshotFailure(key, time.Now()); recordErr != nil {
				slog.Warn("record a failed snapshot build", "snapshot", key, "error", recordErr)
			}
		default:
			if forgetErr := r.forgetSnapshotFailure(key); forgetErr != nil {
				slog.Warn("forget a snapshot build's failure", "snapshot", key, "error", forgetErr)
			}
		}
		b.mu.Lock()
		delete(b.active, key)
		b.mu.Unlock()
		if err != nil {
			slog.Warn("build a snapshot; its plan provisions from scratch", "snapshot", key, "error", err, "retryAfter", snapshotRetryAfter)
			return
		}
		slog.Info("built a snapshot", "snapshot", key, "took", time.Since(started).Round(time.Second))
	}()
}

// holdSnapshotClear ends every build asked for and starts none until the
// returned release, then waits out the build it ended, for a clear that
// drops what builds publish.
func (r *sandboxCore) holdSnapshotClear() func() {
	b := &r.snapshots
	b.mu.Lock()
	b.clearing++
	for _, cancel := range b.active {
		cancel()
	}
	b.mu.Unlock()
	b.one.Lock()
	return func() {
		b.one.Unlock()
		b.mu.Lock()
		b.clearing--
		b.mu.Unlock()
	}
}

// cancelSnapshotBuilds ends every build asked for, as a cache clear does.
func (r *sandboxCore) cancelSnapshotBuilds() {
	r.snapshots.mu.Lock()
	defer r.snapshots.mu.Unlock()
	for _, cancel := range r.snapshots.active {
		cancel()
	}
}

// lockSnapshotSweep takes the build lock for a sweep, so no build makes
// anything while it decides what is left over, and reports false while a
// build holds it: what a build under way has made is not left over, and the
// next pass sweeps. The caller unlocks r.snapshots.one when it reports true.
func (r *sandboxCore) lockSnapshotSweep() bool { return r.snapshots.one.TryLock() }

// buildingSnapshots is the keys of the builds asked for and not yet ended.
func (r *sandboxCore) buildingSnapshots() map[string]bool {
	r.snapshots.mu.Lock()
	defer r.snapshots.mu.Unlock()
	keys := make(map[string]bool, len(r.snapshots.active))
	for key := range r.snapshots.active {
		keys[key] = true
	}
	return keys
}

// holdSnapshot marks a create making a box from the snapshot keyed key, and
// returns what releases it.
func (r *sandboxCore) holdSnapshot(key string) func() {
	b := &r.snapshots
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.using == nil {
		b.using = map[string]int{}
	}
	b.using[key]++
	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.using[key]--; b.using[key] <= 0 {
			delete(b.using, key)
		}
	}
}

// snapshotInUse reports whether a create holds the snapshot keyed key.
func (r *sandboxCore) snapshotInUse(key string) bool {
	r.snapshots.mu.Lock()
	defer r.snapshots.mu.Unlock()
	return r.snapshots.using[key] > 0
}

// snapshotHeldBy reports whether a create holds a snapshot whose key names
// match: a sweep knows a VM or build by its name, which holds only a key's
// prefix.
func (r *sandboxCore) snapshotHeldBy(match func(key string) bool) bool {
	r.snapshots.mu.Lock()
	defer r.snapshots.mu.Unlock()
	for key, n := range r.snapshots.using {
		if n > 0 && match(key) {
			return true
		}
	}
	return false
}

// snapshotFailurePath holds when the build of the plan keyed key last failed.
func (r *sandboxCore) snapshotFailurePath(key string) string {
	return filepath.Join(r.root.PoolData(r.projectID, r.poolID), snapshotFailuresName, key)
}

// snapshotFailedRecently reports whether the plan keyed key failed to build
// within snapshotRetryAfter of now.
func (r *sandboxCore) snapshotFailedRecently(key string, now time.Time) bool {
	at, ok := readSandboxTombstone(r.snapshotFailurePath(key))
	return ok && now.Sub(at) < snapshotRetryAfter
}

func (r *sandboxCore) recordSnapshotFailure(key string, at time.Time) error {
	path := r.snapshotFailurePath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	r.snapshots.records.Lock()
	defer r.snapshots.records.Unlock()
	return fsdurable.WriteFile(path, []byte(at.UTC().Format(time.RFC3339)+"\n"), 0o600)
}

func (r *sandboxCore) forgetSnapshotFailure(key string) error {
	if err := os.Remove(r.snapshotFailurePath(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// sweepSnapshotFailures forgets the failures past snapshotRetryAfter, which
// hold nothing back any longer.
func (r *sandboxCore) sweepSnapshotFailures(now time.Time) {
	dir := filepath.Join(r.root.PoolData(r.projectID, r.poolID), snapshotFailuresName)
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if !r.snapshotFailedRecently(entry.Name(), now) {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

// forgetSnapshotFailures forgets every plan that failed to build, as a cache
// clear does: a box that came up wrong from one starts from scratch next.
func (r *sandboxCore) forgetSnapshotFailures() error {
	return os.RemoveAll(filepath.Join(r.root.PoolData(r.projectID, r.poolID), snapshotFailuresName))
}

// snapshotsDir holds each build's tree (layout.Root.PoolSnapshots): its asset
// record, its registry namespace, and, on a Docker pool, the directory bound
// at agentlayer.SnapshotDir.
func (r *sandboxCore) snapshotsDir() string {
	return r.root.PoolSnapshots(r.projectID, r.poolID)
}

// prepareSnapshotBuild lays what the build of plan needs on the pool's side —
// its tree, with the asset record assets.discobox.internal answers it from,
// and its proxy material and egress policy — and returns its plan document,
// and the agent to place beside it. What it laid goes with
// removeSnapshotBuildMaterial, once the build's box is gone.
func (r *sandboxCore) prepareSnapshotBuild(ctx context.Context, plan snapshotPlan) ([]byte, agentlayer.Agent, error) {
	id := plan.buildID()
	dir := r.root.Snapshot(r.projectID, r.poolID, id)
	if err := os.RemoveAll(dir); err != nil {
		return nil, agentlayer.Agent{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, agentlayer.Agent{}, err
	}
	// What assets.discobox.internal answers the build, as a sandbox's record
	// is what it answers the sandbox.
	if err := assetcache.WriteRecord(dir, plan.delivered); err != nil {
		return nil, agentlayer.Agent{}, err
	}
	material, err := proxyagent.EnsureSandboxMaterial(r.root, r.projectID, r.poolID, id)
	if err != nil {
		return nil, agentlayer.Agent{}, err
	}
	if err := proxyagent.SetSandboxEgress(r.root, r.projectID, r.poolID, id, plan.Egress); err != nil {
		return nil, agentlayer.Agent{}, err
	}
	agent, ok, err := r.sandboxAgent(ctx, id)
	if err != nil {
		return nil, agentlayer.Agent{}, fmt.Errorf("the agent of pins %v: %w", plan.Pins, err)
	}
	if !ok {
		return nil, agentlayer.Agent{}, errors.New("this pool has no asset cache to place the agent from")
	}
	// The proxy and DNS alone: a build asks for no credentials and builds
	// nothing through the pool's builder.
	endpoints := sandboxconfig.PoolEndpoints{Proxy: r.poolEndpoints.Proxy, DNS: r.poolEndpoints.DNS, ServerName: r.poolEndpoints.ServerName}
	proxy := material.Proxy
	doc := agentlayer.SnapshotPlan{
		ID:            id,
		ProjectID:     r.projectID,
		PoolID:        r.poolID,
		Pool:          endpoints,
		Env:           material.Env,
		RuntimeConfig: sandboxconfig.RuntimeConfig{Revision: 1, Proxy: &proxy},
		Features:      plan.features,
	}
	if err := doc.Validate(); err != nil {
		return nil, agentlayer.Agent{}, err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, agentlayer.Agent{}, err
	}
	return data, agent, nil
}

// removeSnapshotBuildMaterial removes what the pool laid for the build id: its
// egress policy and proxy material, then its tree, each only once what goes
// before it is gone. Its caller has removed the build's box first, or seen it
// committed, which never runs again: an identity with no egress entry is
// unrestricted. The tree goes last, since it is what the next sweep finds a
// build by to try again.
func (r *sandboxCore) removeSnapshotBuildMaterial(id string) error {
	if err := errors.Join(
		proxyagent.RemoveSandboxPolicy(r.root, r.projectID, r.poolID, id),
		proxyagent.RemoveSandboxMaterial(r.root, r.projectID, r.poolID, id),
	); err != nil {
		return fmt.Errorf("remove build %s's proxy record: %w", id, err)
	}
	return os.RemoveAll(r.root.Snapshot(r.projectID, r.poolID, id))
}

// snapshotCommand is the agent's snapshot command, as a build runs it.
var snapshotCommand = []string{agentlayer.AgentPath, agentlayer.SnapshotCommand}

// runSnapshotCommand is a build's one imaging exec, on every runtime (ADR
// 26-11-03-986 §1): it places the agent, its units and pins in the started
// box and runs the snapshot command there, its plan on standard input — never
// on the box's disk, since the plan holds the build's client key. Its exit
// status, and the end of its stderr, are the build's result.
func runSnapshotCommand(ctx context.Context, guest machine.Guest, agent agentlayer.Agent, pins agentlayer.Pins, plan []byte) error {
	reader, writer := io.Pipe()
	go func() { writer.CloseWithError(agentlayer.Archive(writer, agent, false, pins, agentlayer.Units)) }()
	defer reader.Close()
	result, err := provisioner.Provision(ctx, guest, provisioner.Plan{
		Dir:     "/",
		Tree:    reader,
		Command: machine.ExecRequest{Argv: snapshotCommand},
		Stdin:   bytes.NewReader(plan),
	})
	if exit := (*provisioner.ExitError)(nil); errors.As(err, &exit) {
		if tail := strings.TrimSpace(string(result.Stderr)); tail != "" {
			return fmt.Errorf("its provisioning failed (status %d): %s", exit.ExitCode, tail)
		}
		return fmt.Errorf("its provisioning failed with status %d", exit.ExitCode)
	}
	if err != nil {
		return fmt.Errorf("run the build's provisioning: %w", err)
	}
	return nil
}
