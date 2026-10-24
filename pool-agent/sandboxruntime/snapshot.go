package sandboxruntime

import (
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

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pool-agent/assetcache"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
	"github.com/discobox-ai/discobox/proxy/egress"
)

// A snapshot is a box provisioned for no sandbox, kept so a sandbox wanting
// the same plan starts from it (ADR 26-10-16-423 §8). On this runtime it is
// an image committed from a build container: the sandbox base image, booted
// with the agent placed and a snapshot plan (agentlayer.SnapshotPlan) where a
// sandbox's bootstrap would be, provisioned with every step blocking, powered
// off, and committed. It is only ever a cache: a delivered create finding no
// snapshot for its plan runs the base image and provisions from scratch, as
// before, and asks for the snapshot to be built behind it, so the next create
// of the plan finds it. The control plane never knows which a box started
// from.
//
// A build is anonymous. Its proxy identity is its own (layout.SnapshotIDPrefix), its
// egress is the create's provisionEgress — every level but the sandbox's own
// — and its plan names no sandbox, account, source, secret, gateway or
// credentials endpoint, so nothing of the create that asked for it is in it.

const (
	// snapshotRepository holds every snapshot, each tagged with its key, so
	// a snapshot is found by name and the image reclaim, which keeps the
	// newest of a repository, keeps the one built last whatever its age.
	snapshotRepository = "discobox-snapshot"
	// snapshotBaseLabel is, on a snapshot, the ID of the sandbox base image
	// it was built from: a container started from it runs that base.
	snapshotBaseLabel = "io.discobox.snapshot.base.v1"
	// snapshotAgentLabel is, on a snapshot, the agent layer it holds, which
	// a start need not place again.
	snapshotAgentLabel = "io.discobox.snapshot.agent-layer.v1"
	// snapshotContainerPrefix names every build container, which is how a
	// build is found: a build's labels reach every container started from
	// its snapshot, so it carries only its project's and pool's, which every
	// container of the pool carries, and none saying it is a build.
	snapshotContainerPrefix = "discobox-snapshot-"
	// snapshotBuildTimeout bounds a build, which provisions everything a
	// fresh box does in the background — Docker, Chromium, Nix, Go,
	// Homebrew — with every step blocking.
	snapshotBuildTimeout = 45 * time.Minute
	// snapshotRetryAfter is how long a plan whose build failed waits before
	// a create asks for it again.
	snapshotRetryAfter = time.Hour
)

// snapshotBuilds is the builds under way and the plans whose builds failed.
type snapshotBuilds struct {
	mu sync.Mutex
	// active is the builds asked for and not yet ended, by key, each with
	// what ends it early: a cache clear does.
	active map[string]context.CancelFunc
	failed map[string]time.Time
	// one runs a build at a time: each is as heavy as a fresh box.
	one sync.Mutex
}

