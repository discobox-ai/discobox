package vm_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/obot-platform/discobox/providers/sandbox/vm"
	sandbox "github.com/obot-platform/discobox/sandboxprovider"
	workerbootstrap "github.com/obot-platform/discobox/workerbootstrap"
)

func TestBuildBootConfigIncludesTenantAndWorkerRegistration(t *testing.T) {
	boot := vm.BuildBootConfig(vm.BootInput{
		Ref: sandbox.SandboxRef{
			TenantID:  "tenant-1",
			ProjectID: "project-1",
			SandboxID: "sandbox-1",
		},
		WorkerBootstrap: workerbootstrap.Bootstrap{
			WorkerID: "worker-1",
			Token:    "secret-token",
		},
		ControlPlaneURL: "https://control.example",
		AgentPort:       3002,
	})

	if boot.Env[workerbootstrap.EnvTenantID] != "tenant-1" {
		t.Fatalf("tenant env = %q", boot.Env[workerbootstrap.EnvTenantID])
	}
	if boot.Env[workerbootstrap.EnvWorkerID] != "worker-1" {
		t.Fatalf("worker env = %q", boot.Env[workerbootstrap.EnvWorkerID])
	}
	if !strings.Contains(boot.CloudInitUserData, workerbootstrap.EnvBootstrapToken) {
		t.Fatalf("cloud-init userdata does not contain bootstrap token env")
	}
	joined := strings.Join(boot.KernelCommandLine, " ")
	if !strings.Contains(joined, "discobox.discobox_tenant_id=tenant-1") {
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

	ref := sandbox.SandboxRef{TenantID: "tenant-1", ProjectID: "project-1", SandboxID: "sandbox-1"}
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
	if driver.createdSpec.Boot.Env[workerbootstrap.EnvTenantID] != "tenant-1" {
		t.Fatalf("created tenant env = %q", driver.createdSpec.Boot.Env[workerbootstrap.EnvTenantID])
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

type recordingDriver struct {
	createdSpec vm.InstanceSpec
}

func (d *recordingDriver) CreateVM(_ context.Context, spec vm.InstanceSpec) (*vm.Instance, error) {
	d.createdSpec = spec
	now := time.Now().UTC()
	return &vm.Instance{ID: "vm-1", Name: spec.Name, Image: spec.Image, Status: sandbox.StatusCreated, CreatedAt: now}, nil
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
