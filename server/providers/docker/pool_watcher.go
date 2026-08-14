package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/dockerworker"
	"github.com/obot-platform/discobox/server/providers/poolruntime"
)

// startBackgroundWatchers starts the provider instance's background loops over
// the local Docker daemon: the pool runtime drift watcher, and the image reaper
// that reclaims Discobox images the daemon no longer needs.
//
// Everything runs in the background so provider initialization never blocks on,
// or fails because of, Docker connectivity, and everything shares one cancel so
// closing the local driver stops all of it.
func startBackgroundWatchers(driver *LocalDriver, engine *dockerworker.Engine, manager poolruntime.PoolManager, provider *model.SandboxProviderInstance) error {
	if manager == nil {
		return fmt.Errorf("pool manager is required")
	}
	watcher := dockerPoolWatcher{
		client:     driver.client,
		engine:     engine,
		manager:    manager,
		projectID:  provider.ProjectID,
		providerID: provider.ID,
	}
	watchCtx, cancel := context.WithCancel(context.Background())
	driver.watcherMu.Lock()
	if driver.watcherCancel != nil {
		driver.watcherMu.Unlock()
		cancel()
		return nil
	}
	driver.watcherCancel = cancel
	driver.watcherMu.Unlock()
	go watcher.run(watchCtx)
	go reclaimImages(watchCtx, driver.client, engine, provider.ID)
	return nil
}