// snapshotPlan is what a delivered create is provisioned with, as a snapshot
// is keyed: everything a snapshot of it holds depends on, and nothing of the
// sandbox.
type snapshotPlan struct {
	// Base is the sandbox base image's ID.
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

// Key is the plan's digest's hex: the snapshot's tag.
func (p snapshotPlan) Key() string {
	data, err := json.Marshal(p)
	if err != nil {
		panic(err) // A struct of strings and slices always marshals.
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (p snapshotPlan) reference() string { return snapshotRepository + ":" + p.Key() }

// buildID is the build's identity, which its proxy client certificate names.
func (p snapshotPlan) buildID() string { return snapshotBuildID(p.Key()) }

// snapshotBuildID is the build identity of the plan keyed key.
func snapshotBuildID(key string) string { return layout.SnapshotIDPrefix + key[:24] }

// snapshotContainerName is the container of the build whose identity is id.
func (r *DockerSandboxRuntime) snapshotContainerName(id string) string {
	return snapshotContainerPrefix + r.poolID + "-" + strings.TrimPrefix(id, layout.SnapshotIDPrefix)
}

// newSnapshotPlan is the plan of a delivered create on the base image baseID.
func (r *DockerSandboxRuntime) newSnapshotPlan(baseID string, req *workerapimodel.PoolSandboxCreateRequest) (snapshotPlan, error) {
	delivered := delivery(req)
	pins, _, err := applied(delivered)
	if err != nil {
		return snapshotPlan{}, err
	}
	plan := snapshotPlan{
		Base:      baseID,
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

// deliveredImage is the image a delivered create's container runs: the
// snapshot of its plan when there is one, and otherwise the sandbox base
// image, with the snapshot asked for behind it. baseID is the base's ID.
func (r *DockerSandboxRuntime) deliveredImage(ctx context.Context, baseID string, req *workerapimodel.PoolSandboxCreateRequest) string {
	if !req.ProvisionEgress.Set {
		// A server that sends no build policy gets no snapshot: built
		// without one, it would run unrestricted.
		return baseID
	}
	plan, err := r.newSnapshotPlan(baseID, req)
	if err != nil {
		slog.WarnContext(ctx, "plan a snapshot; starting from the sandbox base image", "error", err)
		return baseID
	}
	inspected, err := r.client.ImageInspect(ctx, plan.reference())
	switch {
	case err == nil && inspected.Config != nil && inspected.Config.Labels[snapshotBaseLabel] == baseID:
		slog.InfoContext(ctx, "starting from a snapshot", "snapshot", plan.reference(), "sandboxId", req.SandboxId)
		return inspected.ID
	case err != nil && !cerrdefs.IsNotFound(err):
		slog.WarnContext(ctx, "look up a snapshot; starting from the sandbox base image", "snapshot", plan.reference(), "error", err)
		return baseID
	}
	r.requestSnapshot(plan)
	return baseID
}

// requestSnapshot builds plan's snapshot in the background, unless one is
// being built or failed within snapshotRetryAfter.
func (r *DockerSandboxRuntime) requestSnapshot(plan snapshotPlan) {
	key := plan.Key()
	b := &r.snapshots
	b.mu.Lock()
	if b.active == nil {
		b.active, b.failed = map[string]context.CancelFunc{}, map[string]time.Time{}
	}
	if _, building := b.active[key]; building || time.Since(b.failed[key]) < snapshotRetryAfter {
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
			// It starts a container as a create does, so it waits out a
			// cache clear and holds the next one off (clearcache.go).
			leave, err := r.starts.enter(ctx)
			if err != nil {
				return err
			}
			defer leave()
			slog.Info("building a snapshot", "snapshot", plan.reference())
			return r.buildSnapshot(ctx, plan)
		}()
		b.mu.Lock()
		delete(b.active, key)
		switch {
		case errors.Is(ctx.Err(), context.Canceled):
			// Ended by a clear: the next create asks again.
		case err != nil:
			b.failed[key] = time.Now()
		default:
			delete(b.failed, key)
		}
		b.mu.Unlock()
		if err != nil {
			slog.Warn("build a snapshot; its plan provisions from scratch", "snapshot", plan.reference(), "error", err, "retryAfter", snapshotRetryAfter)
			return
		}
		slog.Info("built a snapshot", "snapshot", plan.reference(), "took", time.Since(started).Round(time.Second))
	}()
}

// snapshotsDir holds each build's tree (layout.Root.PoolSnapshots): its asset
// record, its registry namespace, and the directory bound at
// agentlayer.SnapshotDir.
func (r *DockerSandboxRuntime) snapshotsDir() string {
	return r.root.PoolSnapshots(r.projectID, r.poolID)
}

// buildSnapshot boots the base image with plan, waits for its provisioning
// to power it off, and commits it as plan's snapshot. Everything it made on
// the way — the container, the build's tree, its proxy material and policy —
// is removed whatever the outcome.
func (r *DockerSandboxRuntime) buildSnapshot(ctx context.Context, plan snapshotPlan) (err error) {
	id := plan.buildID()
	dir := r.root.Snapshot(r.projectID, r.poolID, id)
	name := r.snapshotContainerName(id)
	defer func() {
		err = errors.Join(err, r.removeSnapshotBuild(context.WithoutCancel(ctx), id))
	}()
	if _, err := r.client.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove an earlier build container: %w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	etc := filepath.Join(dir, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		return err
	}
	// What assets.discobox.internal answers the build, as a sandbox's record
	// is what it answers the sandbox.
	if err := assetcache.WriteRecord(dir, plan.delivered); err != nil {
		return err
	}
	material, err := proxyagent.EnsureSandboxMaterial(r.root, r.projectID, r.poolID, id)
	if err != nil {
		return err
	}
	if err := proxyagent.SetSandboxEgress(r.root, r.projectID, r.poolID, id, plan.Egress); err != nil {
		return err
	}
	agent, ok, err := r.sandboxAgent(ctx, id)
	if err != nil {
		return fmt.Errorf("the agent of pins %v: %w", plan.Pins, err)
	}
	if !ok {
		return errors.New("this pool has no asset cache to place the agent from")
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
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	// The client key is in it, so it is root's alone, as the material is.
	if err := os.WriteFile(filepath.Join(etc, filepath.Base(agentlayer.SnapshotPlanFile)), data, 0o600); err != nil {
		return err
	}
	// Bound, never copied in: nothing under it is in the container's own
	// filesystem, so none of it is committed. It is the build's alone, and
	// none of a sandbox's volumes is mounted: a build provisions as an
	// image is built, onto the root filesystem the snapshot keeps.
	hostCfg, netCfg := r.boxConfig([]mount.Mount{{Type: mount.TypeBind, Source: r.daemonPath(etc), Target: agentlayer.SnapshotDir}})
	created, err := r.client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:    plan.Base,
			Hostname: "discobox-snapshot",
			// So the reaping of a pool that is gone finds it (SyncKnownPools).
			Labels:     map[string]string{sandboxLabelProject: r.projectID, sandboxLabelPool: r.poolID},
			Entrypoint: []string{"/sbin/init"},
			Cmd:        []string{},
			Tty:        true,
		},
		HostConfig:       hostCfg,
		NetworkingConfig: netCfg,
	})
	if err != nil {
		return fmt.Errorf("create the build container: %w", err)
	}
	reader, writer := io.Pipe()
	go func() { writer.CloseWithError(agentlayer.Archive(writer, agent, false, plan.Pins, agentlayer.Units)) }()
	_, err = r.client.CopyToContainer(ctx, created.ID, client.CopyToContainerOptions{DestinationPath: "/", Content: reader})
	reader.Close()
	if err != nil {
		return fmt.Errorf("place the agent in the build container: %w", err)
	}
	waited := r.client.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNextExit})
	if _, err := r.client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start the build container: %w", err)
	}
	select {
	case <-waited.Result:
	case err := <-waited.Error:
		return fmt.Errorf("wait for the build to power off: %w", err)
	}
	result, err := readSnapshotResult(filepath.Join(etc, filepath.Base(agentlayer.SnapshotResultFile)))
	if err != nil {
		return err
	}
	if result.Error != "" {
		return fmt.Errorf("its provisioning failed: %s", result.Error)
	}
	labels := map[string]string{
		harness.ReclaimLabel: harness.ReclaimLabelValue,
		snapshotBaseLabel:    plan.Base,
		snapshotAgentLabel:   agent.Digest,
	}
	if _, err := r.client.ContainerCommit(ctx, created.ID, client.ContainerCommitOptions{
		Reference: plan.reference(),
		Comment:   "a Discobox snapshot (ADR 26-10-16-423 §8)",
		Config:    &container.Config{Labels: labels},
	}); err != nil {
		return fmt.Errorf("commit the snapshot: %w", err)
	}
	return nil
}

