package docker

import (
	"testing"

	"github.com/moby/moby/api/types/mount"

	"github.com/obot-platform/discobox/providers/sandbox/vm"
	sandbox "github.com/obot-platform/discobox/sandboxprovider"
	workerbootstrap "github.com/obot-platform/discobox/workerbootstrap"
)

func TestDefinitionIncludesSystemdConfig(t *testing.T) {
	def := Definition()
	if def.Name != "Docker" {
		t.Fatalf("name = %q", def.Name)
	}
	keys := map[string]bool{}
	var controlPlaneRequired, controlPlaneAdvanced bool
	for _, field := range def.ConfigFields {
		keys[field.Key] = true
		if field.Key == "controlPlaneUrl" {
			controlPlaneRequired = field.Required
			controlPlaneAdvanced = field.Advanced
		}
	}
	for _, key := range []string{"controlPlaneUrl", "image", "systemd", "privileged", "poolSize"} {
		if !keys[key] {
			t.Fatalf("definition missing config key %q", key)
		}
	}
	if controlPlaneRequired {
		t.Fatalf("controlPlaneUrl is required, want optional")
	}
	if !controlPlaneAdvanced {
		t.Fatalf("controlPlaneUrl advanced = false, want true")
	}
}

func TestNewDriverWithClientDefaultsToSystemdContainer(t *testing.T) {
	d := NewDriverWithClient(nil, Config{})
	if d.image != defaultImage {
		t.Fatalf("image = %q", d.image)
	}
	if d.agentPort != defaultAgentPort {
		t.Fatalf("agentPort = %d", d.agentPort)
	}
	if d.systemd {
		t.Fatalf("systemd default = true, want false for raw driver config")
	}
	if d.privileged {
		t.Fatalf("privileged default = true, want false when systemd is false")
	}
}

func TestNewDriverWithClientSystemdDefaults(t *testing.T) {
	d := NewDriverWithClient(nil, Config{Systemd: true})
	if !d.systemd {
		t.Fatalf("systemd = false")
	}
	if !d.privileged {
		t.Fatalf("privileged = false, want true for systemd")
	}
	if len(d.command) != 1 || d.command[0] != "/usr/local/bin/discobox-worker-agent" {
		t.Fatalf("command = %#v", d.command)
	}
}

func TestNewDriverWithClientHonorsPrivilegedOverride(t *testing.T) {
	privileged := false
	d := NewDriverWithClient(nil, Config{Systemd: true, Privileged: &privileged})
	if d.privileged {
		t.Fatalf("privileged override ignored")
	}
}

func TestContainerBootConfigDerivesControlPlaneURL(t *testing.T) {
	t.Setenv("PORT", "9090")
	d := NewDriverWithClient(nil, Config{})
	boot := d.containerBootConfig(vm.BootConfig{})
	if got := boot.Env[workerbootstrap.EnvControlPlaneURL]; got != "http://host.docker.internal:9090" {
		t.Fatalf("control plane url = %q", got)
	}
}

func TestContainerBootConfigPreservesConfiguredControlPlaneURL(t *testing.T) {
	d := NewDriverWithClient(nil, Config{})
	boot := d.containerBootConfig(vm.BootConfig{Env: map[string]string{
		workerbootstrap.EnvControlPlaneURL: "http://control.example",
	}})
	if got := boot.Env[workerbootstrap.EnvControlPlaneURL]; got != "http://control.example" {
		t.Fatalf("control plane url = %q", got)
	}
}

func TestDefaultControlPlaneURLAddsHostGateway(t *testing.T) {
	t.Setenv("PORT", "9090")
	d := NewDriverWithClient(nil, Config{})
	original := vm.BootConfig{}
	boot := d.containerBootConfig(original)

	if !controlPlaneURLDefaulted(original) {
		t.Fatalf("control plane URL defaulted = false, want true")
	}
	if !controlPlaneURLUsesHostGateway(boot.Env[workerbootstrap.EnvControlPlaneURL]) {
		t.Fatalf("control plane URL = %q, want host-gateway URL", boot.Env[workerbootstrap.EnvControlPlaneURL])
	}
	extraHosts := controlPlaneExtraHosts(controlPlaneURLDefaulted(original), boot.Env[workerbootstrap.EnvControlPlaneURL])
	if len(extraHosts) != 1 || extraHosts[0] != "host.docker.internal:host-gateway" {
		t.Fatalf("extra hosts = %#v, want host gateway mapping", extraHosts)
	}
}