// reclaimImages reclaims unused Discobox images from the daemon this provider
// instance hosts its pools on, on a slow interval, until the driver closes
// (ADR 0040).
//
// This is the daemon `task dev` rebuilds onto and the one an upgrade pulls onto,
// so it accumulates a superseded image per build and per upgrade with nothing
// else to clean it up. A failed pass is logged and retried on the next tick: the
// daemon being briefly unreachable is not a reason to stop reclaiming.
func reclaimImages(ctx context.Context, cli *client.Client, engine *dockerworker.Engine, providerID string) {
	ticker := time.NewTicker(engine.ImageReclaimInterval())
	defer ticker.Stop()
	for {
		if err := engine.ReclaimImages(ctx, cli, slog.Default()); err != nil && ctx.Err() == nil {
			log.Printf("docker image reaper for provider %s failed: %v", providerID, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// run performs an initial best-effort drift scan and then watches for runtime
// events. Scan failures are logged, never fatal: reconcile marks and runtime
// events cover anything the scan missed.
func (w dockerPoolWatcher) run(ctx context.Context) {
	if _, err := w.scan(ctx); err != nil {
		log.Printf("docker pool watcher for provider %s initial scan failed: %v", w.providerID, err)
	}
	w.watch(ctx)
}

type dockerPoolWatcher struct {
	client     *client.Client
	engine     *dockerworker.Engine
	manager    poolruntime.PoolManager
	projectID  string
	providerID string
}

func (w dockerPoolWatcher) scan(ctx context.Context) (bool, error) {
	pools, err := w.manager.ListPoolsForProviderInstance(ctx, w.projectID, w.providerID)
	if err != nil {
		return false, err
	}
	containers, err := w.listPoolContainers(ctx)
	if err != nil {
		return false, err
	}
	containersByPool := make(map[string]*container.InspectResponse, len(containers))
	for i := range containers {
		poolID := strings.TrimSpace(containers[i].Config.Labels[dockerworker.LabelPoolID])
		if poolID == "" {
			continue
		}
		containersByPool[poolID] = &containers[i]
	}
	poolsByID := make(map[string]struct{}, len(pools))
	pendingPoolReconcile := false
	for i := range pools {
		poolsByID[pools[i].ID] = struct{}{}
		scheduled, err := w.checkPool(ctx, &pools[i], containersByPool[pools[i].ID])
		if err != nil {
			// One bad pool row must not block the scan (or provider init);
			// its own reconcile surfaces the failure.
			log.Printf("docker pool watcher for provider %s could not check pool %s: %v", w.providerID, pools[i].ID, err)
			continue
		}
		pendingPoolReconcile = pendingPoolReconcile || scheduled
	}
	for i := range containers {
		poolID := strings.TrimSpace(containers[i].Config.Labels[dockerworker.LabelPoolID])
		if poolID == "" {
			continue
		}
		if _, ok := poolsByID[poolID]; ok {
			continue
		}
		// Orphan managed pool runtime with no pool row: no persisted lifecycle
		// is left to reconcile, delete it directly.
		if _, err := w.client.ContainerRemove(ctx, containers[i].ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !cerrdefs.IsNotFound(err) {
			return false, err
		}
	}
	return pendingPoolReconcile, nil
}

func (w dockerPoolWatcher) listPoolContainers(ctx context.Context) ([]container.InspectResponse, error) {
	filters := make(client.Filters).
		Add("label", dockerworker.LabelManaged+"=true").
		Add("label", dockerworker.LabelPoolAgent+"=true").
		Add("label", dockerworker.LabelProviderInstanceID+"="+w.providerID)
	summaries, err := w.client.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, err
	}
	containers := make([]container.InspectResponse, 0, len(summaries.Items))
	for _, summary := range summaries.Items {
		inspect, err := w.client.ContainerInspect(ctx, summary.ID, client.ContainerInspectOptions{})
		if err != nil {
			if cerrdefs.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		containers = append(containers, inspect.Container)
	}
	return containers, nil
}

func (w dockerPoolWatcher) checkPool(ctx context.Context, pool *model.Pool, current *container.InspectResponse) (bool, error) {
	if pool == nil || pool.ProjectID != w.projectID {
		return false, nil
	}
	if current != nil && pool.DesiredState == model.DesiredStateDeleted {
		return w.schedulePoolReconciliation(ctx, pool)
	}
	if pool.DesiredState != model.DesiredStatePresent || pool.RevokedAt != nil {
		return false, nil
	}
	// Offline counts: it is a pool whose host stopped answering and is expected
	// back, which is exactly what this watcher exists to recover (ADR 0017 §4).
	if pool.ErrorMessage != nil || pool.State == model.PoolStateFailed || pool.State == model.PoolStateOffline {
		// A created pool is stateful, so recover it whether or not a container
		// is still present: its runtime may need to be recreated. A pool that
		// never registered only reconciles while a container lingers to clean up.
		if pool.EverCreated() || (current != nil && containerRunning(current)) {
			return w.schedulePoolReconciliation(ctx, pool)
		}
		return false, nil
	}
	state, err := dockerworker.DecodeRuntimeState(pool.RuntimeState)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			if current != nil {
				return w.schedulePoolReconciliation(ctx, pool)
			}
			return false, nil
		}
		return false, err
	}
	if strings.TrimSpace(state.ContainerID) == "" {
		// Runtime identity without a container (e.g. a partially-initialized
		// pool): treat it like a missing container and reconcile.
		return w.schedulePoolReconciliation(ctx, pool)
	}
	inspect, err := w.client.ContainerInspect(ctx, state.ContainerID, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return w.schedulePoolReconciliation(ctx, pool)
	}
	if err != nil {
		return false, err
	}
	if containerRunning(&inspect.Container) {
		if current != nil && current.ID != inspect.Container.ID {
			return w.schedulePoolReconciliation(ctx, pool)
		}
		if w.engine.ShouldReconcileWorkerContainer(inspect.Container.Config.Image, inspect.Container.Config.Labels) {
			return w.schedulePoolReconciliation(ctx, pool)
		}
		return false, nil
	}
	return w.schedulePoolReconciliation(ctx, pool)
}

func containerRunning(inspect *container.InspectResponse) bool {
	return inspect != nil && inspect.State != nil && inspect.State.Running
}

func (w dockerPoolWatcher) watch(ctx context.Context) {
	backoff := time.Second
	for {
		err := w.watchOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
			log.Printf("docker pool watcher for provider %s stopped: %v", w.providerID, err)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (w dockerPoolWatcher) watchOnce(ctx context.Context) error {
	filters := make(client.Filters).
		Add("type", string(events.ContainerEventType)).
		Add("label", dockerworker.LabelPoolAgent+"=true").
		Add("label", dockerworker.LabelProviderInstanceID+"="+w.providerID)
	result := w.client.Events(ctx, client.EventsListOptions{Filters: filters})
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-result.Messages:
			if !ok {
				return nil
			}
			if !poolContainerLostAction(event.Action) {
				continue
			}
			if err := w.handleEvent(ctx, event); err != nil {
				log.Printf("docker pool watcher for provider %s could not record event: %v", w.providerID, err)
			}
		case err, ok := <-result.Err:
			if !ok {
				return nil
			}
			return err
		}
	}
}

func (w dockerPoolWatcher) handleEvent(ctx context.Context, event events.Message) error {
	if event.Type != events.ContainerEventType || !poolContainerLostAction(event.Action) {
		return nil
	}
	poolID := strings.TrimSpace(event.Actor.Attributes[dockerworker.LabelPoolID])
	if poolID == "" {
		return nil
	}
	_, err := w.schedulePoolReconciliationByID(ctx, poolID)
	return err
}

func (w dockerPoolWatcher) schedulePoolReconciliation(ctx context.Context, pool *model.Pool) (bool, error) {
	return true, w.manager.SchedulePoolReconciliation(ctx, pool.ProjectID, pool.ID)
}

func (w dockerPoolWatcher) schedulePoolReconciliationByID(ctx context.Context, poolID string) (bool, error) {
	return true, w.manager.SchedulePoolReconciliation(ctx, w.projectID, poolID)
}

func poolContainerLostAction(action events.Action) bool {
	switch action {
	case events.ActionDie, events.ActionStop, events.ActionKill, events.ActionOOM, events.ActionDestroy, events.ActionRemove:
		return true
	default:
		return false
	}
}

func mapDockerNotFound(err error) error {
	if err == nil {
		return nil
	}
	if cerrdefs.IsNotFound(err) {
		return sandbox.ErrNotFound
	}
	return err
}
