package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/dockerworker"
	"github.com/obot-platform/discobox/server/providers/workerpool"
)

// startWorkerWatcher starts the worker runtime drift watcher for the provider
// instance. The initial drift scan and the event watch both run in the
// background so provider initialization never blocks on, or fails because of,
// Docker connectivity. The watcher stops when the local driver closes.
func startWorkerWatcher(driver *LocalDriver, engine *dockerworker.Engine, manager workerpool.WorkerManager, provider *model.SandboxProviderInstance) error {
	if manager == nil {
		return fmt.Errorf("worker manager is required")
	}
	watcher := dockerWorkerWatcher{
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
	return nil
}

// run performs an initial best-effort drift scan and then watches for runtime
// events. Scan failures are logged, never fatal: reconcile jobs and runtime
// events cover anything the scan missed.
func (w dockerWorkerWatcher) run(ctx context.Context) {
	if _, err := w.scan(ctx); err != nil {
		log.Printf("docker worker watcher for provider %s initial scan failed: %v", w.providerID, err)
	}
	w.watch(ctx)
}

type dockerWorkerWatcher struct {
	client     *client.Client
	engine     *dockerworker.Engine
	manager    workerpool.WorkerManager
	projectID  string
	providerID string
}

func (w dockerWorkerWatcher) scan(ctx context.Context) (bool, error) {
	workers, err := w.manager.ListWorkers(ctx, w.projectID, w.providerID)
	if err != nil {
		return false, err
	}
	containers, err := w.listWorkerContainers(ctx)
	if err != nil {
		return false, err
	}
	containersByWorker := make(map[string]*container.InspectResponse, len(containers))
	for i := range containers {
		workerID := strings.TrimSpace(containers[i].Config.Labels[dockerworker.LabelWorkerID])
		if workerID == "" {
			continue
		}
		containersByWorker[workerID] = &containers[i]
	}
	workersByID := make(map[string]struct{}, len(workers))
	pendingWorkerReconcile := false
	for i := range workers {
		workersByID[workers[i].ID] = struct{}{}
		scheduled, err := w.checkWorker(ctx, &workers[i], containersByWorker[workers[i].ID])
		if err != nil {
			// One bad worker row must not block the scan (or provider init);
			// its own reconcile job surfaces the failure.
			log.Printf("docker worker watcher for provider %s could not check worker %s: %v", w.providerID, workers[i].ID, err)
			continue
		}
		pendingWorkerReconcile = pendingWorkerReconcile || scheduled
	}
	for i := range containers {
		workerID := strings.TrimSpace(containers[i].Config.Labels[dockerworker.LabelWorkerID])
		if workerID == "" {
			continue
		}
		if _, ok := workersByID[workerID]; ok {
			continue
		}
		// Orphan managed worker runtime with no worker row: no persisted
		// lifecycle is left to reconcile, delete it directly.
		if _, err := w.client.ContainerRemove(ctx, containers[i].ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true}); err != nil && !cerrdefs.IsNotFound(err) {
			return false, err
		}
	}
	return pendingWorkerReconcile, nil
}

func (w dockerWorkerWatcher) listWorkerContainers(ctx context.Context) ([]container.InspectResponse, error) {
	filters := make(client.Filters).
		Add("label", dockerworker.LabelManaged+"=true").
		Add("label", dockerworker.LabelWorkerAgent+"=true").
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

func (w dockerWorkerWatcher) checkWorker(ctx context.Context, worker *model.Worker, current *container.InspectResponse) (bool, error) {
	if worker == nil || worker.ProjectID != w.projectID || worker.ProviderInstanceID != w.providerID {
		return false, nil
	}
	if current != nil && worker.DesiredState == model.WorkerDesiredStateDeleted {
		return w.scheduleWorkerReconciliation(ctx, worker.ID)
	}
	if worker.DesiredState != model.WorkerDesiredStateActive || worker.RevokedAt != nil {
		return false, nil
	}
	if worker.LastOperationStatus == model.OperationStatusFailed || worker.Phase == model.WorkerPhaseFailed {
		if current != nil && containerRunning(current) {
			return w.scheduleWorkerReconciliation(ctx, worker.ID)
		}
		return false, nil
	}
	state, err := dockerworker.DecodeRuntimeState(worker.RuntimeState)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			if current != nil {
				return w.scheduleWorkerReconciliation(ctx, worker.ID)
			}
			return false, nil
		}
		return false, err
	}
	if strings.TrimSpace(state.ContainerID) == "" {
		// Runtime identity without a container (e.g. a partially-initialized
		// worker): treat it like a missing container and reconcile.
		return w.scheduleWorkerReconciliation(ctx, worker.ID)
	}
	inspect, err := w.client.ContainerInspect(ctx, state.ContainerID, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return w.scheduleWorkerReconciliation(ctx, worker.ID)
	}
	if err != nil {
		return false, err
	}
	if containerRunning(&inspect.Container) {
		if current != nil && current.ID != inspect.Container.ID {
			return w.scheduleWorkerReconciliation(ctx, worker.ID)
		}
		if w.engine.ShouldReconcileWorkerContainer(inspect.Container.Config.Image, inspect.Container.Config.Labels) {
			return w.scheduleWorkerReconciliation(ctx, worker.ID)
		}
		return false, nil
	}
	return w.scheduleWorkerReconciliation(ctx, worker.ID)
}

func containerRunning(inspect *container.InspectResponse) bool {
	return inspect != nil && inspect.State != nil && inspect.State.Running
}

func (w dockerWorkerWatcher) watch(ctx context.Context) {
	backoff := time.Second
	for {
		err := w.watchOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) {
			log.Printf("docker worker watcher for provider %s stopped: %v", w.providerID, err)
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

func (w dockerWorkerWatcher) watchOnce(ctx context.Context) error {
	filters := make(client.Filters).
		Add("type", string(events.ContainerEventType)).
		Add("label", dockerworker.LabelWorkerAgent+"=true").
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
			if !workerContainerLostAction(event.Action) {
				continue
			}
			if err := w.handleEvent(ctx, event); err != nil {
				log.Printf("docker worker watcher for provider %s could not record event: %v", w.providerID, err)
			}
		case err, ok := <-result.Err:
			if !ok {
				return nil
			}
			return err
		}
	}
}

func (w dockerWorkerWatcher) handleEvent(ctx context.Context, event events.Message) error {
	if event.Type != events.ContainerEventType || !workerContainerLostAction(event.Action) {
		return nil
	}
	workerID := strings.TrimSpace(event.Actor.Attributes[dockerworker.LabelWorkerID])
	if workerID == "" {
		return nil
	}
	_, err := w.scheduleWorkerReconciliation(ctx, workerID)
	return err
}

func (w dockerWorkerWatcher) scheduleWorkerReconciliation(ctx context.Context, workerID string) (bool, error) {
	return true, w.manager.ScheduleWorkerReconciliation(ctx, workerID)
}

func workerContainerLostAction(action events.Action) bool {
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
