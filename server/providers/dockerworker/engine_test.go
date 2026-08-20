package dockerworker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	"github.com/obot-platform/discobox/layout"
	poolagent "github.com/obot-platform/discobox/pool-agent"
	"github.com/obot-platform/discobox/server/internal/model"
	sandbox "github.com/obot-platform/discobox/server/internal/sandbox"
	"github.com/obot-platform/discobox/server/internal/transport"
)

type nopDriver struct{}

func (nopDriver) Close() error { return nil }
func (nopDriver) EnsureVM(context.Context, string, VMSpec) (*VMInfo, error) {
	return &VMInfo{ID: "local", Status: sandbox.StatusRunning}, nil
}
func (nopDriver) StopVM(context.Context, string) error   { return nil }
func (nopDriver) DeleteVM(context.Context, string) error { return nil }
func (nopDriver) InspectVM(context.Context, string) (*VMInfo, error) {
	return &VMInfo{ID: "local", Status: sandbox.StatusRunning}, nil
}
func (nopDriver) AcquireDockerClient(context.Context, string) (*DockerClientLease, error) {
	return nil, errors.New("no docker in unit tests")
}
func (nopDriver) AcquirePoolAgentClient(context.Context, string) (*transport.HTTPClientLease, error) {
	return nil, errors.New("no worker agent in unit tests")
}

type lifecycleDriver struct {
	nopDriver
	status       sandbox.Status
	stopCalls    int
	deleteCalls  int
	ensureCalled bool
}

func (d *lifecycleDriver) InspectVM(context.Context, string) (*VMInfo, error) {
	return &VMInfo{ID: "local", Status: d.status}, nil
}

func (d *lifecycleDriver) StopVM(context.Context, string) error {
	d.stopCalls++
	return nil
}

func (d *lifecycleDriver) DeleteVM(context.Context, string) error {
	d.deleteCalls++
	return nil
}

func (d *lifecycleDriver) EnsureVM(context.Context, string, VMSpec) (*VMInfo, error) {
	d.ensureCalled = true
	return &VMInfo{ID: "local", Status: sandbox.StatusRunning}, nil
}

func newTestEngine(t *testing.T, cfg Config) *Engine {
	t.Helper()
	if cfg.Image == "" {
		cfg.Image = "worker-image"
	}
	engine, err := New(cfg, nopDriver{})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	return engine
}

func TestRepairStopsUnhealthyVMWithoutDeletingPersistentState(t *testing.T) {
	driver := &lifecycleDriver{status: sandbox.StatusStopped}
	engine, err := New(Config{Image: "worker-image"}, driver)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = engine.RepairPool(
		ctx,
		nil,
		&model.SandboxProviderInstance{ID: "provider-1"},
		&model.Pool{ID: "pool-1", ProjectID: "project-1"},
		nil,
		"",
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("RepairPool error = %v, want canceled after VM lifecycle", err)
	}
	if driver.stopCalls != 1 || driver.deleteCalls != 0 || !driver.ensureCalled {
		t.Fatalf(
			"lifecycle calls = stop:%d delete:%d ensure:%v, want stop:1 delete:0 ensure:true",
			driver.stopCalls,
			driver.deleteCalls,
			driver.ensureCalled,
		)
	}
}

func TestNewDefaults(t *testing.T) {
	engine := newTestEngine(t, Config{})
	if engine.cfg.AgentPort != defaultAgentPort {
		t.Fatalf("agentPort = %d", engine.cfg.AgentPort)
	}
	if engine.cfg.DockerSocket != dockerSocketPath {
		t.Fatalf("dockerSocket = %q", engine.cfg.DockerSocket)
	}
	// The worker always runs systemd as PID 1, so it is privileged and runs the
	// pool agent unless the caller overrides either.
	if !engine.privileged() {
		t.Fatalf("privileged default = false, want true")
	}
	if len(engine.cfg.Command) != 1 || engine.cfg.Command[0] != "/usr/local/bin/discobox-pool-agent" {
		t.Fatalf("command = %#v", engine.cfg.Command)
	}
}

