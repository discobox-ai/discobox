package sandboxruntime

import (
	"context"
	"testing"

	"github.com/moby/moby/api/types/mount"

	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
)

const gib = 1 << 30

// sizedMachineSnapshotCreate is a snapshot-able create asking for size.
func sizedMachineSnapshotCreate(sandboxID string, cpuVCPUs float64, memoryBytes int64) *workerapimodel.PoolSandboxCreateRequest {
	req := machineSnapshotCreate(sandboxID)
	req.Size = workerclient.NewOptSandboxSize(workerapimodel.SandboxSize{
		CpuVcpus:    workerclient.NewOptFloat64(cpuVCPUs),
		MemoryBytes: workerclient.NewOptInt64(memoryBytes),
	})
	return req
}

// A box's container is limited to the size its create sends, swap included,
// and one sent none, or a snapshot build, is not limited at all (ADR
// 26-11-23-445 §4).
func TestABoxContainerIsLimitedToItsSize(t *testing.T) {
	r := testDockerRuntime(&DockerSandboxRuntime{projectID: "proj_1", poolID: "pool_1"})
	host, _ := r.boxConfig([]mount.Mount{}, boxSize{CPUVCPUs: 1.5, MemoryBytes: 2 * gib})
	if host.NanoCPUs != 1_500_000_000 || host.Memory != 2*gib || host.MemorySwap != 2*gib {
		t.Errorf("sized box: NanoCPUs %d, Memory %d, MemorySwap %d; want 1.5 CPUs and 2 GiB with no swap past it", host.NanoCPUs, host.Memory, host.MemorySwap)
	}
	host, _ = r.boxConfig(nil, boxSize{MemoryBytes: gib})
	if host.NanoCPUs != 0 || host.Memory != gib {
		t.Errorf("memory-only box: NanoCPUs %d, Memory %d; want no CPU limit and 1 GiB", host.NanoCPUs, host.Memory)
	}
	unsized := requestedSize(&workerapimodel.PoolSandboxCreateRequest{SandboxId: "sbx_1"})
	host, _ = r.boxConfig(nil, unsized)
	if host.NanoCPUs != 0 || host.Memory != 0 || host.MemorySwap != 0 {
		t.Errorf("unsized box: %+v; want no limit", host.Resources)
	}
	sized := requestedSize(sizedMachineSnapshotCreate("sbx_1", 2, 4*gib))
	if sized != (boxSize{CPUVCPUs: 2, MemoryBytes: 4 * gib}) {
		t.Errorf("requested size = %+v, want what the create sent", sized)
	}
}

// A sized box's machine is made at its size, vCPUs rounded up to a whole
// one; its plan's snapshot is keyed and built at that size, apart from an
// unsized box's, and the next box of that size is made from it. A machine
// made again with no request in hand keeps the size (ADR 26-11-23-445 §§2,
// 5).
func TestASizedMachineBoxAndItsSnapshotAreMadeAtItsSize(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	first := sizedMachineSnapshotCreate("sbx_1", 1.5, 3*gib)
	if _, err := r.CreateSandbox(ctx, first); err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-1")
	if err != nil || inst.CPUs != 2 || inst.Memory != 3*gib {
		t.Fatalf("sbx_1's machine = %d CPUs, %d bytes, %v; want 2 and 3 GiB", inst.CPUs, inst.Memory, err)
	}
	waitForSnapshotBuilds(t, r)

	plan, err := r.newSnapshotPlan(r.snapshotBase(), first)
	if err != nil {
		t.Fatal(err)
	}
	unsizedKey := plan.Key()
	plan.Size = &MachineSize{CPUs: 2, MemoryBytes: 3 * gib}
	key := plan.Key()
	if key == unsizedKey {
		t.Fatal("a sized plan has an unsized plan's key")
	}
	if _, ok, _ := r.readSnapshotRecord(unsizedKey); ok {
		t.Error("a sized box's create built an unsized snapshot")
	}
	record, ok, err := r.readSnapshotRecord(key)
	if err != nil || !ok {
		t.Fatalf("snapshot record = %v, %v; want the sized build recorded", ok, err)
	}
	if record.Plan == nil || record.Plan.Size == nil || *record.Plan.Size != *plan.Size {
		t.Errorf("snapshot's plan = %+v, want it to name its size", record.Plan)
	}
	snap, err := f.owner.Instance(ctx, snapshotMachineName(key))
	if err != nil || snap.CPUs != 2 || snap.Memory != 3*gib {
		t.Fatalf("snapshot machine = %d CPUs, %d bytes, %v; want built at the box's size", snap.CPUs, snap.Memory, err)
	}

	if _, err := r.CreateSandbox(ctx, sizedMachineSnapshotCreate("sbx_2", 2, 3*gib)); err != nil {
		t.Fatal(err)
	}
	second, _, err := r.readMachineRecord("sbx_2")
	if err != nil || second.Snapshot != key {
		t.Fatalf("a box of the same size: snapshot %q, %v; want %s", second.Snapshot, err, key)
	}
	inst, err = f.owner.Instance(ctx, "sbx-2")
	if err != nil || inst.CPUs != 2 || inst.Memory != 3*gib {
		t.Fatalf("sbx_2's machine = %d CPUs, %d bytes, %v; want 2 and 3 GiB", inst.CPUs, inst.Memory, err)
	}

	unlock := holdPower(t, r, "sbx_1")
	err = r.remakeMachine(ctx, "sbx_1")
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	inst, err = f.owner.Instance(ctx, "sbx-1")
	if err != nil || inst.CPUs != 2 || inst.Memory != 3*gib {
		t.Fatalf("sbx_1's machine made again = %d CPUs, %d bytes, %v; want its recorded size", inst.CPUs, inst.Memory, err)
	}
}

// A create the driver refuses to make from the plan's snapshot, as archil
// refuses a copy at a size other than its source's, makes the box's machine
// fresh at its size rather than failing it on the driver's refusal (ADR
// 26-11-23-445 §5).
func TestACopyTheDriverRefusesIsMadeFreshAtTheBoxsSize(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, sizedMachineSnapshotCreate("sbx_1", 4, 8*gib)); err != nil {
		t.Fatal(err)
	}
	waitForSnapshotBuilds(t, r)
	f.driver.copyRefused.Store(true)

	if _, err := r.CreateSandbox(ctx, sizedMachineSnapshotCreate("sbx_2", 4, 8*gib)); err != nil {
		t.Fatalf("CreateSandbox = %v, want the box made fresh", err)
	}
	record, _, err := r.readMachineRecord("sbx_2")
	if err != nil || record.Snapshot != "" {
		t.Fatalf("sbx_2 made from snapshot %q, %v; want fresh", record.Snapshot, err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-2")
	if err != nil || inst.From != "" || inst.CPUs != 4 || inst.Memory != 8*gib {
		t.Fatalf("sbx_2's machine = %+v, %v; want fresh at 4 CPUs and 8 GiB", inst, err)
	}
}
