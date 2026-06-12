package docker

import (
	"testing"

	"github.com/obot-platform/discobox/internal/sandbox/vm"
	"github.com/obot-platform/discobox/internal/workeragent"
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
	boot := d.containerBootConfig(vm.BootConfig{Env: map[string]string{
		workeragent.EnvTenantID: "tenant-1",
	}})
	if got := boot.Env[workeragent.EnvControlPlaneURL]; got != "http://host.docker.internal:9090" {
		t.Fatalf("control plane url = %q", got)
	}
	if got := boot.Env[workeragent.EnvTenantID]; got != "tenant-1" {
		t.Fatalf("tenant ID = %q", got)
	}
}

func TestContainerBootConfigPreservesConfiguredControlPlaneURL(t *testing.T) {
	d := NewDriverWithClient(nil, Config{})
	boot := d.containerBootConfig(vm.BootConfig{Env: map[string]string{
		workeragent.EnvControlPlaneURL: "http://control.example",
	}})
	if got := boot.Env[workeragent.EnvControlPlaneURL]; got != "http://control.example" {
		t.Fatalf("control plane url = %q", got)
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
