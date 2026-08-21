package dockerworker

import (
	"strconv"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	"github.com/discobox-ai/discobox/server/internal/model"
)

func testConsolePool() *model.Pool {
	return &model.Pool{ID: "pool-1", ProjectID: "project-1"}
}

// The console exists to look at the host the way the host sees itself, so every
// namespace is the host's and the host filesystem and Docker socket are both in
// place. A console missing any of these is a console that cannot answer the
// questions it is opened for.
func TestConsoleHostConfigSharesTheHost(t *testing.T) {
	engine := newTestEngine(t, Config{})
	hostConfig := engine.consoleHostConfig()

	if !hostConfig.Privileged {
		t.Fatal("console is not privileged")
	}
	if hostConfig.PidMode != "host" || hostConfig.IpcMode != container.IPCModeHost ||
		hostConfig.UTSMode != "host" || hostConfig.NetworkMode != "host" ||
		hostConfig.CgroupnsMode != container.CgroupnsModeHost {
		t.Fatalf("console namespaces = pid %q ipc %q uts %q net %q cgroup %q",
			hostConfig.PidMode, hostConfig.IpcMode, hostConfig.UTSMode, hostConfig.NetworkMode, hostConfig.CgroupnsMode)
	}

	var hostRoot, socket *mount.Mount
	for i := range hostConfig.Mounts {
		switch hostConfig.Mounts[i].Target {
		case consoleHostRoot:
			hostRoot = &hostConfig.Mounts[i]
		case dockerSocketPath:
			socket = &hostConfig.Mounts[i]
		}
	}
	if hostRoot == nil || hostRoot.Source != "/" || hostRoot.Type != mount.TypeBind {
		t.Fatalf("host root mount = %#v", hostRoot)
	}
	// Propagation stays at the daemon default: a stock host mounts / privately,
	// and asking for rslave there fails the container outright.
	if hostRoot.BindOptions != nil && hostRoot.BindOptions.Propagation != "" {
		t.Fatalf("host root propagation = %q, want the daemon default", hostRoot.BindOptions.Propagation)
	}
	if socket == nil || socket.Source != dockerSocketPath {
		t.Fatalf("docker socket mount = %#v", socket)
	}
}

// The console runs the pool image but not the pool agent: the image's
// entrypoint and healthcheck belong to the agent, and a console that inherited
// them would run an agent instead of a shell.
func TestConsoleConfigRunsAShellNotTheAgent(t *testing.T) {
	engine := newTestEngine(t, Config{Image: "pool-image"})
	config := engine.consoleConfig(&model.SandboxProviderInstance{ID: "provider-1"}, testConsolePool())

	if config.Image != "pool-image" {
		t.Fatalf("image = %q", config.Image)
	}
	if len(config.Entrypoint) != 0 {
		t.Fatalf("entrypoint = %#v, want cleared", config.Entrypoint)
	}
	if len(config.Cmd) == 0 || config.Cmd[0] != consoleShell {
		t.Fatalf("cmd = %#v", config.Cmd)
	}
	if config.Healthcheck == nil || len(config.Healthcheck.Test) != 1 || config.Healthcheck.Test[0] != "NONE" {
		t.Fatalf("healthcheck = %#v", config.Healthcheck)
	}
	if !config.Tty || !config.OpenStdin {
		t.Fatalf("tty = %v, openStdin = %v", config.Tty, config.OpenStdin)
	}
	if config.WorkingDir != consoleHostRoot {
		t.Fatalf("workingDir = %q", config.WorkingDir)
	}
}

// Pool runtime drift detection reconciles and removes containers labeled as
// pool agents. The console carries the pool's identity but must never carry
// that label, or the watcher would treat an operator's shell as a pool runtime.
func TestConsoleLabelsAreNotPoolAgentLabels(t *testing.T) {
	engine := newTestEngine(t, Config{})
	labels := engine.consoleLabels(&model.SandboxProviderInstance{ID: "provider-1"}, testConsolePool())

	if _, ok := labels[LabelPoolAgent]; ok {
		t.Fatalf("console carries the pool-agent label: %v", labels)
	}
	if labels[LabelPoolConsole] != "true" || labels[LabelPoolID] != "pool-1" ||
		labels[LabelProjectID] != "project-1" || labels[LabelProviderInstanceID] != "provider-1" {
		t.Fatalf("console labels = %v", labels)
	}
	if labels[LabelConsoleConfig] != strconv.Itoa(consoleConfigLayoutVersion) {
		t.Fatalf("console config revision = %q", labels[LabelConsoleConfig])
	}
}

// An existing console is reused, except when it was built from another image or
// an older console layout: reattaching to a console with the previous
// namespaces or mounts would silently give an operator the wrong host view.
func TestShouldReplaceConsoleContainer(t *testing.T) {
	engine := newTestEngine(t, Config{Image: "pool-image"})
	current := strconv.Itoa(consoleConfigLayoutVersion)

	for name, testCase := range map[string]struct {
		config *container.Config
		want   bool
	}{
		"matching":    {config: &container.Config{Image: "pool-image", Labels: map[string]string{LabelConsoleConfig: current}}, want: false},
		"other image": {config: &container.Config{Image: "old-image", Labels: map[string]string{LabelConsoleConfig: current}}, want: true},
		"old layout":  {config: &container.Config{Image: "pool-image", Labels: map[string]string{LabelConsoleConfig: "0"}}, want: true},
		"no labels":   {config: &container.Config{Image: "pool-image"}, want: true},
		"no config":   {config: nil, want: true},
	} {
		if got := engine.shouldReplaceConsoleContainer(container.InspectResponse{Config: testCase.config}); got != testCase.want {
			t.Fatalf("%s: shouldReplaceConsoleContainer = %v, want %v", name, got, testCase.want)
		}
	}
}

// The console is a second container on the same daemon as the pool it belongs
// to, so its name must not collide with the pool runtime's.
func TestConsoleContainerNameIsDistinct(t *testing.T) {
	if ConsoleContainerName("pool-1") == ContainerName("pool-1") {
		t.Fatal("console and pool container names collide")
	}
	if got := ConsoleContainerName("pool/1"); got != "discobox-console-pool-1" {
		t.Fatalf("console container name = %q", got)
	}
}
