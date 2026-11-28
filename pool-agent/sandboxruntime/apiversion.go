package sandboxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/discobox-ai/discobox/pkg/fsdurable"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
)

// The sandbox.json apiVersion a pool writes is one the sandbox's pinned agent
// reads, as its agent bundle declares (ADR 26-11-24-519 §§2–3): the agent can
// be older than this pool agent, and refuses at boot a value it does not read.
//
// What the bundle declares is in its config blob, which the pool fetches only
// for an agent layer new to the sandbox: the record below says what it learned
// of which layer. A bundle gone from its registry (a pruned main build, pinned
// by a manual box) keeps working as it did, since its layer is placed from the
// pool's own copy, and a box made before the record booted that layer's agent
// on the legacy value, which answers for it when the fetch cannot.

// sandboxConfigRecordName is the pool's record, beside the sandbox's tree and
// never in a volume the sandbox writes, of the apiVersion its sandbox.json
// holds and what the pool knows of an agent layer that reads it. A container
// keeps the file it was built with when its pins move in place, so a create
// reads this to know whether the agent it moved to reads it.
const sandboxConfigRecordName = "sandbox-config.json"

// sandboxConfigRecord is what the pool knows of a sandbox's sandbox.json:
// Layer's agent reads APIVersion, and the newest version it reads is Writes.
type sandboxConfigRecord struct {
	// APIVersion is the version the sandbox's sandbox.json was last written
	// with.
	APIVersion string `json:"apiVersion"`
	// Layer is an agent layer known to read APIVersion: the one it was
	// written for, or one the agent moved to since that was asked.
	Layer string `json:"layer"`
	// Writes is the version the pool writes for Layer: the newest it reads.
	Writes string `json:"writes"`
}

func (r *sandboxCore) sandboxConfigRecordPath(sandboxID string) string {
	return filepath.Join(r.sandboxRoot(sandboxID), sandboxConfigRecordName)
}

// recordSandboxConfig records what the pool knows of the sandbox's
// sandbox.json.
func (r *sandboxCore) recordSandboxConfig(sandboxID string, record sandboxConfigRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	path := r.sandboxConfigRecordPath(sandboxID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsdurable.WriteFile(path, data, 0o600)
}

// readSandboxConfig is the sandbox's record, and false for a sandbox whose
// sandbox.json was written before it, when every pool wrote the legacy version.
func (r *sandboxCore) readSandboxConfig(sandboxID string) (sandboxConfigRecord, bool, error) {
	data, err := os.ReadFile(r.sandboxConfigRecordPath(sandboxID))
	if errors.Is(err, fs.ErrNotExist) {
		return sandboxConfigRecord{}, false, nil
	}
	if err != nil {
		return sandboxConfigRecord{}, false, err
	}
	var record sandboxConfigRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return sandboxConfigRecord{}, false, fmt.Errorf("decode %s: %w", sandboxConfigRecordName, err)
	}
	return record, true, nil
}

