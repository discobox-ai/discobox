package pools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// Staging the images a sandbox will want onto a pool, as its own reconciled
// resource.
//
// It is deliberately not part of the pool's own reconcile, and deliberately not
// part of server startup, which is where it began. Two properties are what the
// separation buys:
//
// A pool whose images are not staged is still active and healthy. Staging is a
// head start, not a precondition — a sandbox that wants an image the pool does
// not have pulls it then, exactly as it always did. Making it a pool state
// would turn "this optimisation has not finished" into "this host cannot take
// work", which is false and would gate scheduling on a registry.
//
// And it is claimed and leased like anything else the engine runs. The first
// version called EnsurePool from server startup, outside the engine, where it
// could race the reconciler over the same pool's containers. Nothing here
// creates a pool; the reconcile that marks this dirty has already converged it.

// PoolImagesResourceType is the reconciled resource: one row per pool, holding
// the staging of that pool's image set.
const PoolImagesResourceType = "poolImages"

const (
	// imageStageRetryDelay is how long a failed stage waits before trying
	// again. A registry that is down, or an image that is not published yet,
	// resolves on its own or does not; either way this is not urgent work.
	imageStageRetryDelay = 5 * time.Minute
	// imageStageRefresh re-stages a converged pool this often, so a harness
	// config that gains a new image gets it staged without anyone asking.
	imageStageRefresh = 6 * time.Hour
)

// PoolImagesReconciler stages a pool's image set onto it.
type PoolImagesReconciler struct {
	store   *store.Store
	manager *sandbox.ProviderManager
}

func NewPoolImagesReconciler(appStore *store.Store, manager *sandbox.ProviderManager) *PoolImagesReconciler {
	return &PoolImagesReconciler{store: appStore, manager: manager}
}

