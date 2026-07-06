package vm_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
	"github.com/obot-platform/discobox/server/providers/workerpool/vm"
	workeragent "github.com/obot-platform/discobox/worker-agent"
)

func TestBuildBootConfigIncludesProjectAndWorkerRegistration(t *testing.T) {
	boot := vm.BuildBootConfig(vm.BootInput{
		Ref: sandbox.SandboxRef{
			ProjectID: "project-1",
			SandboxID: "sandbox-1",
		},
		WorkerBootstrap: workeragent.Bootstrap{
			WorkerID: "worker-1",
			Token:    "secret-token",
		},
		ControlPlaneURL: "https://control.example",
		AgentPort:       3002,
	})

	if boot.Env[workeragent.EnvProjectID] != "project-1" {
		t.Fatalf("project env = %q", boot.Env[workeragent.EnvProjectID])
	}
	if boot.Env[workeragent.EnvWorkerID] != "worker-1" {
		t.Fatalf("worker env = %q", boot.Env[workeragent.EnvWorkerID])
	}
	if !strings.Contains(boot.CloudInitUserData, workeragent.EnvBootstrapToken) {
		t.Fatalf("cloud-init userdata does not contain bootstrap token env")
	}
	joined := strings.Join(boot.KernelCommandLine, " ")
	if !strings.Contains(joined, "discobox.discobox_project_id=project-1") {
		t.Fatalf("kernel args = %q", joined)
	}
}

