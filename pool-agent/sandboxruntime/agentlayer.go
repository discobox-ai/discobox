package sandboxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	"golang.org/x/sync/singleflight"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pool-agent/assetcache"
)

// A sandbox its pool delivers the platform to — one pinned to a platform
// bundle — runs the provider's sandbox base image, and the agent reaches it
// as a layer of that bundle (ADR 26-10-16-423 §7). Before every start of its
// container this runtime places, through the daemon, the agent layer when the
// box lacks it, the agent's units, and the pins the box applies
// (agentlayer.Archive). The box fetches everything else into its store from
// assets.discobox.internal itself.
//
// Nothing here is bound into a box: the layer is copied into the container's
// own filesystem, which is the box's to keep. The extracted agent on this
// side, under the pool's build area, is what the export mode runs, bound
// read-only into a container that is not the box (runExportMode).

// AgentLayers is what the runtime reads an agent layer through: the pool's
// asset cache, answering only the layer of a sandbox's own agent pin.
type AgentLayers interface {
	AgentLayer(ctx context.Context, sandboxID string) (string, bool, error)
	OpenAgentLayer(ctx context.Context, sandboxID, digest string) (io.ReadCloser, error)
}

// agentBinaryName is the agent, extracted from its layer, in agentsDir.
const agentBinaryName = "discobox-sandbox-agent"

// agentExtract is the agents extracted on this side, one fetch per layer
// however many sandboxes start on it at once.
type agentExtract struct {
	group singleflight.Group
	// mu serializes removing what no sandbox's platform reaches with an
	// extraction, so a sweep never takes a layer mid-extraction.
	mu sync.Mutex
}

// agentsDir holds each agent layer's agent, by layer digest, in the pool's
// build area: never bound into a box (ADR 0050).
func (r *sandboxCore) agentsDir() string {
	return filepath.Join(r.root.PoolBuild(r.projectID, r.poolID), "agents")
}

// deliveredPins is the sandbox's recorded bundles as the box applies them,
// and false when the sandbox is delivered nothing.
func (r *sandboxCore) deliveredPins(sandboxID string) (agentlayer.Pins, bool, error) {
	d, err := assetcache.ReadRecord(r.sandboxRoot(sandboxID))
	if err != nil {
		return nil, false, err
	}
	return applied(d)
}

// applied is the bundles d delivers as the box applies them, in order, and
// false when the pool places nothing in the box: d delivers nothing, or names
// no agent, as a sandbox recorded before the agent was a bundle does, whose
// container keeps the agent placed in it then (assetcache.Delivery.PlacesAgent).
func applied(d assetcache.Delivery) (agentlayer.Pins, bool, error) {
	if !d.PlacesAgent() {
		return nil, false, nil
	}
	out := make(agentlayer.Pins, 0, len(d.Bundles))
	for _, pin := range d.Bundles {
		out = append(out, agentlayer.Pin{Name: pin.Name, Digest: pin.Digest})
	}
	return out, true, out.Validate()
}

// sandboxAgent is the sandbox's agent layer, extracted on this side, and
// false for a sandbox delivered no platform.
func (r *sandboxCore) sandboxAgent(ctx context.Context, sandboxID string) (agentlayer.Agent, bool, error) {
	if r.agentLayers == nil {
		return agentlayer.Agent{}, false, nil
	}
	digest, ok, err := r.agentLayers.AgentLayer(ctx, sandboxID)
	if err != nil || !ok {
		return agentlayer.Agent{}, ok, err
	}
	path := filepath.Join(r.agentsDir(), agentlayer.Hex(digest), agentBinaryName)
	if _, err, _ := r.agents.group.Do(digest, func() (any, error) {
		r.agents.mu.Lock()
		defer r.agents.mu.Unlock()
		if _, err := os.Stat(path); err == nil {
			return nil, nil
		}
		return nil, r.extractAgent(ctx, sandboxID, digest, path)
	}); err != nil {
		return agentlayer.Agent{}, true, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return agentlayer.Agent{}, true, err
	}
	return agentlayer.Agent{
		Digest: digest,
		Size:   info.Size(),
		Open:   func() (io.ReadCloser, error) { return os.Open(path) },
	}, true, nil
}