// Reconcile stages the pool's images, recording what it is doing on the pool as
// it goes.
//
// A failure is recorded and retried, never returned as a reconcile error: the
// engine's failure backoff is for resources that must converge, and this one
// must not. Returning an error here would put a pool into a failing reconcile
// because a registry was briefly unreachable.
func (r *PoolImagesReconciler) Reconcile(ctx context.Context, poolID string) (reconcile.Result, error) {
	pool, err := r.store.GetPoolByID(ctx, poolID)
	if errors.Is(err, store.ErrNotFound) {
		// The pool is gone. Nothing to stage and nothing to record.
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	if pool.RevokedAt != nil || pool.DesiredState != model.DesiredStatePresent || !pool.Ready {
		// Not a host to pull onto yet. The pool's own reconcile marks this
		// dirty again when that changes.
		return reconcile.Result{}, nil
	}
	images, err := r.imageSet(ctx, pool.ProjectID)
	if err != nil {
		return reconcile.Result{}, err
	}
	if len(images) == 0 {
		return reconcile.Result{}, r.record(ctx, pool, model.PoolImageStage{State: model.PoolImageStateReady, Total: 0})
	}

	runtime, err := r.poolRuntime(ctx, pool)
	if err != nil {
		return reconcile.Result{}, err
	}
	if runtime == nil {
		// A disabled provider, or one with no pool runtime: no daemon to stage
		// onto, and nothing wrong.
		return reconcile.Result{}, nil
	}

	stage := model.PoolImageStage{State: model.PoolImageStateStaging, Total: len(images)}
	_ = r.record(ctx, pool, stage)
	stageErr := runtime.StageImages(ctx, pool, images, func(progress sandbox.PreloadProgress) {
		snapshot := model.PoolImageStage{
			State: model.PoolImageStateStaging,
			Image: progress.Image,
			Done:  progress.Done,
			Total: progress.Total,
		}
		if pull := progress.Pull; pull != nil {
			snapshot.Current, snapshot.Size = pull.Current, pull.Total
			snapshot.Layers, snapshot.LayersComplete = pull.Layers, pull.LayersComplete
		}
		_ = r.record(ctx, pool, snapshot)
	})
	if ctx.Err() != nil {
		return reconcile.Result{}, ctx.Err()
	}
	if stageErr != nil {
		_ = r.record(ctx, pool, model.PoolImageStage{
			State: model.PoolImageStateFailed,
			Total: len(images),
			Error: stageErr.Error(),
		})
		// Recorded and retried on this resource's own cadence, not returned:
		// the engine's failure backoff escalates a resource that must
		// converge, and this one must not.
		return reconcile.RequeueAfter(imageStageRetryDelay), nil //nolint:nilerr // the failure is the recorded condition, not a reconcile error
	}
	if err := r.record(ctx, pool, model.PoolImageStage{State: model.PoolImageStateReady, Done: len(images), Total: len(images)}); err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.RequeueAfter(imageStageRefresh), nil
}

func (r *PoolImagesReconciler) record(ctx context.Context, pool *model.Pool, stage model.PoolImageStage) error {
	encoded, err := json.Marshal(stage)
	if err != nil {
		return err
	}
	return r.store.RecordPoolImageStage(ctx, pool.ID, encoded, stage.State == model.PoolImageStateReady, time.Now().UTC())
}

func (r *PoolImagesReconciler) poolRuntime(ctx context.Context, pool *model.Pool) (sandbox.PoolRuntime, error) {
	if r.manager == nil {
		return nil, fmt.Errorf("sandbox provider manager is required")
	}
	instance, err := r.store.GetSandboxProviderInstance(ctx, pool.ProjectID, pool.ProviderInstanceID)
	if err != nil {
		return nil, err
	}
	if instance.Disabled {
		return nil, nil
	}
	provider, err := r.manager.ResolveInstance(ctx, instance)
	if err != nil {
		return nil, err
	}
	runtime, ok := provider.(sandbox.PoolRuntime)
	if !ok {
		// A provider with no pool runtime has no daemon to stage onto.
		return nil, nil
	}
	return runtime, nil
}

// imageSet is every image a sandbox on this project might run: the default
// sandbox image this server resolved, and the image of every harness config the
// project has.
//
// Read from the project's harness configs rather than from the built-in list,
// because a project can register its own and those are exactly as much of a
// first-run wait as the built-in three.
func (r *PoolImagesReconciler) imageSet(ctx context.Context, projectID string) ([]string, error) {
	configs, err := r.store.ListHarnessConfigs(ctx, projectID)
	if err != nil {
		return nil, err
	}
	images := make([]string, 0, len(configs)+1)
	images = append(images, defaultSandboxImage())
	for i := range configs {
		images = append(images, configs[i].Image)
	}
	return stageableImages(images), nil
}

// stageableImages is the set worth pulling: deduped, ordered, and without the
// local tags that exist on no registry — pulling one fails on every development
// build, where the image is already on the daemon anyway.
func stageableImages(images []string) []string {
	seen := map[string]struct{}{}
	for _, image := range images {
		image = strings.TrimSpace(image)
		if image == "" || strings.HasSuffix(image, ":local") {
			continue
		}
		seen[image] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for image := range seen {
		out = append(out, image)
	}
	// Stable order, so what a status line says does not depend on map order.
	sort.Strings(out)
	return out
}

// The image a sandbox with no harness config runs, as this server resolved it.
//
// Package-level and guarded, rather than a field: the reconciler is built
// during app construction and the value is resolved from configuration a moment
// later, and there is exactly one answer per process.
var (
	defaultSandboxImageMu sync.RWMutex
	resolvedSandboxImage  string
)

func setDefaultSandboxImage(image string) {
	defaultSandboxImageMu.Lock()
	defer defaultSandboxImageMu.Unlock()
	resolvedSandboxImage = strings.TrimSpace(image)
}

func defaultSandboxImage() string {
	defaultSandboxImageMu.RLock()
	defer defaultSandboxImageMu.RUnlock()
	if resolvedSandboxImage != "" {
		return resolvedSandboxImage
	}
	return sandbox.DefaultSandboxImageName
}

// ScanDirty is the level-triggered backstop: a pool that is up but not staged
// is picked up here, so a lost mark costs a scan interval rather than the
// staging.
//
// Staged pools are deliberately not returned. They re-stage on the refresh
// interval through RequeueAt, and returning them every scan would re-inspect
// every image on every pool once a minute for nothing.
func (r *PoolImagesReconciler) ScanDirty(ctx context.Context) ([]string, error) {
	projects, err := r.store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for i := range projects {
		pools, err := r.store.ListPools(ctx, projects[i].ID)
		if err != nil {
			return nil, err
		}
		for j := range pools {
			pool := &pools[j]
			if pool.RevokedAt != nil || pool.DesiredState != model.DesiredStatePresent {
				continue
			}
			if pool.Ready && !pool.ImagesStaged {
				ids = append(ids, pool.ID)
			}
		}
	}
	return ids, nil
}
