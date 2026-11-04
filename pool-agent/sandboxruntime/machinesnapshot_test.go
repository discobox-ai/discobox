package sandboxruntime

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"

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
	f := newMachineFixture(t)
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
}

// A driver that does not report Commit gets no snapshot: every sandbox is
// made fresh, and no build is ever asked for (ADR 26-11-03-986 §2).
func TestAMachinePoolOnADriverThatDoesNotCommitBuildsNoSnapshot(t *testing.T) {
	t.Setenv(fake.NoCommitEnv, "1")
	f := newMachineFixture(t)
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
	f := newMachineFixture(t)
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
	f := newMachineFixture(t)
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
	f := newMachineFixture(t)
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
}

// A cache clear drops every snapshot, record first, and forgets the plans
// that failed, so the next create is fresh and builds again.
func TestAMachinePoolsCacheClearDropsItsSnapshots(t *testing.T) {
	f := newMachineFixture(t)
	ctx := context.Background()
	r := f.runtime
	req := machineSnapshotCreate("sbx_1")
	key := machineSnapshotKey(t, f, req)
	if _, err := r.CreateSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	waitForSnapshotBuilds(t, r)
	if err := r.recordSnapshotFailure("other", time.Now()); err != nil {
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
	f := newMachineFixture(t)
	_, err := f.runtime.engine.Create(context.Background(), engine.CreateOptions{Name: "sbx-9", From: snapshotMachineName(strings.Repeat("a", 64))})
	if !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("create from a missing snapshot = %v, want engine.ErrNotFound", err)
	}
}

// A build never destroys a snapshot a create is still making a box from: it
// unpublishes it, so no create takes it after, and stops, which is not the
// plan's failure. The sweep destroys the VM once the create lets it go.
func TestABuildLeavesASnapshotACreateHolds(t *testing.T) {
	f := newMachineFixture(t)
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
	f := newMachineFixture(t)
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