// readSnapshotResult is the result the build wrote; none is a build that
// powered off before it finished.
func readSnapshotResult(path string) (agentlayer.SnapshotResult, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return agentlayer.SnapshotResult{}, errors.New("the build stopped without a result")
	}
	if err != nil {
		return agentlayer.SnapshotResult{}, err
	}
	var result agentlayer.SnapshotResult
	if err := json.Unmarshal(data, &result); err != nil {
		return agentlayer.SnapshotResult{}, fmt.Errorf("decode the build's result: %w", err)
	}
	return result, nil
}

// snapshotOf is what a container's image says of the snapshot it is: the
// base it was built from and the agent layer it holds, both empty for an
// image that is no snapshot.
func (r *DockerSandboxRuntime) snapshotOf(ctx context.Context, imageID string) (base, agentLayer string, err error) {
	inspected, err := r.client.ImageInspect(ctx, imageID)
	if err != nil {
		return "", "", err
	}
	if inspected.Config == nil {
		return "", "", nil
	}
	labels := inspected.Config.Labels
	if !slices.ContainsFunc(inspected.RepoTags, func(tag string) bool { return strings.HasPrefix(tag, snapshotRepository+":") }) {
		return "", "", nil
	}
	return labels[snapshotBaseLabel], labels[snapshotAgentLabel], nil
}