// extractAgent reads the layer digest and writes its agent to path,
// refusing a layer that is not the agent's alone (agentlayer.ExtractAgent).
func (r *sandboxCore) extractAgent(ctx context.Context, sandboxID, digest, path string) error {
	layer, err := r.agentLayers.OpenAgentLayer(ctx, sandboxID, digest)
	if err != nil {
		return err
	}
	defer layer.Close()
	if err := agentlayer.ExtractAgent(layer, path); err != nil {
		return fmt.Errorf("layer %s: %w", digest, err)
	}
	return nil
}

// placeAgent places the agent in the sandbox's container before it starts:
// the layer when the pool has not placed it in this container, and its units
// and pins always, recording what it placed (placedRecord). A sandbox whose
// pool places no agent in it (applied) is left as it is.
func (r *DockerSandboxRuntime) placeAgent(ctx context.Context, sandboxID, containerID string) error {
	pins, ok, err := r.deliveredPins(sandboxID)
	if err != nil || !ok {
		return err
	}
	agent, ok, err := r.sandboxAgent(ctx, sandboxID)
	if err != nil {
		return fmt.Errorf("the agent of sandbox %s's pins %v: %w", sandboxID, pins, err)
	}
	if !ok {
		return fmt.Errorf("sandbox %s is pinned to %v, and this pool has no asset cache to place its agent from", sandboxID, pins)
	}
	// The layer goes in unless the pool's own record says it placed this
	// one in this container already; nothing in the box is asked.
	record, _, err := r.readPlaced(sandboxID)
	if err != nil {
		return err
	}
	placed := record.Container == containerID && record.Layer == agent.Digest
	if !placed {
		// A snapshot holds the layer it was built with.
		placed, err = r.imageHoldsAgent(ctx, containerID, agent.Digest)
		if err != nil {
			return err
		}
	}
	reader, writer := io.Pipe()
	go func() {
		writer.CloseWithError(agentlayer.Archive(writer, agent, placed, pins, agentlayer.Units))
	}()
	defer reader.Close()
	if _, err := r.client.CopyToContainer(ctx, containerID, client.CopyToContainerOptions{DestinationPath: "/", Content: reader}); err != nil {
		return fmt.Errorf("place the agent in sandbox %s: %w", sandboxID, err)
	}
	return r.writePlaced(sandboxID, placedRecord{Container: containerID, Layer: agent.Digest, Pins: pins})
}

