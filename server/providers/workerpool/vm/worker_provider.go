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

type workerVMInspector interface {
	InspectWorkerVM(ctx context.Context, workerID string) (*Instance, error)
}

func (p *Provider) CreateWorker(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker, token string, controlPlanePublicKey string) error {
	if p == nil {
		return errors.New("vm provider is required")
	}
	plan := p.workerRuntimePlan(project, provider, worker, token, controlPlanePublicKey)

	state := worker.RuntimeState
	if len(state) > 0 {
		runtimeWorker, err := p.Get(ctx, plan.ref, state)
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
	if len(state) > 0 {
		return nil
	}
	inst, err := p.driver.CreateVM(ctx, plan.spec)
	if errors.Is(err, sandbox.ErrAlreadyExists) {
		return nil
	}
	if err != nil {
		return err
	}
	state, err = encodeState(stateData{InstanceID: inst.ID, Worker: plan.bootstrap})
	if err != nil {
		return err
	}
	worker.RuntimeState, err = safeWorkerRuntimeState(state)
	return err
}

func (p *Provider) RepairWorker(ctx context.Context, project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker, token string, controlPlanePublicKey string, reason string) error {
	if p == nil {
		return errors.New("vm provider is required")
	}
	plan := p.workerRuntimePlan(project, provider, worker, token, controlPlanePublicKey)
	currentInstanceID, err := workerRuntimeInstanceID(worker.RuntimeState)
	if err != nil {
		return err
	}
	inst, err := p.driver.RepairWorkerVM(ctx, worker.ID, currentInstanceID, plan.spec, reason)
	if err != nil {
		return err
	}
	if inst == nil {
		return errors.New("vm driver repair returned no instance")
	}
	state, err := encodeState(stateData{InstanceID: inst.ID, Worker: plan.bootstrap})
	if err != nil {
		return err
	}
	worker.RuntimeState, err = safeWorkerRuntimeState(state)
	if err != nil {
		return err
	}
	worker.Ready = false
	worker.Schedulable = false
	worker.Degraded = false
	worker.Phase = model.WorkerPhaseRegistering
	return nil
}

type workerRuntimePlan struct {
	ref       sandbox.SandboxRef
	bootstrap WorkerBootstrap
	spec      InstanceSpec
}

func (p *Provider) workerRuntimePlan(project *model.Project, provider *model.SandboxProviderInstance, worker *model.Worker, token string, controlPlanePublicKey string) workerRuntimePlan {
	labels := map[string]string{
		"discobox.worker_id":            worker.ID,
		"discobox.worker_agent":         "true",
		"discobox.provider_instance_id": provider.ID,
	}
	for key, value := range p.metadata {
		labels[key] = value
	}
	ref := sandbox.SandboxRef{ProjectID: project.ID, SandboxID: "worker-" + worker.ID}
	bootstrap := workeragent.Bootstrap{
		ControlPlaneURL: p.controlPlaneURL,
		ProjectID:       project.ID,
		WorkerID:        worker.ID,
		Token:           token,
		ControlPlaneKey: controlPlanePublicKey,
		AgentPort:       p.agentPort,
	}
	boot := BuildBootConfig(BootInput{
		Ref:             ref,
		WorkerBootstrap: bootstrap,
		ControlPlaneURL: p.controlPlaneURL,
		AgentPort:       p.agentPort,
	})
	return workerRuntimePlan{
		ref:       ref,
		bootstrap: bootstrap,
		spec: InstanceSpec{
			Ref:      ref,
			Name:     instanceName(ref),
			Image:    p.defaultImage,
			Boot:     boot,
			Metadata: labels,
		},
	}
}

func (p *Provider) RemoveWorker(ctx context.Context, project *model.Project, _ *model.SandboxProviderInstance, worker *model.Worker) error {
	if p == nil {
		return errors.New("vm provider is required")
	}
	if err := p.removeWorkerRuntime(ctx, project, worker, true); err != nil {
		return err
	}
	worker.RuntimeState = nil
	worker.Ready = false
	worker.Schedulable = false
	worker.Degraded = false
	return nil
}

func (p *Provider) removeWorkerRuntime(ctx context.Context, project *model.Project, worker *model.Worker, removeVolumes bool) error {
	ref := sandbox.SandboxRef{ProjectID: project.ID, SandboxID: "worker-" + worker.ID}
	removed := false
	if len(worker.RuntimeState) > 0 {
		removeOptions := []sandbox.RemoveOption(nil)
		if removeVolumes {
			removeOptions = append(removeOptions, sandbox.RemoveVolumes())
		}
		if _, err := p.Remove(ctx, ref, worker.RuntimeState, removeOptions...); err != nil && !errors.Is(err, sandbox.ErrNotFound) {
			return err
		} else if err == nil {
			removed = true
		}
	}
	if !removed {
		inspector, ok := p.driver.(workerVMInspector)
		if ok {
			inst, err := inspector.InspectWorkerVM(ctx, worker.ID)
			if err != nil && !errors.Is(err, sandbox.ErrNotFound) {
				return err
			}
			if err == nil {
				if err := p.driver.DeleteVM(ctx, inst.ID, removeVolumes); err != nil && !errors.Is(err, sandbox.ErrNotFound) {
					return err
				}
			}
		}
	}
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

func workerRuntimeInstanceID(state []byte) (string, error) {
	if len(state) == 0 {
		return "", nil
	}
	data, err := decodeState(state)
	if errors.Is(err, sandbox.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return data.InstanceID, nil
}
