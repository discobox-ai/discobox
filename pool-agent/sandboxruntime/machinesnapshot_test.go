package sandboxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"

	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
)

// machineSnapshotCreate is a create a snapshot can be built for: one with a
// build policy, as every current server sends.
func machineSnapshotCreate(sandboxID string) *workerapimodel.PoolSandboxCreateRequest {
	req := machineCreate(sandboxID)
	req.ProvisionEgress = workerclient.NewOptEgressPolicy(workerclient.EgressPolicy{})
	return req
}

// machineSnapshotKey is the key of req's plan on f's runtime.
func machineSnapshotKey(t *testing.T, f *machineFixture, req *workerapimodel.PoolSandboxCreateRequest) string {
	t.Helper()
	plan, err := f.runtime.newSnapshotPlan(f.runtime.snapshotBase(), req)
	if err != nil {
		t.Fatal(err)
	}
	return plan.Key()
}

// guestHas reports whether the running sandbox's machine has a file at path.
func guestHas(t *testing.T, f *machineFixture, sandboxID, path string) bool {
	t.Helper()
	ctx := context.Background()
	inst, err := f.runtime.instance(ctx, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := f.runtime.engine.Guest(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	code, err := machine.Run(ctx, guest, machine.ExecRequest{Argv: []string{"sh", "-c", `test -e "$DISCO_VM_GUEST_ROOT` + path + `"`}}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return code == 0
}

// guestGreps reports whether any file of the running sandbox's machine holds
// text.
func guestGreps(t *testing.T, f *machineFixture, sandboxID, text string) bool {
	t.Helper()
	ctx := context.Background()
	inst, err := f.runtime.instance(ctx, sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := f.runtime.engine.Guest(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	code, err := machine.Run(ctx, guest, machine.ExecRequest{Argv: []string{"sh", "-c", `grep -rqs -- "$1" "$DISCO_VM_GUEST_ROOT"`, "sh", text}}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return code == 0
}

// A machine pool whose driver commits makes the first sandbox of a plan
// fresh and builds the plan's snapshot behind it: a VM of the plan's name,
// provisioned by the snapshot command, stopped, committed and recorded, with
// nothing of the build left. The next sandbox of the plan is made from it
// (ADR 26-11-03-986 §§2, 4).
func TestAMachinePoolMakesTheNextSandboxFromItsPlansSnapshot(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	first := machineSnapshotCreate("sbx_1")
	key := machineSnapshotKey(t, f, first)
	if _, err := r.CreateSandbox(ctx, first); err != nil {
		t.Fatal(err)
	}
	if record, _, _ := r.readMachineRecord("sbx_1"); record.Snapshot != "" {
		t.Fatalf("the plan's first sandbox was made from snapshot %q, want fresh", record.Snapshot)
	}
	waitForSnapshotBuilds(t, r)

	record, ok, err := r.readSnapshotRecord(key)
	if err != nil || !ok {
		t.Fatalf("snapshot record = %v, %v; want the build recorded", ok, err)
	}
	// The build's machine is made running, booted once, and never started
	// apart from its create: the sandbox's create and the build's are the
	// two asked to start.
	if got, want := f.driver.takeEvents(snapshotMachineName(key)), []string{"boot"}; !slices.Equal(got, want) {
		t.Fatalf("the build's machine = %v, want %v", got, want)
	}
	if got := f.driver.createdStarted.Load(); got != 2 {
		t.Fatalf("creates asked to start = %d, want the sandbox's and the build's", got)
	}
	snap, err := f.owner.Instance(ctx, snapshotMachineName(key))
	if err != nil || snap.ID != record.Instance {
		t.Fatalf("snapshot machine %+v, %v; want the one its record names, %s", snap, err, record.Instance)
	}
	id := snapshotBuildID(key)
	for _, path := range []string{r.root.Snapshot("proj_1", "pool_1", id), filepath.Join(proxyagent.PoolSandboxMaterialRoot(r.root, "proj_1", "pool_1"), id)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the build left %s: %v", path, err)
		}
	}
	if r.snapshotFailedRecently(key, time.Now()) {
		t.Error("a build that succeeded is recorded as failed")
	}

	if _, err := r.CreateSandbox(ctx, machineSnapshotCreate("sbx_2")); err != nil {
		t.Fatal(err)
	}
	second, _, err := r.readMachineRecord("sbx_2")
	if err != nil || second.Snapshot != key || !r.snapshotProven("sbx_2") {
		t.Fatalf("the plan's next sandbox: snapshot %q, proven %v, %v; want %s, its agent having answered", second.Snapshot, r.snapshotProven("sbx_2"), err, key)
	}
	inst, err := f.owner.Instance(ctx, "sbx-2")
	if err != nil || inst.From != snapshotMachineName(key) {
		t.Fatalf("sbx_2's machine %+v, %v; want one made from the snapshot", inst, err)
	}
	// What the build provisioned is on its disk; the plan, and the client
	// key in it, never were: it came on the command's standard input.
	if !guestHas(t, f, "sbx_2", "/provisioned") {
		t.Error("the sandbox made from the snapshot does not hold what was provisioned")
	}
	if guestGreps(t, f, "sbx_2", "PRIVATE KEY") {
		t.Error("a key is on the disk of the sandbox made from the snapshot")
	}
	if agent := f.agents.get(t, "sbx_2"); len(agent.revisions()) == 0 {
		t.Error("the sandbox made from the snapshot was delivered no runtime config")
	}

	// The pool reports the snapshot by its plan, with the box made from it,
	// and each box what it was made from (ADR 26-11-17-316).
	report, err := r.Snapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Held) != 1 || len(report.Building) != 0 || len(report.Failed) != 0 {
		t.Fatalf("snapshots = %+v, want the one held", report)
	}
	held := report.Held[0]
	if held.Key != key || held.Boxes != 1 || held.Plan == nil || held.LastUsedAt == nil || held.SizeBytes != nil {
		t.Errorf("held = %+v, want %s by its plan, its use, no size, and one box", held, key)
	}
	if held.Plan != nil && (held.Plan.Base != r.snapshotBase() || len(held.Plan.Bundles) != 1 || held.Plan.Agent == "") {
		t.Errorf("held plan = %+v", *held.Plan)
	}
	for sandboxID, want := range map[string]string{"sbx_1": "", "sbx_2": key} {
		sb, err := r.GetSandbox(ctx, sandboxID)
		if err != nil || sb.StartedFrom == nil || sb.StartedFrom.Snapshot != want {
			t.Errorf("%s started from %+v, %v; want snapshot %q", sandboxID, sb.StartedFrom, err, want)
		}
	}
}

// A driver that does not report Commit gets no snapshot: every sandbox is
// made fresh, and no build is ever asked for (ADR 26-11-03-986 §2).
func TestAMachinePoolOnADriverThatDoesNotCommitBuildsNoSnapshot(t *testing.T) {
	f := newMachineFixture(t, json.RawMessage(`{"noCommit":true}`))
	ctx := context.Background()
	r := f.runtime
	if r.commits() {
		t.Fatal("the fake driver reports Commit with it turned off")
	}
	for _, id := range []string{"sbx_1", "sbx_2"} {
		if _, err := r.CreateSandbox(ctx, machineSnapshotCreate(id)); err != nil {
			t.Fatal(err)
		}
		if building := r.buildingSnapshots(); len(building) != 0 {
			t.Fatalf("asked for builds %v on a driver that does not commit", building)
		}
	}
	instances, err := f.owner.Instances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, inst := range instances {
		if strings.HasPrefix(inst.Name, snapshotMachinePrefix) || inst.From != "" {
			t.Errorf("machine %+v on a driver that does not commit", inst)
		}
	}
}

// A build whose provisioning fails publishes nothing and leaves nothing: its
// VM is destroyed, its proxy record and tree removed, and its plan waits
// before it is built again — on disk, so a restarted agent waits too.
func TestAFailedMachineSnapshotBuildLeavesNothing(t *testing.T) {
	f := newMachineFixture(t, nil)
	f.agents.snapshotFails = true
	ctx := context.Background()
	r := f.runtime
	req := machineSnapshotCreate("sbx_1")
	key := machineSnapshotKey(t, f, req)
	if _, err := r.CreateSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	waitForSnapshotBuilds(t, r)
	if _, ok, _ := r.readSnapshotRecord(key); ok {
		t.Fatal("a failed build was recorded as a snapshot")
	}
	if _, err := f.owner.Instance(ctx, snapshotMachineName(key)); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("a failed build left its machine: %v", err)
	}
	id := snapshotBuildID(key)
	if _, err := os.Stat(r.root.Snapshot("proj_1", "pool_1", id)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a failed build left its tree: %v", err)
	}
	if !r.snapshotFailedRecently(key, time.Now()) {
		t.Fatal("a failed build was not recorded as failed")
	}
	if _, err := r.CreateSandbox(ctx, machineSnapshotCreate("sbx_2")); err != nil {
		t.Fatal(err)
	}
	if building := r.buildingSnapshots(); len(building) != 0 {
		t.Fatal("asked again for a build that failed moments ago")
	}
}

// A build of a plan clears whatever VM has its name first, and the sweep
// destroys a snapshot VM no record names, drops the record of one whose VM
// is gone, reclaims one unused past its retention, record first, and keeps
// the rest, a machine whose name only begins like a snapshot's included
// (ADR 26-11-03-986 §§4, 5).
func TestTheMachineSnapshotSweepKeepsOnlyRecordedSnapshots(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	req := machineSnapshotCreate("sbx_1")
	key := machineSnapshotKey(t, f, req)
	name := snapshotMachineName(key)
	// A leftover of the plan's name, which a crashed build could have left.
	if _, err := r.engine.Create(ctx, engine.CreateOptions{Name: name}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	waitForSnapshotBuilds(t, r)
	record, ok, err := r.readSnapshotRecord(key)
	if err != nil || !ok {
		t.Fatalf("a build over a leftover of its name: record %v, %v", ok, err)
	}

	unrecorded := snapshotMachineName(strings.Repeat("b", 64))
	// An operator's machine that only begins like a snapshot's is not one.
	for _, name := range []string{unrecorded, "snap-foo"} {
		if _, err := r.engine.Create(ctx, engine.CreateOptions{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	gone := strings.Repeat("d", 64)
	if err := r.writeSnapshotRecord(gone, snapshotRecord{Instance: "nothing", LastUsed: time.Now()}); err != nil {
		t.Fatal(err)
	}
	leftTree := snapshotBuildID(strings.Repeat("e", 64))
	if _, err := proxyagent.EnsureSandboxMaterial(r.root, "proj_1", "pool_1", leftTree); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.root.Snapshot("proj_1", "pool_1", leftTree), 0o755); err != nil {
		t.Fatal(err)
	}

	sweepMachineSnapshots(t, r, time.Now())
	if _, err := f.owner.Instance(ctx, unrecorded); !errors.Is(err, machine.ErrNotFound) {
		t.Errorf("a snapshot machine no record names was kept: %v", err)
	}
	if _, err := f.owner.Instance(ctx, "snap-foo"); err != nil {
		t.Errorf("a machine not named as a snapshot's was swept: %v", err)
	}
	if _, ok, _ := r.readSnapshotRecord(gone); ok {
		t.Error("the record of a snapshot whose machine is gone was kept")
	}
	if _, err := os.Stat(r.root.Snapshot("proj_1", "pool_1", leftTree)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a build's tree no build holds was kept: %v", err)
	}
	if inst, err := f.owner.Instance(ctx, name); err != nil || inst.ID != record.Instance {
		t.Fatalf("the recorded snapshot was not kept: %+v, %v", inst, err)
	}

	// Held by a create, it outlives its retention; released, it goes.
	release := r.holdSnapshot(key)
	sweepMachineSnapshots(t, r, time.Now().Add(snapshotRetention+time.Hour))
	if _, ok, _ := r.readSnapshotRecord(key); !ok {
		t.Fatal("a snapshot a create holds was reclaimed")
	}
	release()
	sweepMachineSnapshots(t, r, time.Now().Add(snapshotRetention+time.Hour))
	if _, ok, _ := r.readSnapshotRecord(key); ok {
		t.Error("a snapshot unused past its retention kept its record")
	}
	if _, err := f.owner.Instance(ctx, name); !errors.Is(err, machine.ErrNotFound) {
		t.Errorf("a snapshot unused past its retention kept its machine: %v", err)
	}
}

// A machine made from a snapshot whose agent never comes up drops the
// snapshot and is made again fresh, in the same create (ADR 26-11-03-986
// §6).
func TestASandboxWhoseSnapshotNeverComesUpIsMadeFresh(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	first := machineSnapshotCreate("sbx_1")
	key := machineSnapshotKey(t, f, first)
	if _, err := r.CreateSandbox(ctx, first); err != nil {
		t.Fatal(err)
	}
	waitForSnapshotBuilds(t, r)
	if _, ok, _ := r.readSnapshotRecord(key); !ok {
		t.Fatal("no snapshot was built")
	}

	f.agents.mu.Lock()
	f.agents.sealedBroken = true
	f.agents.mu.Unlock()
	r.agentReadyTimeout = 2 * time.Second
	sb, err := r.CreateSandbox(ctx, machineSnapshotCreate("sbx_2"))
	if err != nil {
		t.Fatalf("a create whose snapshot never came up: %v", err)
	}
	if sb.Status != StatusRunning {
		t.Fatalf("sbx_2 is %s, want running", sb.Status)
	}
	record, _, err := r.readMachineRecord("sbx_2")
	if err != nil || record.Snapshot != "" || !record.Started {
		t.Fatalf("sbx_2's record %+v, %v; want a fresh machine, started", record, err)
	}
	if inst, err := f.owner.Instance(ctx, "sbx-2"); err != nil || inst.From != "" || inst.ID != record.Instance {
		t.Fatalf("sbx_2's machine %+v, %v; want the fresh one its record names", inst, err)
	}
	if _, ok, _ := r.readSnapshotRecord(key); ok {
		t.Error("the snapshot whose sandbox never came up is still recorded")
	}
	if _, err := f.owner.Instance(ctx, snapshotMachineName(key)); !errors.Is(err, machine.ErrNotFound) {
		t.Errorf("the snapshot whose sandbox never came up kept its machine: %v", err)
	}
	// It is reported as a failed plan, by its plan and why (ADR 26-11-17-316 §2).
	report, err := r.Snapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Held) != 0 || len(report.Failed) != 1 {
		t.Fatalf("snapshots = %+v, want the plan failed and none held", report)
	}
	if failed := report.Failed[0]; failed.Key != key || failed.Plan == nil || !strings.Contains(failed.Error, "did not come up") {
		t.Errorf("failed = %+v, want %s by its plan, saying its sandbox did not come up", failed, key)
	}
}

// A cache clear drops every snapshot, record first, and forgets the plans
// that failed, so the next create is fresh and builds again.
func TestAMachinePoolsCacheClearDropsItsSnapshots(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	req := machineSnapshotCreate("sbx_1")
	key := machineSnapshotKey(t, f, req)
	if _, err := r.CreateSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	waitForSnapshotBuilds(t, r)
	if err := r.recordSnapshotFailure("other", time.Now(), "it failed", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ClearCache(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := r.readSnapshotRecord(key); ok {
		t.Error("a clear kept a snapshot's record")
	}
	if _, err := f.owner.Instance(ctx, snapshotMachineName(key)); !errors.Is(err, machine.ErrNotFound) {
		t.Errorf("a clear kept a snapshot's machine: %v", err)
	}
	if r.snapshotFailedRecently("other", time.Now()) {
		t.Error("a clear kept a plan's failure")
	}
}

// A create from a snapshot that vanished under it is told so in the error
// ensureInstance reads to fall back to a fresh machine: the engine's own
// not-found, which it answers before the driver is asked.
func TestACreateFromAVanishedSnapshotSaysNotFound(t *testing.T) {
	f := newMachineFixture(t, nil)
	_, err := f.runtime.engine.Create(context.Background(), engine.CreateOptions{Name: "sbx-9", From: snapshotMachineName(strings.Repeat("a", 64))})
	if !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("create from a missing snapshot = %v, want engine.ErrNotFound", err)
	}
}

// A build never destroys a snapshot a create is still making a box from: it
// unpublishes it, so no create takes it after, and stops, which is not the
// plan's failure. The sweep destroys the VM once the create lets it go.
func TestABuildLeavesASnapshotACreateHolds(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	req := machineSnapshotCreate("sbx_1")
	key := machineSnapshotKey(t, f, req)
	if _, err := r.CreateSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	waitForSnapshotBuilds(t, r)
	record, ok, err := r.readSnapshotRecord(key)
	if err != nil || !ok {
		t.Fatalf("no snapshot was built: %v", err)
	}
	plan, err := r.newSnapshotPlan(r.snapshotBase(), req)
	if err != nil {
		t.Fatal(err)
	}
	release := r.holdSnapshot(key)
	if err := r.buildMachineSnapshot(ctx, plan); !errors.Is(err, errSnapshotHeld) {
		t.Fatalf("a build of a held snapshot = %v, want errSnapshotHeld", err)
	}
	if inst, err := f.owner.Instance(ctx, snapshotMachineName(key)); err != nil || inst.ID != record.Instance {
		t.Fatalf("the held snapshot %+v, %v; want its VM kept", inst, err)
	}
	if _, ok, _ := r.readSnapshotRecord(key); ok {
		t.Error("a build that stopped for a held snapshot left it published")
	}
	sweepMachineSnapshots(t, r, time.Now())
	if _, err := f.owner.Instance(ctx, snapshotMachineName(key)); err != nil {
		t.Fatalf("the sweep destroyed a VM a create holds: %v", err)
	}
	release()
	sweepMachineSnapshots(t, r, time.Now())
	if _, err := f.owner.Instance(ctx, snapshotMachineName(key)); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("the sweep kept an unpublished VM no create holds: %v", err)
	}
}

// sweepMachineSnapshots runs one snapshot sweep at now, on the machines the
// pool's disco-vm pool has, as the volume watcher does.
func sweepMachineSnapshots(t *testing.T, r *MachineSandboxRuntime, now time.Time) {
	t.Helper()
	r.sweepSnapshots(context.Background(), slog.Default(), now)
}

// A machine made from a snapshot that was started before — its agent may
// have answered unseen, its start cut short — holds the sandbox's work: a
// later start whose agent never answers fails, and nothing is made again or
// dropped (ADR 26-11-03-986 §6).
func TestAMachineStartedBeforeIsNeverMadeAgainFresh(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	first := machineSnapshotCreate("sbx_1")
	key := machineSnapshotKey(t, f, first)
	if _, err := r.CreateSandbox(ctx, first); err != nil {
		t.Fatal(err)
	}
	waitForSnapshotBuilds(t, r)
	made := machineSnapshotCreate("sbx_2")
	made.Config.Start = workerclient.NewOptBool(false)
	if _, err := r.CreateSandbox(ctx, made); err != nil {
		t.Fatal(err)
	}
	record, _, err := r.readMachineRecord("sbx_2")
	if err != nil || record.Snapshot != key {
		t.Fatalf("sbx_2's record %+v, %v; want one made from the snapshot", record, err)
	}
	// Started once already, by a start the pool agent did not see finish.
	record.Started = true
	if err := r.writeMachineRecord("sbx_2", record); err != nil {
		t.Fatal(err)
	}
	f.agents.mu.Lock()
	f.agents.sealedBroken = true
	f.agents.mu.Unlock()
	r.agentReadyTimeout = 2 * time.Second
	if err := r.StartSandbox(ctx, "sbx_2", nil); err == nil {
		t.Fatal("a start whose agent never answered succeeded")
	}
	if inst, err := f.owner.Instance(ctx, "sbx-2"); err != nil || inst.ID != record.Instance {
		t.Fatalf("sbx_2's machine %+v, %v; want the one it had, kept", inst, err)
	}
	if _, ok, _ := r.readSnapshotRecord(key); !ok {
		t.Error("a start of a machine started before dropped its snapshot")
	}
}

// A build reaches its pool as the pool's sandboxes do, by the same hop: on a
// machine pool reached directly, the pool's name with the provider's host as
// discobox's answer for it, resolved through the provider, and the stub made
// the box's resolver; through an edge, the tunnel. Only the credentials
// endpoint and the builder's are left out, which a build does not use. A
// plan without the address dialed the pool's name through the provider's
// resolver, which boxd does not answer, so nothing it fetched arrived (#1197).
func TestASnapshotBuildReachesItsPoolByTheSandboxesHop(t *testing.T) {
	direct := proxyagent.PoolEndpoints("")
	direct.Address = "dvm-prov-1--pool-1.boxd"
	direct.DNSSystemResolver = true
	edge := proxyagent.PoolEndpoints("wss://dvm-pool-1.boxd.sh")
	edge.DNSSystemResolver = true
	for name, endpoints := range map[string]sandboxconfig.PoolEndpoints{"direct": direct, "edge": edge} {
		f := newMachineFixture(t, nil)
		r := f.runtime
		r.poolEndpoints = endpoints
		plan, err := r.newSnapshotPlan(r.snapshotBase(), machineSnapshotCreate("sbx_1"))
		if err != nil {
			t.Fatal(err)
		}
		data, _, err := r.prepareSnapshotBuild(context.Background(), plan)
		if err != nil {
			t.Fatal(err)
		}
		var doc agentlayer.SnapshotPlan
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		want := endpoints
		want.Credentials, want.BuildKit = "", ""
		if doc.Pool != want {
			t.Errorf("%s: the build's plan names the pool as %+v, want %+v", name, doc.Pool, want)
		}
		if err := r.removeSnapshotBuildMaterial(plan.buildID()); err != nil {
			t.Fatal(err)
		}
	}
}

// A build behind an edge reaches its pool as the pool's sandboxes do: its
// plan's proxy carries the edge the pool holds now — the address it dials and
// the credential the edge takes — so its bridge takes the edge path and
// resolves nothing its fence closes. A plan without it had the bridge resolve
// the pool's host, which the fence denies, and the build never finished.
func TestASnapshotBuildBehindAnEdgeCarriesThePoolsEdge(t *testing.T) {
	f := newMachineFixture(t, nil)
	r := f.runtime
	r.poolEndpoints = proxyagent.PoolEndpoints("wss://7302-pool.edge.example")
	edge := sandboxconfig.RuntimeEdge{Address: "192.0.2.7", Header: "X-Edge-Token", Token: "token", Expires: time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)}
	r.poolEdge = func() *sandboxconfig.RuntimeEdge {
		held := edge
		return &held
	}
	plan, err := r.newSnapshotPlan(r.snapshotBase(), machineSnapshotCreate("sbx_1"))
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := r.prepareSnapshotBuild(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.removeSnapshotBuildMaterial(plan.buildID()) })
	var doc agentlayer.SnapshotPlan
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.RuntimeConfig.Proxy == nil {
		t.Fatal("the build's plan carries no proxy material")
	}
	if got := doc.RuntimeConfig.Proxy.Edge; got == nil || *got != edge {
		t.Fatalf("the build's plan carries proxy edge %+v, want the pool's %+v", got, edge)
	}
}

// A build behind an edge the pool cannot yet name an address or a credential
// for, or holds a credential for that expires before the longest build ends,
// is refused rather than started: its plan is delivered once and never
// renewed, so its bridge would resolve the pool's host, which its fence
// denies, or be turned away by the edge, for the whole of the build. Refused,
// it is recorded as failed and built again later, by which time the pool
// holds both.
func TestASnapshotBuildBehindAnEdgeWithoutItsAddressOrCredentialIsRefused(t *testing.T) {
	full := sandboxconfig.RuntimeEdge{Address: "192.0.2.7", Header: "X-Edge-Token", Token: "token", Expires: time.Now().Add(24 * time.Hour)}
	noAddress, noCredential, expiring := full, full, full
	noAddress.Address = ""
	noCredential.Header, noCredential.Token, noCredential.Expires = "", "", time.Time{}
	expiring.Expires = time.Now().Add(snapshotBuildTimeout / 2)
	for name, edge := range map[string]sandboxconfig.RuntimeEdge{"no address": noAddress, "no credential": noCredential, "expiring mid-build": expiring} {
		f := newMachineFixture(t, nil)
		r := f.runtime
		r.poolEndpoints = proxyagent.PoolEndpoints("wss://7302-pool.edge.example")
		r.poolEdge = func() *sandboxconfig.RuntimeEdge {
			held := edge
			return &held
		}
		plan, err := r.newSnapshotPlan(r.snapshotBase(), machineSnapshotCreate("sbx_1"))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := r.prepareSnapshotBuild(context.Background(), plan); err == nil {
			t.Errorf("%s: the build was prepared, want it refused", name)
		}
		if err := r.removeSnapshotBuildMaterial(plan.buildID()); err != nil {
			t.Fatal(err)
		}
	}
}

// A machine that keeps its data on its volume, rebuilt while its plan has a
// snapshot, is given a root from that snapshot, and keeps its data and cache
// volumes: its new root does not provision every feature from scratch.
func TestAMachineKeptOnItsVolumeIsRebuiltOnItsPlansSnapshot(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	first := machineSnapshotCreate("sbx_7")
	key := machineSnapshotKey(t, f, first)
	if _, err := r.CreateSandbox(ctx, first); err != nil {
		t.Fatal(err)
	}
	waitForSnapshotBuilds(t, r)
	if _, ok, _ := r.readSnapshotRecord(key); !ok {
		t.Fatal("no snapshot was built")
	}
	inst, err := f.owner.Instance(ctx, "sbx-7")
	if err != nil {
		t.Fatal(err)
	}
	writeMachineFiles(t, r, inst)

	req := machineSnapshotCreate("sbx_7")
	req.Config.SpecFingerprint = workerclient.NewOptString("spec-2")
	if got := machineSnapshotKey(t, f, req); got != key {
		t.Fatalf("the moved spec's plan is %s, want the same plan %s", got, key)
	}
	if _, err := r.CreateSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	if f.driver.upgrades != 1 {
		t.Fatalf("upgrades = %d, want one", f.driver.upgrades)
	}
	if inst, err = f.owner.Instance(ctx, "sbx-7"); err != nil {
		t.Fatal(err)
	}
	assertNewRootKeptVolumes(t, r, inst)
	if record, _, _ := r.readMachineRecord("sbx_7"); record.Snapshot != key {
		t.Errorf("the rebuilt machine's record names snapshot %q, want its plan's %s", record.Snapshot, key)
	}
	if !guestHas(t, f, "sbx_7", "/provisioned") {
		t.Error("the rebuilt machine's new root does not hold what its plan's snapshot provisioned: it was given a fresh root from the base")
	}
}

// snapshotMachineWithFiles creates sandboxID from a plan with a snapshot,
// built behind its first create, and lays files on its root and volumes, so
// that what a new root keeps can be read. It returns the plan's key.
func snapshotMachineWithFiles(t *testing.T, f *machineFixture, sandboxID string) string {
	t.Helper()
	ctx := context.Background()
	r := f.runtime
	req := machineSnapshotCreate(sandboxID)
	key := machineSnapshotKey(t, f, req)
	if _, err := r.CreateSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	waitForSnapshotBuilds(t, r)
	if _, ok, _ := r.readSnapshotRecord(key); !ok {
		t.Fatal("no snapshot was built")
	}
	name, err := machineName(sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	writeMachineFiles(t, r, inst)
	return key
}

// assertFreshRootKeptVolumes checks the running sandboxID's machine was given
// a fresh root from the driver's base, no snapshot's, and kept its volumes.
func assertFreshRootKeptVolumes(t *testing.T, f *machineFixture, sandboxID string) {
	t.Helper()
	name, err := machineName(sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(context.Background(), name)
	if err != nil {
		t.Fatalf("%s's machine: %v; want it kept", sandboxID, err)
	}
	assertNewRootKeptVolumes(t, f.runtime, inst)
	if record, _, _ := f.runtime.readMachineRecord(sandboxID); record.Snapshot != "" || record.NewRootSnapshot != "" || record.NewRootAsked {
		t.Errorf("%s's record %+v; want a root no snapshot's, and nothing left asked", sandboxID, record)
	}
	if guestHas(t, f, sandboxID, "/provisioned") {
		t.Errorf("%s's new root holds what a snapshot provisioned, want a fresh one", sandboxID)
	}
}

// A rebuild's root from its plan's snapshot whose agent never comes up is
// replaced by a fresh root in the same create, never destroyed: its volumes
// hold the sandbox's data. The snapshot stands, and its plan is not held
// back: a box that has run can break its own volume, which its agent reads at
// boot, so its agent never answering is no word on a snapshot its plan's
// other boxes share.
func TestARebuiltRootFromASnapshotThatNeverComesUpIsMadeFreshKeepingItsVolumes(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	key := snapshotMachineWithFiles(t, f, "sbx_7")
	f.agents.mu.Lock()
	f.agents.sealedBroken = true
	f.agents.mu.Unlock()
	r.agentReadyTimeout = 2 * time.Second

	req := machineSnapshotCreate("sbx_7")
	req.Config.SpecFingerprint = workerclient.NewOptString("spec-2")
	sb, err := r.CreateSandbox(ctx, req)
	if err != nil {
		t.Fatalf("a rebuild whose snapshot root never came up: %v", err)
	}
	if sb.Status != StatusRunning || f.driver.upgrades != 2 {
		t.Fatalf("sbx_7 = %+v after %d upgrades, want it running on two: the snapshot's root, then a fresh one", sb, f.driver.upgrades)
	}
	assertFreshRootKeptVolumes(t, f, "sbx_7")
	if _, ok, _ := r.readSnapshotRecord(key); !ok {
		t.Error("a rebuilt box whose snapshot root never came up dropped the snapshot its plan shares")
	}
	if r.snapshotFailedRecently(key, time.Now()) {
		t.Error("a rebuilt box whose snapshot root never came up held its plan back")
	}
}

// A new root a pool from before newRootSnapshot asked for (newRoot alone,
// which it asked fresh, and may have begun) is finished fresh, even when a
// create in between finds the plan's snapshot: the driver finishes a root
// only from the one it began.
func TestANewRootAskedByAnOlderPoolIsFinishedFresh(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	snapshotMachineWithFiles(t, f, "sbx_5")
	if err := r.StopSandbox(ctx, "sbx_5", nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(r.machineRecordPath("sbx_5"))
	if err != nil {
		t.Fatal(err)
	}
	var old map[string]any
	if err := json.Unmarshal(data, &old); err != nil {
		t.Fatal(err)
	}
	old["newRoot"] = true
	delete(old, "newRootSnapshot")
	delete(old, "newRootAsked")
	if data, err = json.Marshal(old); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.machineRecordPath("sbx_5"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	req := machineSnapshotCreate("sbx_5")
	req.Config.SpecFingerprint = workerclient.NewOptString("spec-2")
	if _, err := r.CreateSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	if f.driver.upgrades != 1 {
		t.Fatalf("upgrades = %d, want one", f.driver.upgrades)
	}
	assertFreshRootKeptVolumes(t, f, "sbx_5")
}

// A snapshot dropped between the rebuild that found it and the start that
// gives the new root is a fresh root.
func TestARebuildWhoseSnapshotIsGoneByItsStartGetsAFreshRoot(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	key := snapshotMachineWithFiles(t, f, "sbx_8")
	if err := r.StopSandbox(ctx, "sbx_8", nil); err != nil {
		t.Fatal(err)
	}
	req := machineSnapshotCreate("sbx_8")
	req.Config.SpecFingerprint = workerclient.NewOptString("spec-2")
	req.Config.Start = workerclient.NewOptBool(false)
	if _, err := r.CreateSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	if record, _, err := r.readMachineRecord("sbx_8"); err != nil || !record.NewRoot || record.NewRootSnapshot != key {
		t.Fatalf("sbx_8's record %+v, %v; want a new root asked from its plan's snapshot", record, err)
	}
	if err := r.dropSnapshot(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := r.StartSandbox(ctx, "sbx_8", nil); err != nil {
		t.Fatal(err)
	}
	if f.driver.upgrades != 1 {
		t.Fatalf("upgrades = %d, want one", f.driver.upgrades)
	}
	assertFreshRootKeptVolumes(t, f, "sbx_8")
}

// An upgrade from a snapshot cut short is finished by the next start from the
// same snapshot, as recorded before the driver was asked.
func TestARebuildFromASnapshotCutShortIsFinishedFromIt(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	key := snapshotMachineWithFiles(t, f, "sbx_9")
	f.driver.upgradeFailures = 1
	req := machineSnapshotCreate("sbx_9")
	req.Config.SpecFingerprint = workerclient.NewOptString("spec-2")
	if _, err := r.CreateSandbox(ctx, req); err == nil || !strings.Contains(err.Error(), "cut short") {
		t.Fatalf("create on an upgrade cut short = %v, want it failed", err)
	}
	if record, _, err := r.readMachineRecord("sbx_9"); err != nil || !record.NewRoot || !record.NewRootAsked || record.NewRootSnapshot != key {
		t.Fatalf("sbx_9's record %+v, %v; want the upgrade from its snapshot recorded as asked", record, err)
	}
	if err := r.StartSandbox(ctx, "sbx_9", nil); err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-9")
	if err != nil {
		t.Fatal(err)
	}
	assertNewRootKeptVolumes(t, r, inst)
	if record, _, _ := r.readMachineRecord("sbx_9"); record.Snapshot != key || record.NewRootAsked {
		t.Errorf("sbx_9's record %+v; want its root the snapshot's, nothing left asked", record)
	}
	if !guestHas(t, f, "sbx_9", "/provisioned") {
		t.Error("the finished upgrade's root is not the snapshot's")
	}
}

// An upgrade from a snapshot cut short, whose snapshot is gone by the next
// start, is asked again from it, and the driver's not-found makes it a fresh
// root.
func TestARebuildCutShortWhoseSnapshotIsGoneGetsAFreshRoot(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	key := snapshotMachineWithFiles(t, f, "sbx_6")
	f.driver.upgradeFailures = 1
	req := machineSnapshotCreate("sbx_6")
	req.Config.SpecFingerprint = workerclient.NewOptString("spec-2")
	if _, err := r.CreateSandbox(ctx, req); err == nil {
		t.Fatal("a create on an upgrade cut short succeeded")
	}
	if err := r.dropSnapshot(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := r.StartSandbox(ctx, "sbx_6", nil); err != nil {
		t.Fatal(err)
	}
	if f.driver.upgrades != 3 {
		t.Fatalf("upgrades = %d, want the one cut short, the one from the gone snapshot, and the fresh one", f.driver.upgrades)
	}
	assertFreshRootKeptVolumes(t, f, "sbx_6")
}

// An upgrade from a snapshot cut short is never finished from a VM of the
// snapshot's name that no done record names (a build of the plan under way):
// the start fails and leaves the upgrade asked, and once the snapshot is
// published again the next start finishes it from it.
func TestARebuildCutShortIsNotFinishedFromAnUnpublishedSnapshot(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	key := snapshotMachineWithFiles(t, f, "sbx_4")
	published, _, err := r.readSnapshotRecord(key)
	if err != nil {
		t.Fatal(err)
	}
	f.driver.upgradeFailures = 1
	req := machineSnapshotCreate("sbx_4")
	req.Config.SpecFingerprint = workerclient.NewOptString("spec-2")
	if _, err := r.CreateSandbox(ctx, req); err == nil {
		t.Fatal("a create on an upgrade cut short succeeded")
	}
	if err := r.unpublishSnapshot(key); err != nil {
		t.Fatal(err)
	}
	if err := r.StartSandbox(ctx, "sbx_4", nil); err == nil || !strings.Contains(err.Error(), "being built again") {
		t.Fatalf("a start whose snapshot is unpublished = %v, want it refused", err)
	}
	if f.driver.upgrades != 1 {
		t.Fatalf("upgrades = %d, want only the one cut short", f.driver.upgrades)
	}
	if record, _, err := r.readMachineRecord("sbx_4"); err != nil || !record.NewRoot || !record.NewRootAsked || record.NewRootSnapshot != key {
		t.Fatalf("sbx_4's record %+v, %v; want the upgrade from its snapshot still asked", record, err)
	}
	if err := r.writeSnapshotRecord(key, published); err != nil {
		t.Fatal(err)
	}
	if err := r.StartSandbox(ctx, "sbx_4", nil); err != nil {
		t.Fatal(err)
	}
	if record, _, _ := r.readMachineRecord("sbx_4"); record.Snapshot != key {
		t.Errorf("sbx_4's record %+v; want its root the snapshot's", record)
	}
	if !guestHas(t, f, "sbx_4", "/provisioned") {
		t.Error("the finished upgrade's root is not the snapshot's")
	}
}

// An upgrade from a snapshot cut short whose snapshot's VM is gone is not
// asked again from it while a build of the plan is under way: the build
// makes a VM of that name, which no done record names yet.
func TestARebuildCutShortWaitsOutABuildOfItsSnapshot(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	key := snapshotMachineWithFiles(t, f, "sbx_3")
	f.driver.upgradeFailures = 1
	req := machineSnapshotCreate("sbx_3")
	req.Config.SpecFingerprint = workerclient.NewOptString("spec-2")
	if _, err := r.CreateSandbox(ctx, req); err == nil {
		t.Fatal("a create on an upgrade cut short succeeded")
	}
	if err := r.dropSnapshot(ctx, key); err != nil {
		t.Fatal(err)
	}
	// A build of the plan, past its hold check, before its create.
	r.snapshots.mu.Lock()
	r.snapshots.active = map[string]*snapshotBuild{key: {cancel: func() {}}}
	r.snapshots.mu.Unlock()
	if err := r.StartSandbox(ctx, "sbx_3", nil); err == nil || !strings.Contains(err.Error(), "being built again") {
		t.Fatalf("a start while its snapshot is being built = %v, want it refused", err)
	}
	if f.driver.upgrades != 1 {
		t.Fatalf("upgrades = %d, want only the one cut short", f.driver.upgrades)
	}
	r.snapshots.mu.Lock()
	delete(r.snapshots.active, key)
	r.snapshots.mu.Unlock()
	if err := r.StartSandbox(ctx, "sbx_3", nil); err != nil {
		t.Fatal(err)
	}
	assertFreshRootKeptVolumes(t, f, "sbx_3")
}
