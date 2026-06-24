package vm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
	workeragent "github.com/obot-platform/discobox/worker-agent"
)

func (p *Provider) InitializeWorkerProvider(ctx context.Context, provider *model.SandboxProviderInstance, manager any) error {
	if p == nil {
		return errors.New("vm provider is required")
	}
	return p.driver.InitializeWorkerProvider(ctx, provider, manager)
}

func (p *Provider) CreateWorker(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker, token string, controlPlanePublicKey string) error {
	if p == nil {
		return errors.New("vm provider is required")
	}
	labels := map[string]string{"discobox.worker_id": worker.ID, "discobox.worker_agent": "true", "discobox.provider_instance_id": provider.ID}
	for key, value := range p.metadata {
		labels[key] = value
	}
	workerProvider := *p
	workerProvider.bootstrap = BootstrapProviderFunc(func(context.Context, sandbox.SandboxRef, sandbox.CreateOptions) (WorkerBootstrap, error) {
		return workeragent.Bootstrap{ControlPlaneURL: p.controlPlaneURL, ProjectID: project.ID, WorkerID: worker.ID, Token: token, ControlPlaneKey: controlPlanePublicKey, AgentPort: p.agentPort}, nil
	})
	workerProvider.metadata = labels

	ref := sandbox.SandboxRef{ProjectID: project.ID, SandboxID: "worker-" + worker.ID}
	state := worker.RuntimeState
	if len(state) > 0 {
		runtimeWorker, err := workerProvider.Get(ctx, ref, state)
		if errors.Is(err, sandbox.ErrNotFound) || shouldRecreateWorkerRuntime(runtimeWorker, p.defaultImage, p.metadata) {
			state = nil
			worker.RuntimeState = nil
			worker.Ready = false
			worker.Schedulable = false
			worker.Phase = model.WorkerPhaseRegistering
		} else if err != nil {
			return err
		}
	}
	_, state, err := workerProvider.Create(ctx, ref, state, sandbox.CreateOptions{Labels: labels})
	if errors.Is(err, sandbox.ErrAlreadyExists) {
		return nil
	}
	if err == nil {
		worker.RuntimeState, err = safeWorkerRuntimeState(state)
	}
	return err
}

func (p *Provider) RemoveWorker(ctx context.Context, project *model.Project, _ *model.SandboxProviderInstance, worker *model.Worker) error {
	if p == nil {
		return errors.New("vm provider is required")
	}
	ref := sandbox.SandboxRef{ProjectID: project.ID, SandboxID: "worker-" + worker.ID}
	if len(worker.RuntimeState) > 0 {
		if _, err := p.Remove(ctx, ref, worker.RuntimeState, sandbox.RemoveVolumes()); err != nil && !errors.Is(err, sandbox.ErrNotFound) {
			return err
		}
	}
	worker.RuntimeState = nil
	worker.Ready = false
	worker.Schedulable = false
	worker.Degraded = false
	return nil
}

func (p *Provider) AcquireWorkerHTTPClient(ctx context.Context, worker *model.Worker) (*transport.HTTPClientLease, error) {
	if p == nil {
		return nil, errors.New("vm provider is required")
	}
	if worker == nil || worker.ID == "" {
		return nil, errors.New("worker is required")
	}
	return p.AcquireWorkerHTTPClientForID(ctx, worker.ID)
}

func shouldRecreateWorkerRuntime(runtimeWorker *sandbox.Sandbox, desiredImage string, desiredMetadata map[string]string) bool {
	if runtimeWorker == nil {
		return true
	}
	if runtimeWorker.Status != sandbox.StatusRunning {
		return true
	}
	if strings.TrimSpace(desiredImage) != "" && runtimeWorker.Image != desiredImage {
		return true
	}
	for key, value := range desiredMetadata {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if runtimeWorker.Metadata[key] != value {
			return true
		}
	}
	return false
}

func safeWorkerRuntimeState(state []byte) ([]byte, error) {
	if len(state) == 0 {
		return nil, nil
	}
	var data struct {
		InstanceID string `json:"instanceId"`
	}
	if err := json.Unmarshal(state, &data); err != nil {
		return nil, err
	}
	if data.InstanceID == "" {
		return nil, sandbox.ErrNotFound
	}
	return json.Marshal(data)
}
