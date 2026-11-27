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
	"unicode/utf8"

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
// so the next create of the plan finds it. The pool reports what it holds,
// builds and failed to build on its resource report, and each box what it
// was made from on the state channel (ADR 26-11-17-316); the control plane
// only shows them, and decides nothing from them.
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
	// snapshotErrorLimit bounds the error a failure record keeps, which can
	// end with the build guest's own output (ADR 26-11-17-316 §3).
	snapshotErrorLimit = 2048
)

// snapshotBuilds is the builds under way, and the creates holding a snapshot.
type snapshotBuilds struct {
	mu sync.Mutex
	// active is the builds asked for and not yet ended, by key.
	active map[string]*snapshotBuild
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

// snapshotBuild is one build asked for and not yet ended.
type snapshotBuild struct {
	// cancel ends it early: a cache clear does.
	cancel context.CancelFunc
	plan   SnapshotPlanName
	// requested is when a create asked for it; started, once it holds the
	// build lock, when it began. A build with no start is queued.
	requested, started time.Time
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
	Agent  string        `json:"agent"`
	Off    []string      `json:"off,omitempty"`
	Egress egress.Policy `json:"egress"`
	// Size is what a machine pool's build is made at, the size of the box
	// that asked for it, since a machine made from a snapshot keeps the
	// snapshot's size (ADR 26-11-23-445 §5). Nil is the driver's default, and
	// always nil on a Docker pool, whose image carries no limit (§4).
	Size      *MachineSize `json:"size,omitempty"`
	features  map[string]sandboxconfig.Feature
	delivered assetcache.Delivery
}

// SnapshotPlanName is a plan as a person reads it: what it provisions, which
// is what tells one key from another (ADR 26-11-17-316 §3). The project, the
// pool and the egress policy are in the key and not here. Every record a
// snapshot is kept by carries it.
type SnapshotPlanName struct {
	Base    string          `json:"base"`
	Agent   string          `json:"agent,omitempty"`
	Bundles agentlayer.Pins `json:"bundles,omitempty"`
	Off     []string        `json:"off,omitempty"`
	// Size is the size a machine snapshot was built at; absent is the
	// driver's default (ADR 26-11-23-445 §5).
	Size *MachineSize `json:"size,omitempty"`
}

// name is how the plan is reported.
func (p snapshotPlan) name() SnapshotPlanName {
	name := SnapshotPlanName{Base: p.Base, Agent: p.Agent, Bundles: slices.Clone(p.Pins), Off: slices.Clone(p.Off)}
	if p.Size != nil {
		size := *p.Size
		name.Size = &size
	}
	return name
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
		b.active = map[string]*snapshotBuild{}
	}
	if _, building := b.active[key]; building || b.clearing > 0 || r.snapshotFailedRecently(key, time.Now()) {
		b.mu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), snapshotBuildTimeout)
	active := &snapshotBuild{cancel: cancel, plan: plan.name(), requested: time.Now().UTC()}
	b.active[key] = active
	b.mu.Unlock()
	built := r.background.Begin()
	go func() {
		defer built()
		defer cancel()
		var started time.Time
		err := func() error {
			b.one.Lock()
			defer b.one.Unlock()
			started = time.Now()
			b.mu.Lock()
			active.started = started.UTC()
			b.mu.Unlock()
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
			if recordErr := r.recordSnapshotFailure(key, time.Now(), err.Error(), &active.plan); recordErr != nil {
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
	for _, build := range b.active {
		build.cancel()
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
	for _, build := range r.snapshots.active {
		build.cancel()
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

// snapshotBuildsReport is the builds asked for and not yet ended, as the
// pool reports them, by key.
func (r *sandboxCore) snapshotBuildsReport() []BuildingSnapshot {
	r.snapshots.mu.Lock()
	defer r.snapshots.mu.Unlock()
	out := make([]BuildingSnapshot, 0, len(r.snapshots.active))
	for key, build := range r.snapshots.active {
		entry := BuildingSnapshot{Key: key, Plan: build.plan, RequestedAt: build.requested}
		if !build.started.IsZero() {
			started := build.started
			entry.StartedAt = &started
		}
		out = append(out, entry)
	}
	slices.SortFunc(out, func(a, b BuildingSnapshot) int { return a.RequestedAt.Compare(b.RequestedAt) })
	return out
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

// snapshotFailure is a failure record: when the plan's build last failed,
// why, and what the plan is (ADR 26-11-17-316 §3).
type snapshotFailure struct {
	FailedAt time.Time         `json:"failedAt"`
	Error    string            `json:"error,omitempty"`
	Plan     *SnapshotPlanName `json:"plan,omitempty"`
}

// readSnapshotFailure reads the failure record at path: the JSON document,
// or the bare time a pool wrote before it, which reads as a failure with no
// reason and no plan.
func readSnapshotFailure(path string) (snapshotFailure, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return snapshotFailure{}, false
	}
	var record snapshotFailure
	if json.Unmarshal(data, &record) == nil && !record.FailedAt.IsZero() {
		return record, true
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	if err != nil {
		return snapshotFailure{}, false
	}
	return snapshotFailure{FailedAt: at}, true
}

// snapshotFailedRecently reports whether the plan keyed key failed to build
// within snapshotRetryAfter of now.
func (r *sandboxCore) snapshotFailedRecently(key string, now time.Time) bool {
	record, ok := readSnapshotFailure(r.snapshotFailurePath(key))
	return ok && now.Sub(record.FailedAt) < snapshotRetryAfter
}

// recordSnapshotFailure records that the build of plan, keyed key, failed at
// at for reason, which is kept bounded. A nil plan is one the pool no longer
// knows.
func (r *sandboxCore) recordSnapshotFailure(key string, at time.Time, reason string, plan *SnapshotPlanName) error {
	path := r.snapshotFailurePath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(snapshotFailure{FailedAt: at.UTC(), Error: boundSnapshotError(reason), Plan: plan})
	if err != nil {
		return err
	}
	r.snapshots.records.Lock()
	defer r.snapshots.records.Unlock()
	return fsdurable.WriteFile(path, data, 0o600)
}

// boundSnapshotError keeps reason within snapshotErrorLimit bytes, cut on a
// rune boundary: its head says which step failed, and its end what the
// build's output said, so the middle is what goes.
func boundSnapshotError(reason string) string {
	reason = strings.ToValidUTF8(reason, "\uFFFD")
	if len(reason) <= snapshotErrorLimit {
		return reason
	}
	const elided = " … "
	half := (snapshotErrorLimit - len(elided)) / 2
	head, tail := half, len(reason)-half
	for head > 0 && !utf8.RuneStart(reason[head]) {
		head--
	}
	for tail < len(reason) && !utf8.RuneStart(reason[tail]) {
		tail++
	}
	return reason[:head] + elided + reason[tail:]
}

// snapshotFailuresReport is the plans whose last build failed within
// snapshotRetryAfter of now, as the pool reports them.
func (r *sandboxCore) snapshotFailuresReport(now time.Time) ([]FailedSnapshot, error) {
	dir := filepath.Join(r.root.PoolData(r.projectID, r.poolID), snapshotFailuresName)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []FailedSnapshot{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []FailedSnapshot{}
	for _, entry := range entries {
		if fsdurable.IsStaged(entry.Name()) {
			continue
		}
		record, ok := readSnapshotFailure(filepath.Join(dir, entry.Name()))
		if !ok || now.Sub(record.FailedAt) >= snapshotRetryAfter {
			continue
		}
		out = append(out, FailedSnapshot{
			Key:        entry.Name(),
			Plan:       record.Plan,
			FailedAt:   record.FailedAt.UTC(),
			RetryAfter: record.FailedAt.Add(snapshotRetryAfter).UTC(),
			Error:      record.Error,
		})
	}
	slices.SortFunc(out, func(a, b FailedSnapshot) int { return b.FailedAt.Compare(a.FailedAt) })
	return out, nil
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
	// Behind an edge, the edge as it is now: the address the build's bridges
	// dial, so they resolve nothing their fence closes, and the credential
	// the edge takes (ADR 26-11-16-619 §3). A build's plan is delivered once
	// and never renewed, so one the pool cannot complete yet is refused
	// rather than started to reach nothing — nor with a credential that may
	// expire before it ends: refused, it is a failed build, built again later.
	var edge *sandboxconfig.RuntimeEdge
	if r.poolEdge != nil {
		edge = r.poolEdge()
		switch {
		case edge.Address == "":
			return nil, agentlayer.Agent{}, errors.New("the pool's host has not resolved yet, so the build could not reach the pool through its edge")
		case edge.Token == "":
			return nil, agentlayer.Agent{}, errors.New("the pool holds no credential for its edge, so the edge would refuse the build's tunnels")
		case edge.Expires.Before(time.Now().Add(snapshotBuildTimeout)):
			return nil, agentlayer.Agent{}, fmt.Errorf("the pool's credential for its edge expires at %s, before the longest build would end, and the server has not renewed it", edge.Expires.Format(time.RFC3339))
		}
	}
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
	// The pool's sandboxes' hop, all of it — on a machine pool its address
	// and resolver are what reach the pool at all (#1197) — less the
	// credentials endpoint and the builder's: a build asks for no
	// credentials and builds nothing through the pool's builder.
	endpoints := r.poolEndpoints
	endpoints.Credentials, endpoints.BuildKit = "", ""
	proxy := material.Proxy
	proxy.Edge = edge
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
