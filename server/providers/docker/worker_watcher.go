package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/workerpool"
	"github.com/obot-platform/discobox/server/providers/workerpool/vm"
)

func (d *Driver) InitializeWorkerProvider(ctx context.Context, provider *model.SandboxProviderInstance, manager any) error {
	if d == nil || provider == nil || manager == nil {
		return nil
	}
	workerManager, ok := manager.(workerpool.WorkerManager)
	if !ok {
		return fmt.Errorf("worker manager is required")
	}

	d.watcherMu.Lock()
	if d.watcherCancel != nil {
		d.watcherMu.Unlock()
		return nil
	}
	watchCtx, cancel := context.WithCancel(context.Background())
	d.watcherCancel = cancel
	d.watcherMu.Unlock()

	watcher := dockerWorkerWatcher{
		driver:     d,
		manager:    workerManager,
		projectID:  provider.ProjectID,
		providerID: provider.ID,
	}
	if _, err := watcher.scan(ctx); err != nil {
		cancel()
		d.watcherMu.Lock()
		d.watcherCancel = nil
		d.watcherMu.Unlock()
		return err
	}
	go watcher.watch(watchCtx)
	return nil
}

type dockerWorkerWatcher struct {
	driver     *Driver
	manager    workerpool.WorkerManager
	projectID  string
	providerID string
}

type dockerWorkerRuntimeState struct {
	InstanceID string `json:"instanceId"`
}

func (w dockerWorkerWatcher) scan(ctx context.Context) (bool, error) {
	workers, err := w.manager.ListWorkers(ctx, w.projectID, w.providerID)
	if err != nil {
		return false, err
	}
	containers, err := w.driver.ListWorkerVMs(ctx, w.providerID)
	if err != nil {
		return false, err
	}
	containersByWorker := make(map[string]*vm.Instance, len(containers))
	for i := range containers {
		workerID := strings.TrimSpace(containers[i].Metadata[labelWorkerID])
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
			return false, err
		}
		pendingWorkerReconcile = pendingWorkerReconcile || scheduled
	}
	for i := range containers {
		workerID := strings.TrimSpace(containers[i].Metadata[labelWorkerID])
		if workerID == "" {
			continue
		}
		if _, ok := workersByID[workerID]; ok {
			continue
		}
		if err := w.driver.DeleteVM(ctx, containers[i].ID, true); err != nil && !errors.Is(err, sandbox.ErrNotFound) {
			return false, err
		}
	}
	return pendingWorkerReconcile, nil
}

func (w dockerWorkerWatcher) checkWorker(ctx context.Context, worker *model.Worker, current *vm.Instance) (bool, error) {
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
		if current != nil && current.Status == sandbox.StatusRunning {
			return w.scheduleWorkerReconciliation(ctx, worker.ID)
		}
		return false, nil
	}
	state, err := decodeDockerWorkerRuntimeState(worker.RuntimeState)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			if current != nil {
				return w.scheduleWorkerReconciliation(ctx, worker.ID)
			}
			return false, nil
		}
		return false, err
	}
	inst, err := w.driver.InspectVM(ctx, state.InstanceID)
	if errors.Is(err, sandbox.ErrNotFound) {
		return w.scheduleWorkerReconciliation(ctx, worker.ID)
	}
	if err != nil {
		return false, err
	}
	if inst.Status == sandbox.StatusRunning {
		if current != nil && current.ID != inst.ID {
			return w.scheduleWorkerReconciliation(ctx, worker.ID)
		}
		return false, nil
	}
	return w.scheduleWorkerReconciliation(ctx, worker.ID)
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
		Add("label", labelWorkerAgent+"=true").
		Add("label", labelProviderInstanceID+"="+w.providerID)
	result := w.driver.client.Events(ctx, client.EventsListOptions{Filters: filters})
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
	workerID := strings.TrimSpace(event.Actor.Attributes[labelWorkerID])
	if workerID == "" {
		return nil
	}
	_, err := w.scheduleWorkerReconciliation(ctx, workerID)
	return err
}

func (w dockerWorkerWatcher) scheduleWorkerReconciliation(ctx context.Context, workerID string) (bool, error) {
	return true, w.manager.ScheduleWorkerReconciliation(ctx, workerID)
}

func decodeDockerWorkerRuntimeState(data []byte) (dockerWorkerRuntimeState, error) {
	if len(data) == 0 {
		return dockerWorkerRuntimeState{}, sandbox.ErrNotFound
	}
	var state dockerWorkerRuntimeState
	if err := json.Unmarshal(data, &state); err != nil {
		return dockerWorkerRuntimeState{}, err
	}
	if strings.TrimSpace(state.InstanceID) == "" {
		return dockerWorkerRuntimeState{}, sandbox.ErrNotFound
	}
	return state, nil
}

func workerContainerLostAction(action events.Action) bool {
	switch action {
	case events.ActionDie, events.ActionStop, events.ActionKill, events.ActionOOM, events.ActionDestroy, events.ActionRemove:
		return true
	default:
		return false
	}
}