// sweepAgents removes the agents extracted for layers no sandbox's platform
// reaches any more. What to keep is decided under the lock every extraction
// takes, so an agent extracted for a create that recorded its pins after the
// sweep began is kept. A sandbox whose layer cannot be named now keeps every
// agent, since any could be its; agentSweepGrace keeps one extracted too
// recently for its sandbox's record to say so.
func (r *sandboxCore) sweepAgents(ctx context.Context, sandboxIDs []string) error {
	if r.agentLayers == nil {
		return nil
	}
	r.agents.mu.Lock()
	defer r.agents.mu.Unlock()
	keep := map[string]bool{}
	for _, id := range sandboxIDs {
		digest, ok, err := r.agentLayers.AgentLayer(ctx, id)
		if err != nil {
			return fmt.Errorf("the agent layer of sandbox %s, which keeps every agent: %w", id, err)
		}
		if ok {
			keep[agentlayer.Hex(digest)] = true
		}
	}
	entries, err := os.ReadDir(r.agentsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || keep[entry.Name()] || time.Since(info.ModTime()) < agentSweepGrace {
			continue
		}
		if err := os.RemoveAll(filepath.Join(r.agentsDir(), entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// agentSweepGrace is how long an extracted agent is kept whatever the
// records say.
const agentSweepGrace = time.Hour

// EnvSandboxBaseImage carries the provider's sandbox base image (ADR
// 26-10-16-423 §10) from its provider instance into the pool agent, which
// runs it for every sandbox delivered its platform. The provider preloads it
// into the pool's daemon.
const EnvSandboxBaseImage = "DISCOBOX_SANDBOX_BASE_IMAGE"

// placedRecordName is, per sandbox, what the pool last placed in its
// container (placedRecord): its own record, beside the tree and never in a
// volume, so nothing the box writes decides what the pool places or whether
// it restarts the box (pool-agent/REVIEW.md).
const placedRecordName = "agent-placed.json"

// placedRecord is what the pool last copied into a sandbox's box: a
// container (placeAgent) or a machine (MachineSandboxRuntime.place).
type placedRecord struct {
	// Container is the box's own ID: a container's, or a machine's instance.
	Container string          `json:"container"`
	Layer     string          `json:"layer"`
	Pins      agentlayer.Pins `json:"pins"`
	// Bootstrap is the digest of the sandbox.json placed beside them, which
	// only a machine is placed: a container's is in its config volume.
	Bootstrap string `json:"bootstrap,omitempty"`
}

func (r *sandboxCore) placedRecordPath(sandboxID string) string {
	return filepath.Join(r.sandboxRoot(sandboxID), placedRecordName)
}

func (r *sandboxCore) readPlaced(sandboxID string) (placedRecord, bool, error) {
	data, err := os.ReadFile(r.placedRecordPath(sandboxID))
	if errors.Is(err, os.ErrNotExist) {
		return placedRecord{}, false, nil
	}
	if err != nil {
		return placedRecord{}, false, err
	}
	var record placedRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return placedRecord{}, false, fmt.Errorf("decode %s: %w", placedRecordName, err)
	}
	return record, true, nil
}

func (r *sandboxCore) writePlaced(sandboxID string, record placedRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	path := r.placedRecordPath(sandboxID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// deliveredMove is what moved under a delivered sandbox's container since
// the pool last started it.
type deliveredMove int

const (
	movedNothing deliveredMove = iota
	// movedPins is pins other than the ones its container last booted on.
	movedPins
	// movedBase is moved pins and a sandbox base image other than the one
	// its container runs: only a new container takes the base.
	movedBase
)

// deliveredMoved compares a delivered create with what its container last
// booted on, as the pool recorded placing it: level-triggered, so a restart
// that failed or was cut short is made by the next create. A container the
// pool has no record of placing has moved, so it is started on what the
// create says. A create that delivers nothing has moved nothing.
func (r *DockerSandboxRuntime) deliveredMoved(ctx context.Context, existing *Sandbox, req *workerapimodel.PoolSandboxCreateRequest) (deliveredMove, error) {
	pins, ok, err := applied(delivery(req))
	if err != nil || !ok {
		return movedNothing, err
	}
	record, ok, err := r.readPlaced(existing.SandboxID)
	if err != nil {
		return movedNothing, err
	}
	if ok && record.Container == existing.ID && samePins(record.Pins, pins) {
		return movedNothing, nil
	}
	inspect, err := r.client.ContainerInspect(ctx, existing.ID, client.ContainerInspectOptions{})
	if err != nil {
		return movedNothing, err
	}
	base, err := r.client.ImageInspect(ctx, r.sandboxBaseImage)
	if err != nil {
		// A base image this pool cannot read now is no move of it: the
		// pins still move, by a restart.
		slog.WarnContext(ctx, "inspect the sandbox base image", "image", r.sandboxBaseImage, "error", err)
		return movedPins, nil
	}
	if inspect.Container.Image == base.ID {
		return movedPins, nil
	}
	// A container started from a snapshot runs the base it was built from.
	snapshotBase, _, err := r.snapshotOf(ctx, inspect.Container.Image)
	if err != nil && !cerrdefs.IsNotFound(err) {
		return movedNothing, err
	}
	if snapshotBase == base.ID {
		return movedPins, nil
	}
	return movedBase, nil
}

func samePins(a, b agentlayer.Pins) bool { return slices.Equal(a, b) }
