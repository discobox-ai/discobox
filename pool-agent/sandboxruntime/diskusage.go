package sandboxruntime

import (
	"context"
	"fmt"

	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/proxy"
)

// AuditPurger removes one sandbox's audit trail from the pool proxy: its rows
// and its recorded bodies and streams (ADR 26-11-14-896 §5). The pool's
// proxy control client is one (proxy.ControlClient.PurgeClient).
type AuditPurger interface {
	PurgeClient(ctx context.Context, sandboxID string) (proxy.AuditPurgeResult, error)
}

// purgeAudit takes the sandbox's audit trail from the pool proxy, as part of
// the durable removal a purge confirms: a proxy that cannot be reached fails
// the delete, and the purge's reconcile retries it (ADR 0022 §3). A sandbox
// with nothing recorded purges to nothing, so a repeated delete succeeds.
func (r *sandboxCore) purgeAudit(ctx context.Context, sandboxID string) error {
	if _, err := r.audit.PurgeClient(ctx, sandboxID); err != nil {
		return fmt.Errorf("remove sandbox %s's audit trail: %w", sandboxID, err)
	}
	return nil
}

// DiskUsage is what a runtime's own store holds for its pool, beyond the
// trees the storage sweep walks (ADR 26-11-14-896 §§1-3).
type DiskUsage struct {
	// Measured is whether the runtime keeps anything of this pool's in a
	// store of its own on this pool's disk. A runtime whose boxes are
	// elsewhere (a machine pool's, on its driver's provider) has nothing to
	// measure, and every figure below is absent rather than zero.
	Measured bool
	// Layers is each box's writable layer, by sandbox ID. A box with no
	// container has none.
	Layers map[string]int64
	// ImageBytes is the images that are Discobox's, each layer counted once.
	ImageBytes int64
	// OtherLayerBytes is the writable layers of this pool's containers that
	// are no box's, such as a snapshot build's.
	OtherLayerBytes int64
	// DaemonShared is whether the store is shared with anything other than
	// this pool, so that it may be on another filesystem and hold what is
	// not Discobox's (ADR 26-11-14-896 §4).
	DaemonShared bool
}

// DiskUsage asks the daemon what it holds for this pool, as `docker system
// df` does: the daemon's own accounting rather than a walk of its data root,
// which the agent cannot see on a shared daemon and which double counts
// shared layers on overlay2.
//
// On a dedicated daemon everything on it is the pool's: every container,
// the pool's own included, and every image. On a shared one, a container is
// the pool's when it carries the pool's labels, and an image is Discobox's
// when it carries harness.ReclaimLabel (a snapshot does) or one of the
// pool's containers runs it. A shared daemon's images are an estimate: the
// daemon names no image's layers, only how many of its bytes it shares with
// any other image, so the figure is each image's unique bytes and the largest
// such share among them. It is high where that share is with an image that is
// not Discobox's, and low where Discobox's images share two different bases.
// Nothing subtracts it from the filesystem there (ADR 26-11-14-896 §4), so
// it is a magnitude to read, not a balance.
func (r *DockerSandboxRuntime) DiskUsage(ctx context.Context) (DiskUsage, error) {
	df, err := r.client.DiskUsage(ctx, client.DiskUsageOptions{Containers: true, Images: true, Verbose: true})
	if err != nil {
		return DiskUsage{}, fmt.Errorf("read the daemon's disk usage: %w", err)
	}
	usage := DiskUsage{Measured: true, Layers: map[string]int64{}, DaemonShared: !r.dedicatedDaemon}
	run := map[string]struct{}{}
	for _, ctr := range df.Containers.Items {
		ours := r.dedicatedDaemon ||
			(ctr.Labels[sandboxLabelProject] == r.projectID && ctr.Labels[sandboxLabelPool] == r.poolID)
		if !ours {
			continue
		}
		run[ctr.ImageID] = struct{}{}
		if sandboxID := ctr.Labels[sandboxLabelSandbox]; sandboxID != "" && ctr.Labels[sandboxLabelManaged] == "true" {
			usage.Layers[sandboxID] += ctr.SizeRw
			continue
		}
		usage.OtherLayerBytes += ctr.SizeRw
	}
	if r.dedicatedDaemon {
		usage.ImageBytes = df.Images.TotalSize
		return usage, nil
	}
	var shared int64
	for _, img := range df.Images.Items {
		_, running := run[img.ID]
		if !running && img.Labels[harness.ReclaimLabel] != harness.ReclaimLabelValue {
			continue
		}
		// SharedSize is -1 where the daemon did not compute it.
		imgShared := max(img.SharedSize, 0)
		usage.ImageBytes += max(img.Size-imgShared, 0)
		shared = max(shared, imgShared)
	}
	usage.ImageBytes += shared
	return usage, nil
}

// DiskUsage measures nothing for a machine pool: its boxes are machines on
// its driver's provider, not on this pool's disk, and it has no images. No
// driver a machine pool runs keeps a machine on the pool's own disk.
func (r *MachineSandboxRuntime) DiskUsage(context.Context) (DiskUsage, error) {
	return DiskUsage{}, nil
}

// DiskUsage is what a test set with SetDiskUsage, and nothing otherwise.
func (r *MemorySandboxRuntime) DiskUsage(context.Context) (DiskUsage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.diskUsage, r.diskUsageErr
}

// SetDiskUsage sets what DiskUsage answers.
func (r *MemorySandboxRuntime) SetDiskUsage(usage DiskUsage, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.diskUsage, r.diskUsageErr = usage, err
}