func TestConfiguredControlPlaneURLDoesNotCountAsDefaulted(t *testing.T) {
	boot := vm.BootConfig{Env: map[string]string{
		workerbootstrap.EnvControlPlaneURL: "http://host.docker.internal:8080",
	}}

	if controlPlaneURLDefaulted(boot) {
		t.Fatalf("control plane URL defaulted = true, want false for configured URL")
	}
	if extraHosts := controlPlaneExtraHosts(controlPlaneURLDefaulted(boot), boot.Env[workerbootstrap.EnvControlPlaneURL]); len(extraHosts) != 0 {
		t.Fatalf("extra hosts = %#v, want none for configured URL", extraHosts)
	}
}

func TestContainerLabelsIncludeSandboxIDByDefault(t *testing.T) {
	d := NewDriverWithClient(nil, Config{})
	labels := d.containerLabels(vm.InstanceSpec{
		Ref:  sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"},
		Name: "instance-1",
		Boot: vm.BootConfig{Env: map[string]string{workerbootstrap.EnvWorkerID: "worker-1"}},
	})

	if labels[labelSandboxID] != "sandbox-1" {
		t.Fatalf("sandbox label = %q, want sandbox-1", labels[labelSandboxID])
	}
	if labels[labelWorkerID] != "worker-1" {
		t.Fatalf("worker label = %q, want worker-1", labels[labelWorkerID])
	}
}

func TestContainerLabelsOmitSandboxIDForWorkerAgent(t *testing.T) {
	d := NewDriverWithClient(nil, Config{})
	labels := d.containerLabels(vm.InstanceSpec{
		Ref:      sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "worker-worker-1"},
		Name:     "instance-1",
		Boot:     vm.BootConfig{Env: map[string]string{workerbootstrap.EnvWorkerID: "worker-1"}},
		Metadata: map[string]string{labelWorkerAgent: "true"},
	})

	if labels[labelWorkerAgent] != "true" {
		t.Fatalf("worker agent label = %q, want true", labels[labelWorkerAgent])
	}
	if _, ok := labels[labelSandboxID]; ok {
		t.Fatalf("sandbox label present for worker agent: %#v", labels)
	}
	if labels[labelWorkerID] != "worker-1" {
		t.Fatalf("worker label = %q, want worker-1", labels[labelWorkerID])
	}
}

func TestWorkerAgentCleanupLabelsAreScopedToProjectAndWorker(t *testing.T) {
	got := workerAgentCleanupLabels("project-1", "worker-1")
	want := []string{
		"discobox.worker_agent=true",
		"discobox.project_id=project-1",
		"discobox.worker_id=worker-1",
	}
	if len(got) != len(want) {
		t.Fatalf("cleanup labels = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cleanup labels = %#v, want %#v", got, want)
		}
	}
}

func TestContainerMountsBindHostDockerSocketForWorkerAgent(t *testing.T) {
	d := NewDriverWithClient(nil, Config{})
	mounts := d.containerMounts(true, "worker-1")

	if !hasMount(mounts, dockerSocketPath, dockerSocketPath) {
		t.Fatalf("mounts = %#v, missing host Docker socket bind mount", mounts)
	}
}

func TestContainerMountsDoNotBindHostDockerSocketForNonWorkerAgent(t *testing.T) {
	d := NewDriverWithClient(nil, Config{})
	mounts := d.containerMounts(false, "worker-1")

	if hasMount(mounts, dockerSocketPath, dockerSocketPath) {
		t.Fatalf("mounts = %#v, host Docker socket should only be mounted for worker agents", mounts)
	}
}

func TestContainerNameUsesWorkerID(t *testing.T) {
	if got := containerName("worker:one", "sandbox:one"); got != "discobox-vm-worker-one" {
		t.Fatalf("container name = %q", got)
	}
	if got := containerName("", "sandbox:one"); got != "discobox-vm-sandbox-one" {
		t.Fatalf("fallback container name = %q", got)
	}
}

func TestScopedVolumeNameUsesWorkerID(t *testing.T) {
	if got := scopedVolumeName("worker:one", "docker"); got != "discobox-worker-worker-one-docker" {
		t.Fatalf("volume name = %q", got)
	}
	if got := scopedVolumeName("", "discobox"); got != "discobox-worker-unknown-discobox" {
		t.Fatalf("fallback volume name = %q", got)
	}
}

func hasMount(mounts []mount.Mount, source, target string) bool {
	for _, m := range mounts {
		if m.Type == mount.TypeBind && m.Source == source && m.Target == target {
			return true
		}
	}
	return false
}