func TestProviderCreatesVMWithBootConfigAndState(t *testing.T) {
	ctx := context.Background()
	driver := &recordingDriver{}
	provider, err := vm.New(vm.Config{
		Driver:          driver,
		DefaultImage:    "image-default",
		ControlPlaneURL: "https://control.example",
		Bootstrap: vm.BootstrapProviderFunc(func(context.Context, sandbox.SandboxRef, sandbox.CreateOptions) (vm.WorkerBootstrap, error) {
			return vm.WorkerBootstrap{WorkerID: "worker-1", Token: "token-1"}, nil
		}),
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}

	ref := sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"}
	runtimeSandbox, state, err := provider.Create(ctx, ref, nil, sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if runtimeSandbox.ID != "vm-1" {
		t.Fatalf("runtime sandbox ID = %q", runtimeSandbox.ID)
	}
	if len(state) == 0 {
		t.Fatalf("expected provider state")
	}
	if driver.createdSpec.Boot.Env[workeragent.EnvProjectID] != "project-1" {
		t.Fatalf("created project env = %q", driver.createdSpec.Boot.Env[workeragent.EnvProjectID])
	}
	if driver.createdSpec.Image != "image-default" {
		t.Fatalf("created image = %q", driver.createdSpec.Image)
	}

	got, err := provider.Get(ctx, ref, state)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != sandbox.StatusRunning {
		t.Fatalf("status = %q", got.Status)
	}
}

func TestProviderCreateWorkerRecreatesRuntimeWhenDesiredMetadataChanges(t *testing.T) {
	ctx := context.Background()
	driver := &recordingDriver{}
	provider, err := vm.New(vm.Config{
		Driver:       driver,
		DefaultImage: "image-default",
		Metadata:     map[string]string{"config-revision": "new"},
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	worker := &model.Worker{
		ID:           "worker-1",
		RuntimeState: []byte(`{"instanceId":"vm-1"}`),
	}

	err = provider.CreateWorker(ctx, &model.Project{ID: "project-1"}, &model.SandboxProviderInstance{ID: "provider-1"}, worker, "token-1", "control-plane-public-key")
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}
	if driver.createCalls != 1 {
		t.Fatalf("CreateVM calls = %d, want 1", driver.createCalls)
	}
	if driver.createdSpec.Metadata["config-revision"] != "new" {
		t.Fatalf("created metadata = %#v, missing config revision", driver.createdSpec.Metadata)
	}
	if string(worker.RuntimeState) != `{"instanceId":"vm-1"}` {
		t.Fatalf("worker runtime state = %s", worker.RuntimeState)
	}
}

func TestProviderRemoveWorkerDeletesCurrentWorkerVMWhenStateIsStale(t *testing.T) {
	ctx := context.Background()
	driver := &recordingDriver{
		missingDeleteIDs: map[string]bool{"stale-vm": true},
		workerInstance:   &vm.Instance{ID: "current-vm", Status: sandbox.StatusRunning},
	}
	provider, err := vm.New(vm.Config{Driver: driver})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	worker := &model.Worker{
		ID:           "worker-1",
		RuntimeState: []byte(`{"instanceId":"stale-vm"}`),
	}

	if err := provider.RemoveWorker(ctx, &model.Project{ID: "project-1"}, &model.SandboxProviderInstance{ID: "provider-1"}, worker); err != nil {
		t.Fatalf("remove worker: %v", err)
	}

	if len(driver.deletedVMIDs) != 2 || driver.deletedVMIDs[0] != "stale-vm" || driver.deletedVMIDs[1] != "current-vm" {
		t.Fatalf("deleted VM IDs = %#v, want stale-vm then current-vm", driver.deletedVMIDs)
	}
	if len(worker.RuntimeState) != 0 || worker.Ready || worker.Schedulable || worker.Degraded {
		t.Fatalf("worker after remove = %#v", worker)
	}
}

func TestProviderRepairWorkerRecreatesRuntimeAndPreservesNamedWorkerVolumes(t *testing.T) {
	driver := &recordingDriver{workerInstance: &vm.Instance{ID: "vm-old", Status: sandbox.StatusRunning}}
	provider, err := vm.New(vm.Config{
		Driver:       driver,
		DefaultImage: "image-default",
		Metadata:     map[string]string{"config-revision": "new"},
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	worker := &model.Worker{
		ID:           "worker-1",
		RuntimeState: []byte(`{"instanceId":"vm-old"}`),
		Ready:        true,
		Schedulable:  true,
		Degraded:     true,
	}

	if err := provider.RepairWorker(context.Background(), &model.Project{ID: "project-1"}, &model.SandboxProviderInstance{ID: "provider-1"}, worker, "token-1", "control-plane-public-key", "runtime unhealthy"); err != nil {
		t.Fatalf("repair worker: %v", err)
	}
	if len(driver.repairWorkerIDs) != 1 || driver.repairWorkerIDs[0] != "worker-1" {
		t.Fatalf("repair worker IDs = %#v, want worker-1", driver.repairWorkerIDs)
	}
	if len(driver.deletedVMIDs) != 1 || driver.deletedVMIDs[0] != "vm-old" {
		t.Fatalf("deleted VMs = %#v, want vm-old", driver.deletedVMIDs)
	}
	if len(driver.deleteRemoveVolumes) != 1 || !driver.deleteRemoveVolumes[0] {
		t.Fatalf("delete remove volumes = %#v, want true", driver.deleteRemoveVolumes)
	}
	if driver.repairSpec.Image != "image-default" {
		t.Fatalf("repair image = %q, want image-default", driver.repairSpec.Image)
	}
	if driver.repairSpec.Metadata["config-revision"] != "new" {
		t.Fatalf("repair metadata = %#v, missing config revision", driver.repairSpec.Metadata)
	}
	if driver.repairSpec.Boot.Env[workeragent.EnvWorkerID] != "worker-1" {
		t.Fatalf("repair worker env = %#v, want worker-1", driver.repairSpec.Boot.Env)
	}
	if driver.createCalls != 1 {
		t.Fatalf("CreateVM calls = %d, want 1", driver.createCalls)
	}
	if worker.RuntimeState == nil {
		t.Fatal("expected repaired runtime state")
	}
	if worker.Ready || worker.Schedulable || worker.Degraded || worker.Phase != model.WorkerPhaseRegistering {
		t.Fatalf("worker after repair = %#v", worker)
	}
}

type recordingDriver struct {
	createdSpec         vm.InstanceSpec
	createCalls         int
	deletedVMIDs        []string
	deleteRemoveVolumes []bool
	repairWorkerIDs     []string
	repairSpec          vm.InstanceSpec
	missingDeleteIDs    map[string]bool
	workerInstance      *vm.Instance
}

func (d *recordingDriver) CreateVM(_ context.Context, spec vm.InstanceSpec) (*vm.Instance, error) {
	d.createCalls++
	d.createdSpec = spec
	now := time.Now().UTC()
	return &vm.Instance{ID: "vm-1", Name: spec.Name, Image: spec.Image, Status: sandbox.StatusCreated, CreatedAt: now}, nil
}

func (d *recordingDriver) InitializeWorkerProvider(context.Context, *model.SandboxProviderInstance, any) error {
	return nil
}

func (d *recordingDriver) Close() error {
	return nil
}

func (d *recordingDriver) StartVM(context.Context, string) (*vm.Instance, error) {
	now := time.Now().UTC()
	return &vm.Instance{ID: "vm-1", Status: sandbox.StatusRunning, CreatedAt: now}, nil
}

func (d *recordingDriver) StopVM(context.Context, string, time.Duration) (*vm.Instance, error) {
	now := time.Now().UTC()
	return &vm.Instance{ID: "vm-1", Status: sandbox.StatusStopped, CreatedAt: now}, nil
}

func (d *recordingDriver) DeleteVM(_ context.Context, id string, removeVolumes bool) error {
	d.deletedVMIDs = append(d.deletedVMIDs, id)
	d.deleteRemoveVolumes = append(d.deleteRemoveVolumes, removeVolumes)
	if d.missingDeleteIDs[id] {
		return sandbox.ErrNotFound
	}
	return nil
}

func (d *recordingDriver) InspectVM(context.Context, string) (*vm.Instance, error) {
	now := time.Now().UTC()
	return &vm.Instance{ID: "vm-1", Status: sandbox.StatusRunning, CreatedAt: now}, nil
}

func (d *recordingDriver) InspectWorkerVM(context.Context, string) (*vm.Instance, error) {
	if d.workerInstance == nil {
		return nil, sandbox.ErrNotFound
	}
	return d.workerInstance, nil
}

func (d *recordingDriver) RemoveWorkerVM(ctx context.Context, workerID string, currentInstanceID string, removeVolumes bool) error {
	instanceID := currentInstanceID
	if instanceID == "" {
		inst, err := d.InspectWorkerVM(ctx, workerID)
		if errors.Is(err, sandbox.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		instanceID = inst.ID
	}
	if instanceID == "" {
		return nil
	}
	return d.DeleteVM(ctx, instanceID, removeVolumes)
}

func (d *recordingDriver) RepairWorkerVM(ctx context.Context, workerID string, currentInstanceID string, spec vm.InstanceSpec, _ string) (*vm.Instance, error) {
	d.repairWorkerIDs = append(d.repairWorkerIDs, workerID)
	d.repairSpec = spec
	instanceID := currentInstanceID
	if instanceID == "" {
		inst, err := d.InspectWorkerVM(ctx, workerID)
		if errors.Is(err, sandbox.ErrNotFound) {
			return d.CreateVM(ctx, spec)
		}
		if err != nil {
			return nil, err
		}
		instanceID = inst.ID
	}
	if err := d.DeleteVM(ctx, instanceID, true); err != nil {
		return nil, err
	}
	return d.CreateVM(ctx, spec)
}

func (d *recordingDriver) AcquireHTTPClient(context.Context, *vm.Instance) (*transport.HTTPClientLease, error) {
	return nil, errors.New("AcquireHTTPClient should not be called")
}

func (d *recordingDriver) AcquireWorkerHTTPClient(context.Context, string) (*transport.HTTPClientLease, error) {
	return nil, errors.New("AcquireWorkerHTTPClient should not be called")
}
