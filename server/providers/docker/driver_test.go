package docker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/providers/workerpool/vm"
	workeragent "github.com/obot-platform/discobox/worker-agent"
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
	d := NewDriverWithClient(nil, DriverConfig{})
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
	d := NewDriverWithClient(nil, DriverConfig{Systemd: true})
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
	d := NewDriverWithClient(nil, DriverConfig{Systemd: true, Privileged: &privileged})
	if d.privileged {
		t.Fatalf("privileged override ignored")
	}
}

func TestContainerBootConfigDerivesControlPlaneURL(t *testing.T) {
	t.Setenv("PORT", "9090")
	d := NewDriverWithClient(nil, DriverConfig{})
	boot := d.containerBootConfig(vm.BootConfig{})
	if got := boot.Env[workeragent.EnvControlPlaneURL]; got != "http://host.docker.internal:9090" {
		t.Fatalf("control plane url = %q", got)
	}
}

func TestContainerBootConfigPreservesConfiguredControlPlaneURL(t *testing.T) {
	d := NewDriverWithClient(nil, DriverConfig{})
	boot := d.containerBootConfig(vm.BootConfig{Env: map[string]string{
		workeragent.EnvControlPlaneURL: "http://control.example",
	}})
	if got := boot.Env[workeragent.EnvControlPlaneURL]; got != "http://control.example" {
		t.Fatalf("control plane url = %q", got)
	}
}

func TestDefaultControlPlaneURLAddsHostGateway(t *testing.T) {
	t.Setenv("PORT", "9090")
	d := NewDriverWithClient(nil, DriverConfig{})
	original := vm.BootConfig{}
	boot := d.containerBootConfig(original)

	if !controlPlaneURLDefaulted(original) {
		t.Fatalf("control plane URL defaulted = false, want true")
	}
	if !controlPlaneURLUsesHostGateway(boot.Env[workeragent.EnvControlPlaneURL]) {
		t.Fatalf("control plane URL = %q, want host-gateway URL", boot.Env[workeragent.EnvControlPlaneURL])
	}
	extraHosts := controlPlaneExtraHosts(controlPlaneURLDefaulted(original), boot.Env[workeragent.EnvControlPlaneURL])
	if len(extraHosts) != 1 || extraHosts[0] != "host.docker.internal:host-gateway" {
		t.Fatalf("extra hosts = %#v, want host gateway mapping", extraHosts)
	}
}

func TestConfiguredControlPlaneURLDoesNotCountAsDefaulted(t *testing.T) {
	boot := vm.BootConfig{Env: map[string]string{
		workeragent.EnvControlPlaneURL: "http://host.docker.internal:8080",
	}}

	if controlPlaneURLDefaulted(boot) {
		t.Fatalf("control plane URL defaulted = true, want false for configured URL")
	}
	if extraHosts := controlPlaneExtraHosts(controlPlaneURLDefaulted(boot), boot.Env[workeragent.EnvControlPlaneURL]); len(extraHosts) != 0 {
		t.Fatalf("extra hosts = %#v, want none for configured URL", extraHosts)
	}
}

func TestContainerLabelsIncludeSandboxIDByDefault(t *testing.T) {
	d := NewDriverWithClient(nil, DriverConfig{})
	labels := d.containerLabels(vm.InstanceSpec{
		Ref:  sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "sandbox-1"},
		Name: "instance-1",
		Boot: vm.BootConfig{Env: map[string]string{workeragent.EnvWorkerID: "worker-1"}},
	})

	if labels[labelSandboxID] != "sandbox-1" {
		t.Fatalf("sandbox label = %q, want sandbox-1", labels[labelSandboxID])
	}
	if labels[labelWorkerID] != "worker-1" {
		t.Fatalf("worker label = %q, want worker-1", labels[labelWorkerID])
	}
}

