package vm_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
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

type recordingDriver struct {
	createdSpec vm.InstanceSpec
	createCalls int
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

func (d *recordingDriver) DeleteVM(context.Context, string, bool) error { return nil }

func (d *recordingDriver) InspectVM(context.Context, string) (*vm.Instance, error) {
	now := time.Now().UTC()
	return &vm.Instance{ID: "vm-1", Status: sandbox.StatusRunning, CreatedAt: now}, nil
}