// sweepSnapshotBuilds removes the build containers and trees no build under
// way holds — what a pool agent that stopped mid-build left, whose container
// may still be running — with their proxy material, so none is left
// provisioning under a policy the pool no longer holds for it. A build's tree
// holds its asset record, so the sweep goes before the blob sweep reads it.
func (r *DockerSandboxRuntime) sweepSnapshotBuilds(ctx context.Context) error {
	r.snapshots.mu.Lock()
	defer r.snapshots.mu.Unlock()
	// A build holds its container and tree for its whole length; a key in
	// active is one asked for, whatever it is doing now.
	keep := map[string]bool{}
	for key := range r.snapshots.active {
		keep[snapshotBuildID(key)] = true
	}
	entries, err := os.ReadDir(r.snapshotsDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	var errs []error
	// Each tree no build holds goes whole, its container first.
	named := map[string]bool{}
	for _, entry := range entries {
		named["/"+r.snapshotContainerName(entry.Name())] = true
		if !keep[entry.Name()] {
			errs = append(errs, r.removeSnapshotBuild(ctx, entry.Name()))
		}
	}
	for id := range keep {
		named["/"+r.snapshotContainerName(id)] = true
	}
	// And a build container with no tree, which holds nothing else.
	containers, err := r.client.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: r.snapshotBuildFilters().Add("label", sandboxLabelPool+"="+r.poolID)})
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, ctr := range containers.Items {
		if slices.ContainsFunc(ctr.Names, func(name string) bool { return named[name] }) {
			continue
		}
		if _, err := r.client.ContainerRemove(ctx, ctr.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// imageHoldsAgent reports whether the container's image is a snapshot
// holding the agent layer digest.
func (r *DockerSandboxRuntime) imageHoldsAgent(ctx context.Context, containerID, digest string) (bool, error) {
	inspect, err := r.client.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return false, err
	}
	_, layer, err := r.snapshotOf(ctx, inspect.Container.Image)
	if err != nil {
		return false, err
	}
	return layer == digest, nil
}

// removeSnapshotBuild removes the build id: its container, then its egress
// policy and proxy material, then its tree, each only once what goes before
// it is gone. Its policy stays while its container may still run, since an
// identity with no egress entry is unrestricted, and its tree stays until
// its policy and material are gone, since the tree is what the next sweep
// finds a build by to try again.
func (r *DockerSandboxRuntime) removeSnapshotBuild(ctx context.Context, id string) error {
	if _, err := r.client.ContainerRemove(ctx, r.snapshotContainerName(id), client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove build %s's container: %w", id, err)
	}
	if err := errors.Join(
		proxyagent.RemoveSandboxPolicy(r.root, r.projectID, r.poolID, id),
		proxyagent.RemoveSandboxMaterial(r.root, r.projectID, r.poolID, id),
	); err != nil {
		return fmt.Errorf("remove build %s's proxy record: %w", id, err)
	}
	return os.RemoveAll(r.root.Snapshot(r.projectID, r.poolID, id))
}

// cancelSnapshotBuilds ends every build asked for, as a cache clear does.
func (r *DockerSandboxRuntime) cancelSnapshotBuilds() {
	r.snapshots.mu.Lock()
	defer r.snapshots.mu.Unlock()
	for _, cancel := range r.snapshots.active {
		cancel()
	}
}

// clearSnapshots removes this pool's snapshots no container runs, and forgets
// which plans failed to build, as a cache clear does: a box that came up
// wrong from one starts from scratch next.
func (r *DockerSandboxRuntime) clearSnapshots(ctx context.Context) error {
	r.snapshots.mu.Lock()
	clear(r.snapshots.failed)
	r.snapshots.mu.Unlock()
	images, err := r.client.ImageList(ctx, client.ImageListOptions{Filters: client.Filters{}.
		Add("reference", snapshotRepository).
		Add("label", sandboxLabelPool+"="+r.poolID)})
	if err != nil {
		return err
	}
	var errs []error
	for _, image := range images.Items {
		// Not forced: one a container runs is refused, and kept.
		if _, err := r.client.ImageRemove(ctx, image.ID, client.ImageRemoveOptions{}); err != nil && !cerrdefs.IsNotFound(err) && !cerrdefs.IsConflict(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// snapshotBuildFilters finds this project's build containers, on a daemon
// its pools may share.
func (r *DockerSandboxRuntime) snapshotBuildFilters() client.Filters {
	return client.Filters{}.Add("name", snapshotContainerPrefix).Add("label", sandboxLabelProject+"="+r.projectID)
}