func TestContainerLabelsOmitSandboxIDForWorkerAgent(t *testing.T) {
	d := NewDriverWithClient(nil, DriverConfig{})
	labels := d.containerLabels(vm.InstanceSpec{
		Ref:      sandbox.SandboxRef{ProjectID: "project-1", SandboxID: "worker-worker-1"},
		Name:     "instance-1",
		Boot:     vm.BootConfig{Env: map[string]string{workeragent.EnvWorkerID: "worker-1"}},
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

func TestContainerMountsBindHostDockerSocketForWorkerAgent(t *testing.T) {
	d := NewDriverWithClient(nil, DriverConfig{DockerSocket: "/custom/docker.sock"})
	mounts := d.containerMounts(true, "worker-1")

	if !hasMountWithReadOnly(mounts, "/custom/docker.sock", dockerSocketPath, false) {
		t.Fatalf("mounts = %#v, missing host Docker socket bind mount", mounts)
	}
}

func TestContainerMountsBindConfiguredHostMountsForWorkerAgent(t *testing.T) {
	d := NewDriverWithClient(nil, DriverConfig{HostMounts: []HostMount{
		{Source: "/home", ReadOnly: true},
		{Source: "/Users", ReadOnly: true},
	}})
	mounts := d.containerMounts(true, "worker-1")

	if !hasMountWithReadOnly(mounts, "/home", "/host/home", true) {
		t.Fatalf("mounts = %#v, missing readonly /home host mount", mounts)
	}
	if !hasMountWithReadOnly(mounts, "/Users", "/host/Users", true) {
		t.Fatalf("mounts = %#v, missing readonly /Users host mount", mounts)
	}
}

func TestContainerMountsDoNotBindHostDockerSocketForNonWorkerAgent(t *testing.T) {
	d := NewDriverWithClient(nil, DriverConfig{DockerSocket: dockerSocketPath, HostMounts: []HostMount{{Source: "/home", ReadOnly: true}}})
	mounts := d.containerMounts(false, "worker-1")

	if hasMount(mounts, dockerSocketPath, dockerSocketPath) {
		t.Fatalf("mounts = %#v, host Docker socket should only be mounted for worker agents", mounts)
	}
	if hasMount(mounts, "/home", "/host/home") {
		t.Fatalf("mounts = %#v, host directories should only be mounted for worker agents", mounts)
	}
}

func TestHostMountsAreNormalized(t *testing.T) {
	d := NewDriverWithClient(nil, DriverConfig{HostMounts: []HostMount{
		{Source: "relative", ReadOnly: true},
		{Source: "/home/../home/", ReadOnly: true},
		{Source: "/home", ReadOnly: false},
	}})

	if len(d.hostMounts) != 1 {
		t.Fatalf("host mounts = %#v, want one normalized mount", d.hostMounts)
	}
	if d.hostMounts[0].Source != "/home" || !d.hostMounts[0].ReadOnly {
		t.Fatalf("host mount = %#v, want readonly /home", d.hostMounts[0])
	}
}

func TestHostMountJSONAcceptsDockerStyleStrings(t *testing.T) {
	var mounts []HostMount
	if err := json.Unmarshal([]byte(`["/home:ro","/var/lib/discobox:rw","/var/run/docker.sock"]`), &mounts); err != nil {
		t.Fatalf("decode host mounts: %v", err)
	}
	d := NewDriverWithClient(nil, DriverConfig{HostMounts: mounts})

	if !hasMountWithReadOnly(d.containerMounts(true, "worker-1"), "/home", "/host/home", true) {
		t.Fatalf("mounts = %#v, missing readonly /home mount", d.containerMounts(true, "worker-1"))
	}
	if !hasMountWithReadOnly(d.containerMounts(true, "worker-1"), "/var/lib/discobox", "/host/var/lib/discobox", false) {
		t.Fatalf("mounts = %#v, missing read-write /var/lib/discobox mount", d.containerMounts(true, "worker-1"))
	}
	if !hasMountWithReadOnly(d.containerMounts(true, "worker-1"), dockerSocketPath, "/host/var/run/docker.sock", false) {
		t.Fatalf("mounts = %#v, missing read-write host mount for Docker socket path", d.containerMounts(true, "worker-1"))
	}

	data, err := json.Marshal([]HostMount{{Source: "/home", ReadOnly: true}, {Source: dockerSocketPath}})
	if err != nil {
		t.Fatalf("encode host mounts: %v", err)
	}
	if string(data) != `["/home:ro","/var/run/docker.sock:rw"]` {
		t.Fatalf("encoded host mounts = %s, want [\"/home:ro\",\"/var/run/docker.sock:rw\"]", data)
	}
}

func TestConfiguredDockerSocketIsSeparateFromHostMountTargeting(t *testing.T) {
	d := NewDriverWithClient(nil, DriverConfig{
		DockerSocket: "/run/user/1000/docker.sock",
		HostMounts:   []HostMount{{Source: dockerSocketPath}},
	})
	mounts := d.containerMounts(true, "worker-1")

	if !hasMountWithReadOnly(mounts, "/run/user/1000/docker.sock", dockerSocketPath, false) {
		t.Fatalf("mounts = %#v, missing configured Docker socket bind", mounts)
	}
	if !hasMountWithReadOnly(mounts, dockerSocketPath, "/host/var/run/docker.sock", false) {
		t.Fatalf("mounts = %#v, hostMounts Docker socket path should still mount under /host", mounts)
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

func TestContainerReadyErrorTreatsRunningWithoutHealthcheckAsReady(t *testing.T) {
	inspect := container.InspectResponse{
		ID:    "1234567890abcdef",
		State: &container.State{Running: true},
	}
	if err := containerReadyError(inspect); err != nil {
		t.Fatalf("ready error = %v", err)
	}
	if containerHasHealth(inspect) {
		t.Fatalf("containerHasHealth = true")
	}
}

func TestContainerReadyErrorReportsHealthStates(t *testing.T) {
	starting := container.InspectResponse{
		ID:    "1234567890abcdef",
		State: &container.State{Running: true, Health: &container.Health{Status: "starting"}},
	}
	if err := containerReadyError(starting); err == nil || !strings.Contains(err.Error(), "starting") {
		t.Fatalf("starting error = %v", err)
	}
	if !containerHealthStarting(starting) {
		t.Fatalf("containerHealthStarting = false")
	}
	if !containerHasHealth(starting) {
		t.Fatalf("containerHasHealth = false")
	}

	unhealthy := container.InspectResponse{
		ID: "1234567890abcdef",
		State: &container.State{
			Running: true,
			Health: &container.Health{
				Status: "unhealthy",
				Log:    []*container.HealthcheckResult{{Output: "agent did not answer\n"}},
			},
		},
	}
	err := containerReadyError(unhealthy)
	if err == nil || !strings.Contains(err.Error(), "agent did not answer") {
		t.Fatalf("unhealthy error = %v", err)
	}
}

func TestContainerReadyErrorReportsStoppedContainer(t *testing.T) {
	err := containerReadyError(container.InspectResponse{
		ID:    "1234567890abcdef",
		State: &container.State{Status: "exited", Error: "process failed"},
	})
	if err == nil || err.Error() != "process failed" {
		t.Fatalf("stopped error = %v", err)
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

func hasMountWithReadOnly(mounts []mount.Mount, source, target string, readOnly bool) bool {
	for _, m := range mounts {
		if m.Type == mount.TypeBind && m.Source == source && m.Target == target && m.ReadOnly == readOnly {
			return true
		}
	}
	return false
}