func TestNewHonorsPrivilegedOverride(t *testing.T) {
	privileged := false
	engine := newTestEngine(t, Config{Privileged: &privileged})
	if engine.privileged() {
		t.Fatalf("privileged override ignored")
	}
}

// The engine validates the transport URLs it will hand the agent, so a
// misconfigured backend fails at construction rather than at pool boot.
func TestNewRejectsUnusableTransportURLs(t *testing.T) {
	for _, cfg := range []Config{
		{Image: "worker-image", AgentListenURL: "ftp://nope"},
		{Image: "worker-image", AgentListenURL: "vsock://2"},
		{Image: "worker-image", ControlPlaneURL: "ftp://nope"},
	} {
		if _, err := New(cfg, nopDriver{}); err == nil {
			t.Fatalf("New(%#v) succeeded with an unusable transport URL", cfg)
		}
	}
}

// A VSOCK-only pool is now expressed purely as URLs.
func TestNewAcceptsVSOCKTransportURLs(t *testing.T) {
	if _, err := New(Config{
		Image:           "worker-image",
		AgentListenURL:  "vsock://:3002",
		ControlPlaneURL: "vsock://2:3001",
	}, nopDriver{}); err != nil {
		t.Fatalf("New with VSOCK transport URLs: %v", err)
	}
}