// sandboxConfigFor is the sandbox.json apiVersion to write for the sandbox's
// agent, as the record to keep once it is written: the newest the agent's
// bundle declares it reads. Where that bundle cannot be read for a sandbox
// from before the record, it is the legacy file its agent booted on, which is
// recorded at once. A sandbox whose pool places no agent keeps the one
// placed in its container before the agent was a bundle, which reads the
// legacy version alone.
func (r *sandboxCore) sandboxConfigFor(ctx context.Context, sandboxID string) (sandboxConfigRecord, error) {
	legacy := sandboxConfigRecord{APIVersion: sandboxconfig.LegacyAPIVersion, Writes: sandboxconfig.LegacyAPIVersion}
	if r.agentLayers == nil {
		return legacy, nil
	}
	layer, ok, err := r.agentLayers.AgentLayer(ctx, sandboxID)
	if err != nil || !ok {
		return legacy, err
	}
	record, ok, err := r.readSandboxConfig(sandboxID)
	if err != nil {
		return sandboxConfigRecord{}, err
	}
	if ok && record.Layer == layer {
		return sandboxConfigRecord{APIVersion: record.Writes, Layer: layer, Writes: record.Writes}, nil
	}
	reads, _, err := r.agentLayers.AgentReads(ctx, sandboxID)
	if err != nil {
		// The agent that booted on the legacy file a pool wrote before the
		// record reads it, whatever else it reads. That is recorded, as the
		// legacy file it holds, so the bundle is not asked again at every
		// check of a running machine's placement.
		booted, bootedErr := r.bootedOnLegacy(sandboxID, layer, ok)
		if bootedErr != nil {
			return sandboxConfigRecord{}, bootedErr
		}
		if !booted {
			return sandboxConfigRecord{}, fmt.Errorf("what sandbox %s's agent reads: %w", sandboxID, err)
		}
		slog.WarnContext(ctx, "the agent's bundle is unreadable, so the legacy sandbox.json it booted on is kept", "sandboxId", sandboxID, "error", err)
		fallback := sandboxConfigRecord{APIVersion: sandboxconfig.LegacyAPIVersion, Layer: layer, Writes: sandboxconfig.LegacyAPIVersion}
		return fallback, r.recordSandboxConfig(sandboxID, fallback)
	}
	version, err := sandboxconfig.WriteAPIVersion(reads)
	if err != nil {
		return sandboxConfigRecord{}, fmt.Errorf("sandbox %s: %w", sandboxID, err)
	}
	return sandboxConfigRecord{APIVersion: version, Layer: layer, Writes: version}, nil
}

// bootedOnLegacy reports whether layer's agent booted on the sandbox's
// sandbox.json as a pool wrote it before the record (recorded false): it is
// the layer last placed in the sandbox, and that file holds the legacy
// version.
func (r *sandboxCore) bootedOnLegacy(sandboxID, layer string, recorded bool) (bool, error) {
	if recorded {
		return false, nil
	}
	placed, ok, err := r.readPlaced(sandboxID)
	return ok && placed.Layer == layer, err
}

// agentReadsWritten reports whether the agent the sandbox is now pinned to
// reads the sandbox.json its container holds. One that does not cannot boot
// on it, so its container is rebuilt, which writes one it reads.
func (r *sandboxCore) agentReadsWritten(ctx context.Context, sandboxID string) (bool, error) {
	record, recorded, err := r.readSandboxConfig(sandboxID)
	if err != nil {
		return false, err
	}
	written := sandboxconfig.LegacyAPIVersion
	if recorded {
		written = record.APIVersion
	}
	if r.agentLayers == nil {
		return written == sandboxconfig.LegacyAPIVersion, nil
	}
	layer, ok, err := r.agentLayers.AgentLayer(ctx, sandboxID)
	if err != nil {
		return false, err
	}
	if !ok {
		return written == sandboxconfig.LegacyAPIVersion, nil
	}
	if recorded && record.Layer == layer {
		return true, nil
	}
	if booted, err := r.bootedOnLegacy(sandboxID, layer, recorded); err != nil || booted {
		return booted, err
	}
	reads, _, err := r.agentLayers.AgentReads(ctx, sandboxID)
	if err != nil {
		return false, fmt.Errorf("what sandbox %s's agent reads: %w", sandboxID, err)
	}
	if !slices.Contains(reads, written) {
		return false, nil
	}
	writes, err := sandboxconfig.WriteAPIVersion(reads)
	if err != nil {
		return false, fmt.Errorf("sandbox %s: %w", sandboxID, err)
	}
	// This layer reads the file too, which the next create need not ask its
	// bundle again, and the next file written for it is its newest.
	return true, r.recordSandboxConfig(sandboxID, sandboxConfigRecord{APIVersion: written, Layer: layer, Writes: writes})
}
