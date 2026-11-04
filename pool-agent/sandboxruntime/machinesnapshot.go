package sandboxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"

	"github.com/discobox-ai/discobox/pkg/layout"
)

// On a machine pool a snapshot is a committed disco-vm VM, and only where the
// pool's driver reports Capabilities.Commit (ADR 26-11-03-986 §2): on any
// other every sandbox's machine is made fresh and provisions from scratch.
// Each plan's VM has one name, snap-<key>, and what makes it a snapshot is the
// pool's own done record, never disco-vm's word that it is committed (§4). A
// build of it always clears the record, destroys whatever VM has the name,
// creates it fresh, places the agent and the plan, runs the snapshot command
// as an exec, stops it, commits it, and only then writes the record: a crash
// anywhere leaves either no record, so the next build starts over, or a
// record naming a VM that committed. A snap- VM no record names is never
// started, copied or committed again, only destroyed (§5).

const (
	// snapshotMachinePrefix begins the name of every snapshot's VM. No
	// sandbox's machine name begins with it (sandboxMachinePrefix), so the
	// orphan reaper and the snapshot sweep never take each other's.
	snapshotMachinePrefix = "snap-"
	// snapshotRecordsName is the directory, in the pool's durable state,
	// that holds each snapshot's done record, by key.
	snapshotRecordsName = "snapshot-records"
	// snapshotRetention is how long a snapshot no create has used is kept.
	snapshotRetention = 14 * 24 * time.Hour
	// snapshotUseStamp is how stale a snapshot's lastUsed may get before a
	// create writes it again: the reclaim needs days, not each create.
	snapshotUseStamp = time.Hour
)

// snapshotMachineName is the VM of the plan keyed key.
func snapshotMachineName(key string) string { return snapshotMachinePrefix + key[:24] }