// With no listen URL configured the engine defaults to TCP on the agent port,
// so existing Docker pools keep their published port unchanged.
func TestNewDefaultsAgentListenToTCP(t *testing.T) {
	engine, err := New(Config{Image: "worker-image", AgentPort: 3002}, nopDriver{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got, want := engine.cfg.AgentListenURL, "http://0.0.0.0:3002"; got != want {
		t.Fatalf("default agent listen URL = %q, want %q", got, want)
	}
}

func TestBootEnvRendersBootstrapContract(t *testing.T) {
	env := BootEnv(poolagent.Bootstrap{
		ControlPlaneURL: "http://control.example",
		ProjectID:       "project-1",
		PoolID:          "pool-1",
		Token:           "token-1",
		ControlPlaneKey: "key-1",
		AgentListenURL:  "http://0.0.0.0:3002",
		HostMountPrefix: "/host",
	})
	want := map[string]string{
		poolagent.EnvControlPlaneURL: "http://control.example",
		poolagent.EnvProjectID:       "project-1",
		poolagent.EnvPoolID:          "pool-1",
		poolagent.EnvBootstrapToken:  "token-1",
		poolagent.EnvControlPlaneKey: "key-1",
		poolagent.EnvAgentListenURL:  "http://0.0.0.0:3002",
		poolagent.EnvHostMountPrefix: "/host",
	}
	for key, value := range want {
		if env[key] != value {
			t.Fatalf("env[%q] = %q, want %q", key, env[key], value)
		}
	}
	if len(env) != len(want) {
		t.Fatalf("env = %#v, want no empty values", env)
	}
}

// A VSOCK pool renders the same two variables as any other backend; the scheme
// is the only difference.
func TestBootEnvRendersVSOCKTransportAsURLs(t *testing.T) {
	env := BootEnv(poolagent.Bootstrap{
		PoolID:          "pool-1",
		ControlPlaneURL: "vsock://2:3001",
		AgentListenURL:  "vsock://:3002",
	})
	if env[poolagent.EnvAgentListenURL] != "vsock://:3002" {
		t.Fatalf("agent listen env = %q", env[poolagent.EnvAgentListenURL])
	}
	if env[poolagent.EnvControlPlaneURL] != "vsock://2:3001" {
		t.Fatalf("control plane env = %q", env[poolagent.EnvControlPlaneURL])
	}
}

func TestBootEnvOmitsEmptyValues(t *testing.T) {
	env := BootEnv(poolagent.Bootstrap{PoolID: "pool-1"})
	if _, ok := env[poolagent.EnvControlPlaneURL]; ok {
		t.Fatalf("env = %#v, want no empty control plane URL", env)
	}
	if _, ok := env[poolagent.EnvAgentListenURL]; ok {
		t.Fatalf("env = %#v, want no empty agent listen URL", env)
	}
}

func TestContainerLabelsIdentifyPool(t *testing.T) {
	engine := newTestEngine(t, Config{Labels: map[string]string{"discobox.provider_type": "docker"}})
	labels := engine.containerLabels(
		&model.SandboxProviderInstance{ID: "provider-1"},
		&model.Pool{ID: "pool-1", ProjectID: "project-1"},
	)
	for key, want := range map[string]string{
		LabelManaged:             "true",
		LabelPoolAgent:           "true",
		LabelProjectID:           "project-1",
		LabelProviderInstanceID:  "provider-1",
		LabelPoolID:              "pool-1",
		"discobox.provider_type": "docker",
	} {
		if labels[key] != want {
			t.Fatalf("labels[%q] = %q, want %q", key, labels[key], want)
		}
	}
	if labels[LabelPoolConfig] != engine.ConfigRevision() {
		t.Fatalf("config revision label = %q, want %q", labels[LabelPoolConfig], engine.ConfigRevision())
	}
}

func TestContainerMountsBindDockerSocket(t *testing.T) {
	engine := newTestEngine(t, Config{DockerSocket: "/custom/docker.sock"})
	mounts := engine.containerMounts("worker-1")

	if !hasMountWithReadOnly(mounts, "/custom/docker.sock", dockerSocketPath, false) {
		t.Fatalf("mounts = %#v, missing Docker socket bind mount", mounts)
	}
}

func TestContainerMountsBindConfiguredHostMounts(t *testing.T) {
	engine := newTestEngine(t, Config{HostMounts: []HostMount{
		{Source: "/home", ReadOnly: true},
		{Source: "/Users", ReadOnly: true},
	}})
	mounts := engine.containerMounts("worker-1")

	if !hasMountWithReadOnly(mounts, "/home", "/host/home", true) {
		t.Fatalf("mounts = %#v, missing readonly /home host mount", mounts)
	}
	if !hasMountWithReadOnly(mounts, "/Users", "/host/Users", true) {
		t.Fatalf("mounts = %#v, missing readonly /Users host mount", mounts)
	}
}

// Every state tree is bound at the very path the container addresses it by, so
// code inside the pool never has to know where the host keeps it.
func TestContainerMountsBindEveryStateTreeAtItsContainerPath(t *testing.T) {
	engine := newTestEngine(t, Config{})
	mounts := engine.containerMounts("worker-1")

	for _, tree := range layout.MountRoots() {
		if !hasMountWithReadOnly(mounts, tree, tree, false) {
			t.Fatalf("mounts = %#v, missing bind mount for state tree %q", mounts, tree)
		}
		m := findMount(mounts, tree, tree)
		if m == nil || m.BindOptions == nil || !m.BindOptions.CreateMountpoint {
			t.Fatalf("mounts = %#v, state tree %q should create its mountpoint", mounts, tree)
		}
	}
}

// A driver whose daemon keeps state somewhere else relocates only the host
// side; the container path is unchanged.
func TestContainerMountsRelocateOnlyTheHostSideOfStateTrees(t *testing.T) {
	engine := newTestEngine(t, Config{HostStateRoot: "/var/lib/docker/discobox"})
	mounts := engine.containerMounts("worker-1")

	for _, tree := range layout.MountRoots() {
		host := "/var/lib/docker/discobox" + strings.TrimPrefix(tree, layout.ContainerRoot)
		if !hasMountWithReadOnly(mounts, host, tree, false) {
			t.Fatalf("mounts = %#v, state tree %q should bind from %q", mounts, tree, host)
		}
	}
}

func TestConfiguredDockerSocketIsSeparateFromHostMountTargeting(t *testing.T) {
	engine := newTestEngine(t, Config{
		DockerSocket: "/run/user/1000/docker.sock",
		HostMounts:   []HostMount{{Source: dockerSocketPath}},
	})
	mounts := engine.containerMounts("worker-1")

	if !hasMountWithReadOnly(mounts, "/run/user/1000/docker.sock", dockerSocketPath, false) {
		t.Fatalf("mounts = %#v, missing configured Docker socket bind", mounts)
	}
	if !hasMountWithReadOnly(mounts, dockerSocketPath, "/host/var/run/docker.sock", false) {
		t.Fatalf("mounts = %#v, hostMounts Docker socket path should still mount under /host", mounts)
	}
}

func TestContainerMountsScopeVolumes(t *testing.T) {
	engine := newTestEngine(t, Config{})
	mounts := engine.containerMounts("worker:one")

	if !hasVolumeMount(mounts, "discobox-pool-worker-one-docker", "/var/lib/docker") {
		t.Fatalf("mounts = %#v, missing pool-scoped Docker volume", mounts)
	}
	// The nested daemon's own storage stays a named volume: it is the pool's
	// scratch image store, and Docker's own graph driver needs a real Linux
	// filesystem underneath it. Discobox state does not — it is bound.
	for _, m := range mounts {
		if m.Type == mount.TypeVolume && strings.HasPrefix(m.Target, layout.ContainerRoot) {
			t.Fatalf("mounts = %#v, Discobox state must be bound, not a named volume", mounts)
		}
	}
}

func TestHostMountsAreNormalized(t *testing.T) {
	engine := newTestEngine(t, Config{HostMounts: []HostMount{
		{Source: "relative", ReadOnly: true},
		{Source: "/home/../home/", ReadOnly: true},
		{Source: "/home", ReadOnly: false},
	}})

	if len(engine.cfg.HostMounts) != 1 {
		t.Fatalf("host mounts = %#v, want one normalized mount", engine.cfg.HostMounts)
	}
	if engine.cfg.HostMounts[0].Source != "/home" || !engine.cfg.HostMounts[0].ReadOnly {
		t.Fatalf("host mount = %#v, want readonly /home", engine.cfg.HostMounts[0])
	}
}

func TestHostMountJSONAcceptsDockerStyleStrings(t *testing.T) {
	var mounts []HostMount
	if err := json.Unmarshal([]byte(`["/home:ro","/var/lib/discobox:rw","/var/run/docker.sock"]`), &mounts); err != nil {
		t.Fatalf("decode host mounts: %v", err)
	}
	engine := newTestEngine(t, Config{HostMounts: mounts})
	containerMounts := engine.containerMounts("worker-1")

	if !hasMountWithReadOnly(containerMounts, "/home", "/host/home", true) {
		t.Fatalf("mounts = %#v, missing readonly /home mount", containerMounts)
	}
	if !hasMountWithReadOnly(containerMounts, "/var/lib/discobox", "/host/var/lib/discobox", false) {
		t.Fatalf("mounts = %#v, missing read-write /var/lib/discobox mount", containerMounts)
	}
	if !hasMountWithReadOnly(containerMounts, dockerSocketPath, "/host/var/run/docker.sock", false) {
		t.Fatalf("mounts = %#v, missing read-write host mount for Docker socket path", containerMounts)
	}

	data, err := json.Marshal([]HostMount{{Source: "/home", ReadOnly: true}, {Source: dockerSocketPath}})
	if err != nil {
		t.Fatalf("encode host mounts: %v", err)
	}
	if string(data) != `["/home:ro","/var/run/docker.sock:rw"]` {
		t.Fatalf("encoded host mounts = %s, want [\"/home:ro\",\"/var/run/docker.sock:rw\"]", data)
	}
}

func TestContainerNameUsesPoolID(t *testing.T) {
	if got := ContainerName("worker:one"); got != "discobox-vm-worker-one" {
		t.Fatalf("container name = %q", got)
	}
	if got := ContainerName(""); got != "discobox-vm-vm" {
		t.Fatalf("fallback container name = %q", got)
	}
}

func TestScopedVolumeNames(t *testing.T) {
	if got := poolScopedVolumeName("worker:one", "docker"); got != "discobox-pool-worker-one-docker" {
		t.Fatalf("pool volume name = %q", got)
	}
	if got := projectScopedVolumeName("project:one", "discobox"); got != "discobox-project-project-one-discobox" {
		t.Fatalf("project volume name = %q", got)
	}
	if got := poolScopedVolumeName("", "docker"); got != "discobox-pool-unknown-docker" {
		t.Fatalf("worker fallback volume name = %q", got)
	}
}

func TestConfigRevisionChangesWithContainerConfig(t *testing.T) {
	base := newTestEngine(t, Config{})
	if base.ConfigRevision() == "" {
		t.Fatalf("config revision is empty")
	}
	withMount := newTestEngine(t, Config{HostMounts: []HostMount{{Source: "/home", ReadOnly: true}}})
	if base.ConfigRevision() == withMount.ConfigRevision() {
		t.Fatalf("config revision did not change after host mount change")
	}
	withImage := newTestEngine(t, Config{Image: "other-image"})
	if base.ConfigRevision() == withImage.ConfigRevision() {
		t.Fatalf("config revision did not change after image change")
	}
}

func TestShouldRemoveExistingContainerDetectsDrift(t *testing.T) {
	existing := container.InspectResponse{
		Config: &container.Config{
			Image:  "worker-image",
			Labels: map[string]string{LabelPoolConfig: "old"},
		},
		State: &container.State{Running: true},
	}
	desired := map[string]string{LabelPoolAgent: "true", LabelPoolConfig: "new"}

	if !shouldRemoveExistingContainer(existing, "worker-image", desired) {
		t.Fatalf("shouldRemoveExistingContainer = false, want true for stale worker config")
	}
	existing.Config.Labels[LabelPoolConfig] = "new"
	existing.Config.Labels[LabelPoolAgent] = "true"
	if shouldRemoveExistingContainer(existing, "worker-image", desired) {
		t.Fatalf("shouldRemoveExistingContainer = true, want false for matching config")
	}
	existing.State.Running = false
	if !shouldRemoveExistingContainer(existing, "worker-image", desired) {
		t.Fatalf("shouldRemoveExistingContainer = false, want true for stopped container")
	}
}

func TestShouldReconcileWorkerContainer(t *testing.T) {
	engine := newTestEngine(t, Config{})
	if engine.ShouldReconcileWorkerContainer("worker-image", map[string]string{LabelPoolConfig: engine.ConfigRevision()}) {
		t.Fatalf("ShouldReconcileWorkerContainer = true, want false for matching container")
	}
	if !engine.ShouldReconcileWorkerContainer("other-image", map[string]string{LabelPoolConfig: engine.ConfigRevision()}) {
		t.Fatalf("ShouldReconcileWorkerContainer = false, want true for image drift")
	}
	if !engine.ShouldReconcileWorkerContainer("worker-image", map[string]string{LabelPoolConfig: "old"}) {
		t.Fatalf("ShouldReconcileWorkerContainer = false, want true for config drift")
	}
}

func TestDecodeRuntimeState(t *testing.T) {
	if _, err := DecodeRuntimeState(nil); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("empty state error = %v, want ErrNotFound", err)
	}
	if _, err := DecodeRuntimeState([]byte(`{}`)); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("identity-less state error = %v, want ErrNotFound", err)
	}
	state, err := DecodeRuntimeState([]byte(`{"instanceId":"vm-1","containerId":"container-1"}`))
	if err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if state.InstanceID != "vm-1" || state.ContainerID != "container-1" {
		t.Fatalf("state = %#v", state)
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

	unhealthy := container.InspectResponse{
		ID: "1234567890abcdef",
		State: &container.State{
			Running: true,
			Health: &container.Health{
				Status: "unhealthy",
				Log:    []*container.HealthcheckResult{{Output: "harness did not answer\n"}},
			},
		},
	}
	err := containerReadyError(unhealthy)
	if err == nil || !strings.Contains(err.Error(), "harness did not answer") {
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

func hasMountWithReadOnly(mounts []mount.Mount, source, target string, readOnly bool) bool {
	for _, m := range mounts {
		if m.Type == mount.TypeBind && m.Source == source && m.Target == target && m.ReadOnly == readOnly {
			return true
		}
	}
	return false
}

func findMount(mounts []mount.Mount, source, target string) *mount.Mount {
	for _, m := range mounts {
		if m.Type == mount.TypeBind && m.Source == source && m.Target == target {
			return &m
		}
	}
	return nil
}

func hasVolumeMount(mounts []mount.Mount, source, target string) bool {
	for _, m := range mounts {
		if m.Type == mount.TypeVolume && m.Source == source && m.Target == target {
			return true
		}
	}
	return false
}

// deadDaemonDriver owns a VM whose Docker daemon cannot be reached: a guest
// that booted but never brought dockerd up, which is the case the pool host
// console exists for.
type deadDaemonDriver struct {
	nopDriver
	deleteCalls int
}

func (d *deadDaemonDriver) AcquireDockerClient(context.Context, string) (*DockerClientLease, error) {
	cli, err := NewDockerClientForHost("unix:///nonexistent/discobox-dead-daemon.sock")
	if err != nil {
		return nil, err
	}
	return NewDockerClientLease(cli, func() { _ = cli.Close() }), nil
}

func (d *deadDaemonDriver) DeleteVM(context.Context, string) error {
	d.deleteCalls++
	return nil
}

// refusingDaemonDriver reaches a daemon that answers and then refuses.
type refusingDaemonDriver struct {
	nopDriver
	host        string
	deleteCalls int
}

func (d *refusingDaemonDriver) AcquireDockerClient(context.Context, string) (*DockerClientLease, error) {
	// Built the way the other daemon fakes here are, so the client talks plain
	// HTTP to the test server instead of negotiating TLS.
	cli, err := testDockerClient(d.host)
	if err != nil {
		return nil, err
	}
	return NewDockerClientLease(cli, func() { _ = cli.Close() }), nil
}

func (d *refusingDaemonDriver) DeleteVM(context.Context, string) error {
	d.deleteCalls++
	return nil
}

// A pool whose daemon cannot be reached must still delete. Nothing about
// retrying makes a daemon reachable, so refusing here strands the pool row and
// its disks forever. Skipping the container removal leaks nothing: DeleteVM
// destroys the guest and everything in it, and on the local Docker driver —
// where DeleteVM is a no-op — the drift watcher reclaims a managed pool
// container that no longer has a pool row.
func TestRemovePoolDeletesTheVMWhenItsDaemonIsUnreachable(t *testing.T) {
	driver := &deadDaemonDriver{}
	engine, err := New(Config{Image: "worker-image"}, driver)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pool := &model.Pool{ID: "pool-1", Ready: true, Schedulable: true, Degraded: true}
	if err := engine.RemovePool(t.Context(), &model.Project{}, &model.SandboxProviderInstance{}, pool); err != nil {
		t.Fatalf("RemovePool: %v", err)
	}
	if driver.deleteCalls != 1 {
		t.Errorf("DeleteVM calls = %d, want 1", driver.deleteCalls)
	}
	if pool.Ready || pool.Schedulable || pool.Degraded {
		t.Errorf("pool runtime flags survived removal: %+v", pool)
	}
	if pool.RuntimeState != nil {
		t.Error("pool runtime state survived removal")
	}
}

// A daemon that answers and refuses is reporting something a retry can fix, so
// that error must keep driving the reconcile rather than being swallowed
// alongside the unreachable case.
func TestRemovePoolSurfacesAReachableDaemonsFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"device or resource busy"}`))
	}))
	t.Cleanup(server.Close)

	driver := &refusingDaemonDriver{host: server.URL}
	engine, err := New(Config{Image: "worker-image"}, driver)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pool := &model.Pool{ID: "pool-1"}
	if err := engine.RemovePool(t.Context(), &model.Project{}, &model.SandboxProviderInstance{}, pool); err == nil {
		t.Fatal("RemovePool succeeded against a daemon that refused the removal")
	}
	if driver.deleteCalls != 0 {
		t.Errorf("DeleteVM was called %d times despite a live daemon refusing", driver.deleteCalls)
	}
}
