package sandboxruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pool-agent/provisioner"
)

// On a Docker pool a snapshot is an image committed from a build container:
// the sandbox base image, started with the agent placed and the plan bound at
// agentlayer.SnapshotDir, provisioned by the snapshot command run as an exec,
// stopped, and committed. SnapshotDir is a bind, so nothing under it is in
// the container's own filesystem.

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
)

func (p snapshotPlan) reference() string { return snapshotRepository + ":" + p.Key() }

// snapshotContainerName is the container of the build whose identity is id.
func (r *DockerSandboxRuntime) snapshotContainerName(id string) string {
	return snapshotContainerPrefix + r.poolID + "-" + strings.TrimPrefix(id, layout.SnapshotIDPrefix)
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
	r.requestSnapshot(plan, r.buildSnapshot)
	return baseID
}

// buildSnapshot starts the base image with plan, runs its provisioning as an
// exec, stops it and commits it as plan's snapshot. Everything it made on the
// way — the container, the build's tree, its proxy material and policy — is
// removed whatever the outcome.
func (r *DockerSandboxRuntime) buildSnapshot(ctx context.Context, plan snapshotPlan) (err error) {
	// It starts a container as a create does, so it waits out a cache clear
	// and holds the next one off (clearcache.go).
	leave, err := r.starts.enter(ctx)
	if err != nil {
		return err
	}
	defer leave()
	id := plan.buildID()
	name := r.snapshotContainerName(id)
	defer func() {
		err = errors.Join(err, r.removeSnapshotBuild(context.WithoutCancel(ctx), id))
	}()
	if _, err := r.client.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove an earlier build container: %w", err)
	}
	data, agent, err := r.prepareSnapshotBuild(ctx, plan)
	if err != nil {
		return err
	}
	etc := filepath.Join(r.root.Snapshot(r.projectID, r.poolID, id), "etc")
	if err := os.MkdirAll(etc, 0o700); err != nil {
		return err
	}
	// Bound, so the proxy material the plan lays there is never in the
	// container's own filesystem, nor committed. It is the build's alone, and
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
	if _, err := r.client.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("start the build container: %w", err)
	}
	if err := runSnapshotCommand(ctx, provisioner.ContainerGuest{Client: r.client, Container: created.ID, Stdin: true}, agent, plan.Pins, data); err != nil {
		return err
	}
	// Stopped in order, so what it wrote is on its filesystem whole.
	timeout := int(snapshotStopTimeout / time.Second)
	if _, err := r.client.ContainerStop(ctx, created.ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("stop the build container: %w", err)
	}
	labels := map[string]string{
		harness.ReclaimLabel: harness.ReclaimLabelValue,
		snapshotBaseLabel:    plan.Base,
		snapshotAgentLabel:   agent.Digest,
	}
	if _, err := r.client.ContainerCommit(ctx, created.ID, client.ContainerCommitOptions{
		Reference: plan.reference(),
		Comment:   "a Discobox snapshot (ADR 26-11-03-986)",
		Config:    &container.Config{Labels: labels},
	}); err != nil {
		return fmt.Errorf("commit the snapshot: %w", err)
	}
	return nil
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
	r.sweepSnapshotFailures(time.Now())
	if !r.lockSnapshotSweep() {
		return nil
	}
	defer r.snapshots.one.Unlock()
	// A build holds its container and tree for its whole length; a key being
	// built is one asked for, whatever it is doing now.
	keep := map[string]bool{}
	for key := range r.buildingSnapshots() {
		keep[snapshotBuildID(key)] = true
	}
	entries, err := os.ReadDir(r.snapshotsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
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

// removeSnapshotBuild removes the build id: its container, then what the
// pool laid for it (removeSnapshotBuildMaterial), only once the container is gone.
func (r *DockerSandboxRuntime) removeSnapshotBuild(ctx context.Context, id string) error {
	if _, err := r.client.ContainerRemove(ctx, r.snapshotContainerName(id), client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("remove build %s's container: %w", id, err)
	}
	return r.removeSnapshotBuildMaterial(id)
}

// clearSnapshots removes this pool's snapshots no container runs, and forgets
// which plans failed to build, as a cache clear does: a box that came up
// wrong from one starts from scratch next.
func (r *DockerSandboxRuntime) clearSnapshots(ctx context.Context) error {
	forgetErr := r.forgetSnapshotFailures()
	images, err := r.client.ImageList(ctx, client.ImageListOptions{Filters: client.Filters{}.
		Add("reference", snapshotRepository).
		Add("label", sandboxLabelPool+"="+r.poolID)})
	if err != nil {
		return errors.Join(forgetErr, err)
	}
	errs := []error{forgetErr}
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