// isSnapshotMachineName reports whether name is one snapshotMachineName
// makes: the prefix and 24 lower-case hex digits, and nothing else. A
// pool's disco-vm pool is a naming convention, not a fence (ADR 26-10-23-551 §4),
// so a machine of any other name, however it begins, is not the sweep's.
func isSnapshotMachineName(name string) bool {
	hex, ok := strings.CutPrefix(name, snapshotMachinePrefix)
	if !ok || len(hex) != 24 {
		return false
	}
	for _, c := range hex {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// snapshotRecord is a snapshot's done record: the VM a build committed, as
// Commit returned it. A VM of the snapshot's name whose ID is not this one is
// not the snapshot.
type snapshotRecord struct {
	Instance string    `json:"instance"`
	Built    time.Time `json:"built"`
	// LastUsed is when a create last made a box from it, which the reclaim
	// reads.
	LastUsed time.Time `json:"lastUsed"`
}

func (r *MachineSandboxRuntime) snapshotRecordsDir() string {
	return filepath.Join(r.root.PoolData(r.projectID, r.poolID), snapshotRecordsName)
}

func (r *MachineSandboxRuntime) snapshotRecordPath(key string) string {
	return filepath.Join(r.snapshotRecordsDir(), key+".json")
}

func (r *MachineSandboxRuntime) readSnapshotRecord(key string) (snapshotRecord, bool, error) {
	data, err := os.ReadFile(r.snapshotRecordPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return snapshotRecord{}, false, nil
	}
	if err != nil {
		return snapshotRecord{}, false, err
	}
	var record snapshotRecord
	if json.Unmarshal(data, &record) != nil || record.Instance == "" {
		// A record that does not read names no VM: the snapshot is not one,
		// and the next build or sweep replaces it.
		return snapshotRecord{}, false, nil //nolint:nilerr // an unreadable record is no record, not an error
	}
	return record, true, nil
}

// writeSnapshotRecord writes the done record of the plan keyed key, durably:
// it is what publishes the snapshot.
func (r *MachineSandboxRuntime) writeSnapshotRecord(key string, record snapshotRecord) error {
	if err := os.MkdirAll(r.snapshotRecordsDir(), 0o700); err != nil {
		return err
	}
	r.snapshots.records.Lock()
	defer r.snapshots.records.Unlock()
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return writeFileDurably(r.snapshotRecordPath(key), data)
}

// unpublishSnapshot unpublishes the snapshot keyed key, durably, before
// anything is done to its VM.
func (r *MachineSandboxRuntime) unpublishSnapshot(key string) error {
	r.snapshots.records.Lock()
	defer r.snapshots.records.Unlock()
	return removeFileDurably(r.snapshotRecordPath(key))
}

// snapshotRecords is every done record, by key.
func (r *MachineSandboxRuntime) snapshotRecords() (map[string]snapshotRecord, error) {
	entries, err := os.ReadDir(r.snapshotRecordsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]snapshotRecord{}
	for _, entry := range entries {
		key, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || len(key) < 24 {
			continue
		}
		record, ok, err := r.readSnapshotRecord(key)
		if err != nil {
			return nil, err
		}
		if ok {
			out[key] = record
		}
	}
	return out, nil
}

// commits reports whether the pool's driver commits VMs, read from what it
// reports and never by trying (ADR 26-11-03-986 §2).
func (r *MachineSandboxRuntime) commits() bool { return r.engine.Driver.Capabilities().Commit }

// snapshotBase is what a build starts from, as a snapshot's key names it: the
// driver and the base its configuration names, so a base moved is a plan
// built again.
func (r *MachineSandboxRuntime) snapshotBase() string {
	return r.engine.Driver.Name() + ":" + r.base
}

// snapshotFor is the snapshot a new sandbox machine for req is made from:
// its VM's name and its plan's key, held until release is called so nothing
// removes it while the create reads it. None is a machine made fresh, and
// asks for the plan's snapshot to be built behind it — unless the driver does
// not commit, or the server sent no build policy.
func (r *MachineSandboxRuntime) snapshotFor(ctx context.Context, req *workerapimodel.PoolSandboxCreateRequest) (from, key string, release func()) {
	release = func() {}
	if req == nil || !req.ProvisionEgress.Set || !r.commits() {
		// A server that sends no build policy gets no snapshot: built
		// without one, it would run unrestricted.
		return "", "", release
	}
	plan, err := r.newSnapshotPlan(r.snapshotBase(), req)
	if err != nil {
		slog.WarnContext(ctx, "plan a snapshot; making the sandbox's machine fresh", "error", err)
		return "", "", release
	}
	key = plan.Key()
	name := snapshotMachineName(key)
	held := r.holdSnapshot(key)
	record, ok, err := r.readSnapshotRecord(key)
	if err != nil {
		slog.WarnContext(ctx, "read a snapshot's record; making the sandbox's machine fresh", "snapshot", name, "error", err)
	}
	if ok {
		inst, err := r.engine.Driver.Instance(ctx, name)
		switch {
		case err == nil && inst.ID == record.Instance:
			if now := time.Now().UTC(); now.Sub(record.LastUsed) >= snapshotUseStamp {
				record.LastUsed = now
				if err := r.writeSnapshotRecord(key, record); err != nil {
					slog.WarnContext(ctx, "record a snapshot's use", "snapshot", name, "error", err)
				}
			}
			slog.InfoContext(ctx, "making a sandbox machine from a snapshot", "snapshot", name, "sandboxId", req.SandboxId)
			return name, key, held
		case err != nil && !errors.Is(err, machine.ErrNotFound):
			// The driver cannot be asked now: fresh, and no build asked
			// for, since the snapshot may well be there.
			slog.WarnContext(ctx, "look up a snapshot; making the sandbox's machine fresh", "snapshot", name, "error", err)
			held()
			return "", "", release
		}
	}
	held()
	r.requestSnapshot(plan, r.buildMachineSnapshot)
	return "", "", release
}

// buildMachineSnapshot builds plan's snapshot (ADR 26-11-03-986 §4): it
// removes the done record, destroys any VM of the snapshot's name, creates
// one fresh, provisions it with the snapshot command, stops it, commits it,
// and writes the record. A failure stops it at the step it reached; the VM it
// made is destroyed unless the record names it, and what the pool laid for
// the build goes once the VM is gone or committed.
func (r *MachineSandboxRuntime) buildMachineSnapshot(ctx context.Context, plan snapshotPlan) (err error) {
	key, id := plan.Key(), plan.buildID()
	name := snapshotMachineName(key)
	// Unpublished first, so no create takes it from here; one already
	// copying the VM of this name keeps it, and the plan is built again
	// when the next create asks.
	if err := r.unpublishSnapshot(key); err != nil {
		return fmt.Errorf("remove the snapshot's record: %w", err)
	}
	if r.snapshotInUse(key) {
		return errSnapshotHeld
	}
	if err := r.destroyMachine(ctx, name); err != nil {
		return err
	}
	made, published := false, false
	defer func() {
		detached := context.WithoutCancel(ctx)
		if made && !published {
			if destroyErr := r.destroyMachine(detached, name); destroyErr != nil {
				// Its proxy record stays while the VM may run: the sweep
				// takes both, in order.
				err = errors.Join(err, destroyErr)
				return
			}
		}
		err = errors.Join(err, r.removeSnapshotBuildMaterial(id))
	}()
	data, agent, err := r.prepareSnapshotBuild(ctx, plan)
	if err != nil {
		return err
	}
	inst, err := r.engine.Create(ctx, r.createOptions(name))
	if err != nil {
		// ErrExists too: what holds the name is not this build's to use,
		// nor to destroy; the sweep finds it unrecorded.
		return fmt.Errorf("create the build's machine: %w", err)
	}
	made = true
	if err := r.engine.Start(ctx, inst, engine.StartOptions{Timeout: machineStartTimeout}); err != nil {
		return fmt.Errorf("start the build's machine: %w", err)
	}
	guest, err := r.engine.Guest(ctx, inst)
	if err != nil {
		return err
	}
	if err := runSnapshotCommand(ctx, guest, agent, plan.Pins, data); err != nil {
		return err
	}
	if err := r.engine.Stop(ctx, inst, snapshotStopTimeout); err != nil {
		return fmt.Errorf("stop the build's machine: %w", err)
	}
	if state := r.engine.State(ctx, inst); state != engine.Stopped {
		return fmt.Errorf("the build's machine is %s after its stop", state)
	}
	committed, err := r.engine.Commit(ctx, inst)
	if err != nil {
		return fmt.Errorf("commit the build's machine: %w", err)
	}
	// A clear that ended the build waits it out (clearSnapshots): what it
	// committed from the caches being cleared is not published.
	if err := ctx.Err(); err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := r.writeSnapshotRecord(key, snapshotRecord{Instance: committed.ID, Built: now, LastUsed: now}); err != nil {
		return fmt.Errorf("record the snapshot: %w", err)
	}
	published = true
	return nil
}

// dropSnapshot unpublishes the snapshot keyed key, then destroys its VM —
// unless a create holds it, when the sweep destroys it later.
func (r *MachineSandboxRuntime) dropSnapshot(ctx context.Context, key string) error {
	if err := r.unpublishSnapshot(key); err != nil {
		return fmt.Errorf("remove the snapshot's record: %w", err)
	}
	if r.snapshotInUse(key) {
		return nil
	}
	return r.destroyMachine(ctx, snapshotMachineName(key))
}

// sweepSnapshots removes what no snapshot is (ADR 26-11-03-986 §5): every
// snap- VM of the pool's disco-vm pool that no done record names and no build under
// way holds, then what the pool laid for a build no longer under way, and
// the record of a snapshot whose VM is gone. It reclaims a snapshot no
// create has used for snapshotRetention, record first.
func (r *MachineSandboxRuntime) sweepSnapshots(ctx context.Context, logger *slog.Logger, now time.Time) {
	r.sweepSnapshotFailures(now)
	if !r.lockSnapshotSweep() {
		return
	}
	defer r.snapshots.one.Unlock()
	// Listed under the build lock, so a build that publishes in between is
	// in the list, as committed.
	instances, err := r.engine.List(ctx)
	if err != nil {
		logger.Warn("list the pool's machines for its snapshots", "error", err)
		return
	}
	building := map[string]bool{}
	for key := range r.buildingSnapshots() {
		building[snapshotMachineName(key)] = true
	}
	records, err := r.snapshotRecords()
	if err != nil {
		logger.Warn("read the pool's snapshot records", "error", err)
		return
	}
	byName := make(map[string]string, len(records))
	for key := range records {
		byName[snapshotMachineName(key)] = key
	}
	// left is every snap- VM still there once this pass is done that is no
	// snapshot: what the pool laid for its build stays with it.
	left := map[string]bool{}
	present := map[string]bool{}
	for _, inst := range instances {
		if !isSnapshotMachineName(inst.Name) || building[inst.Name] {
			continue
		}
		present[inst.Name] = true
		if key, ok := byName[inst.Name]; ok && records[key].Instance == inst.ID {
			if now.Sub(records[key].LastUsed) < snapshotRetention || r.snapshotInUse(key) {
				continue
			}
			logger.Info("reclaiming a snapshot no create has used", "snapshot", inst.Name, "lastUsed", records[key].LastUsed)
			if err := r.dropSnapshot(ctx, key); err != nil {
				logger.Warn("reclaim a snapshot", "snapshot", inst.Name, "error", err)
			}
			continue
		}
		if r.snapshotHeldBy(func(key string) bool { return snapshotMachineName(key) == inst.Name }) {
			// Dropped while a create copies it: destroyed once released.
			continue
		}
		logger.Warn("removing a snapshot machine no record names", "machine", inst.Name)
		if err := r.destroyMachine(ctx, inst.Name); err != nil {
			logger.Warn("remove a snapshot machine no record names", "machine", inst.Name, "error", err)
			left[inst.Name] = true
		}
	}
	for key := range records {
		if name := snapshotMachineName(key); !present[name] && !building[name] {
			if err := r.unpublishSnapshot(key); err != nil {
				logger.Warn("remove the record of a snapshot whose machine is gone", "snapshot", name, "error", err)
			}
		}
	}
	entries, err := os.ReadDir(r.snapshotsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Warn("list the pool's snapshot builds", "error", err)
		return
	}
	for _, entry := range entries {
		name := snapshotMachinePrefix + strings.TrimPrefix(entry.Name(), layout.SnapshotIDPrefix)
		if building[name] || left[name] {
			continue
		}
		if err := r.removeSnapshotBuildMaterial(entry.Name()); err != nil {
			logger.Warn("remove what a snapshot build left", "build", entry.Name(), "error", err)
		}
	}
}

// clearSnapshots drops every snapshot of the pool and forgets which plans
// failed to build, as a cache clear does: a box that came up wrong from one
// is made fresh next.
func (r *MachineSandboxRuntime) clearSnapshots(ctx context.Context) error {
	// The build it ends winds down, and none starts, before anything is
	// dropped, so none publishes after the clear.
	release := r.holdSnapshotClear()
	defer release()
	errs := []error{r.forgetSnapshotFailures()}
	records, err := r.snapshotRecords()
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for key := range records {
		errs = append(errs, r.dropSnapshot(ctx, key))
	}
	return errors.Join(errs...)
}
