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
	if err := watcher.scan(ctx); err != nil {
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

func (w dockerWorkerWatcher) scan(ctx context.Context) error {
	workers, err := w.manager.ListWorkers(ctx, w.projectID, w.providerID)
	if err != nil {
		return err
	}
	for i := range workers {
		if err := w.checkWorker(ctx, &workers[i]); err != nil {
			return err
		}
	}
	return nil
}

func (w dockerWorkerWatcher) checkWorker(ctx context.Context, worker *model.Worker) error {
	if worker == nil || worker.ProjectID != w.projectID || worker.ProviderInstanceID != w.providerID {
		return nil
	}
	if worker.DesiredState != model.WorkerDesiredStateActive || worker.RevokedAt != nil || worker.LastOperationStatus == model.OperationStatusFailed {
		return nil
	}
	state, err := decodeDockerWorkerRuntimeState(worker.RuntimeState)
	if err != nil {
		if errors.Is(err, sandbox.ErrNotFound) {
			return nil
		}
		return err
	}
	inst, err := w.driver.InspectVM(ctx, state.InstanceID)
	if errors.Is(err, sandbox.ErrNotFound) {
		return w.markRuntimeLost(ctx, worker.ID, fmt.Sprintf("worker container %s is missing", shortContainerID(state.InstanceID)))
	}
	if err != nil {
		return err
	}
	if inst.Status == sandbox.StatusRunning {
		return nil
	}
	return w.markRuntimeLost(ctx, worker.ID, runtimeLostMessage(inst.ID, inst.Status, inst.Error))
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
	message := fmt.Sprintf("worker container %s %s", shortContainerID(event.Actor.ID), event.Action)
	return w.markRuntimeLost(ctx, workerID, message)
}

func (w dockerWorkerWatcher) markRuntimeLost(ctx context.Context, workerID string, message string) error {
	updated, err := w.manager.MarkWorkerRuntimeLost(ctx, w.projectID, w.providerID, workerID, message)
	if err != nil || !updated {
		return err
	}
	return w.manager.ScheduleWorkerProviderReconciliation(ctx, w.projectID, w.providerID)
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

func runtimeLostMessage(id string, status sandbox.Status, detail string) string {
	message := fmt.Sprintf("worker container %s is %s", shortContainerID(id), status)
	if strings.TrimSpace(detail) != "" {
		message += ": " + strings.TrimSpace(detail)
	}
	return message
}
