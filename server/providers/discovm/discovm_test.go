package discovm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/adrg/xdg"
	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/boxd"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

// isolateStateRoot points the host's disco-vm state root at a directory of
// the test's own.
func isolateStateRoot(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	xdg.Reload()
	t.Cleanup(xdg.Reload)
}

// fakeConfig is a provider on disco-vm's fake driver, whose guest agent is
// this test binary, which TestMain lets play one.
func fakeConfig(t *testing.T) Config {
	t.Helper()
	return Config{Driver: "fake"}
}

func fakeInstance(t *testing.T) *model.SandboxProviderInstance {
	t.Helper()
	config, err := json.Marshal(fakeConfig(t)) //nolint:gosec // G117: a fake driver's configuration, with no key
	if err != nil {
		t.Fatal(err)
	}
	return &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: ProviderType, Config: config}
}

// fakeBases is the fake driver's base catalog in these tests: what a pool
// machine boots from, as one on boxd boots its default image.
var fakeBases = []machine.Base{
	{Name: "fake-default", ID: "fake-default-id", GuestOS: machine.OS(runtime.GOOS)},
	{Name: "fake-other", ID: "fake-other-id", GuestOS: machine.OS(runtime.GOOS)},
}

// withFakeBases gives the fake driver the bases it lists, which it reads when
// it is constructed.
func withFakeBases(t *testing.T, bases []machine.Base) {
	t.Helper()
	data, err := json.Marshal(bases)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fake.BasesEnv, string(data))
}

// openRuntime is newRuntime for the provider instance prov_test, closed when
// the test ends as the server closes it.
func openRuntime(t *testing.T, cfg Config) *Runtime {
	t.Helper()
	r, err := newRuntime(cfg, "prov_test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// newFakePool is a runtime on the fake driver that hosts its pools in
// machines booted from a base, as a remote driver's are, through
// poolTestDriver, which it returns.
func newFakePool(t *testing.T, base string) (*Runtime, *poolTestDriver) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake guest's console and log are POSIX shell commands")
	}
	isolateStateRoot(t)
	withFakeBases(t, fakeBases)
	return fakePoolRuntime(t, base)
}

// fakePoolRuntime is newFakePool on the bases the test already set.
func fakePoolRuntime(t *testing.T, base string) (*Runtime, *poolTestDriver) {
	t.Helper()
	r := openRuntime(t, fakeConfig(t))
	driver := &poolTestDriver{Driver: r.engine.Driver}
	e, err := engine.Open(driver, r.engine.Project)
	if err != nil {
		t.Fatal(err)
	}
	r.engine = e
	r.host = fakePoolMachine(e, base)
	return r, driver
}

// A pool hosted in a machine: the runtime creates and removes the pool,
// opens its logs, and opens its console. Logs, console and removal go through
// the pool provider the server's factory wraps a runtime in
// (poolruntime.New), around this one.
func TestFakePoolMachineLifecycle(t *testing.T) {
	r, _ := newFakePool(t, "")
	ctx := context.Background()
	instance := fakeInstance(t)
	provider := poolruntime.New(r, Definition(), nil)
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })

	begun := 0
	begin := func(context.Context) error { begun++; return nil }
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), begin); err != nil {
		t.Fatalf("EnsurePool() error = %v", err)
	}
	inst, err := r.host.(*poolMachine).instance(ctx, pool)
	if err != nil {
		t.Fatalf("no machine named for the pool: %v", err)
	}
	if state := r.engine.State(ctx, inst); state != engine.Running {
		t.Fatalf("pool machine is %s, want running", state)
	}
	if inst.Base == nil || inst.Base.ID != fakeBases[0].ID || inst.ImageID != "" {
		t.Fatalf("pool machine runs from base %+v, image %q, want the driver's first base and no image", inst.Base, inst.ImageID)
	}
	if begun != 1 || pool.State != model.PoolStateRegistering || pool.Ready || pool.Schedulable {
		t.Fatalf("after create: begun=%d state=%q ready=%v schedulable=%v, want 1 registering false false", begun, pool.State, pool.Ready, pool.Schedulable)
	}
	if recorded := recordOf(t, pool); recorded.Instance != inst.ID {
		t.Fatalf("RuntimeState = %s, want instance %s", pool.RuntimeState, inst.ID)
	}

	logs, err := provider.OpenLogs(ctx, instance, pool, sandbox.PoolLogOptions{Tail: 2})
	if err != nil {
		t.Fatalf("OpenLogs() error = %v", err)
	}
	text, err := io.ReadAll(logs)
	_ = logs.Close()
	if err != nil || string(text) != "4\n5\n" || logs.Source != "fake machine log" {
		t.Fatalf("OpenLogs() read %q from %q (%v), want the last two lines of the machine's log", text, logs.Source, err)
	}

	console, err := provider.OpenConsole(ctx, instance, pool, sandbox.ConsoleOptions{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatalf("OpenConsole() error = %v", err)
	}
	defer console.Close()
	if _, err := io.WriteString(console, "echo console-$((40+2)); exit 3\n"); err != nil {
		t.Fatal(err)
	}
	waitForOutput(t, console, "console-42")
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if code, err := console.Wait(waitCtx); err != nil || code != 3 {
		t.Fatalf("console Wait() = %d, %v, want the shell's exit 3", code, err)
	}

	if err := provider.RemovePool(ctx, nil, nil, instance, pool); err != nil {
		t.Fatalf("RemovePool() error = %v", err)
	}
	if _, err := r.host.(*poolMachine).instance(ctx, pool); !errors.Is(err, errNoPoolMachine) {
		t.Fatalf("pool machine still exists after RemovePool: %v", err)
	}
	if pool.RuntimeState != nil {
		t.Fatalf("RuntimeState = %s after RemovePool, want none", pool.RuntimeState)
	}
	// Removing a pool whose machine is gone is already done.
	if err := provider.RemovePool(ctx, nil, nil, instance, pool); err != nil {
		t.Fatalf("second RemovePool() error = %v", err)
	}
}

// A pool machine boots the base the provider names by its ID, rather than the
// driver's first, and its bootstrap names the same base for its sandboxes. A
// name is not taken for an ID, since a name can be given to another base; a
// driver with no base to offer creates nothing, saying what to configure.
func TestFakePoolMachineBootsTheConfiguredBase(t *testing.T) {
	ctx := context.Background()
	byName, _ := newFakePool(t, fakeBases[1].Name)
	pool13 := &model.Pool{ID: "pool-13"}
	err := byName.EnsurePool(ctx, nil, nil, pool13, newTestMaker(), nil)
	if err == nil || !strings.Contains(err.Error(), "no base with ID") {
		t.Fatalf("EnsurePool() with a base named, not identified = %v, want a refusal", err)
	}
	if _, err := byName.host.(*poolMachine).instance(ctx, pool13); !errors.Is(err, errNoPoolMachine) {
		t.Fatalf("a machine was made from a base named, not identified: %v", err)
	}

	r, driver := newFakePool(t, fakeBases[1].ID)
	pool := &model.Pool{ID: "pool-10", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatalf("EnsurePool() error = %v", err)
	}
	inst, err := r.host.(*poolMachine).instance(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if inst.Base == nil || inst.Base.ID != fakeBases[1].ID {
		t.Fatalf("pool machine runs from base %+v, want %s", inst.Base, fakeBases[1].Name)
	}
	if driver.applied[0].MachineBase != fakeBases[1].ID {
		t.Fatalf("bootstrap's machine base = %q, want the configured %q", driver.applied[0].MachineBase, fakeBases[1].ID)
	}

	isolateStateRoot(t)
	withFakeBases(t, nil)
	bare, _ := fakePoolRuntime(t, "")
	pool11 := &model.Pool{ID: "pool-11"}
	err = bare.EnsurePool(ctx, nil, nil, pool11, newTestMaker(), nil)
	if err == nil || !strings.Contains(err.Error(), "no base") || !strings.Contains(err.Error(), "provider's base") {
		t.Fatalf("EnsurePool() on a driver with no base = %v, want a refusal naming the provider's base", err)
	}
	if _, err := bare.host.(*poolMachine).instance(ctx, pool11); !errors.Is(err, errNoPoolMachine) {
		t.Fatalf("a machine was made with no base: %v", err)
	}
}

// No driver builds an image: a remote driver's pool machine and sandboxes
// boot from the driver's base, and a local driver has no image defined. Each
// says which, before reading anything.
func TestBuildGuestImageIsUnsupported(t *testing.T) {
	isolateStateRoot(t)
	r := openRuntime(t, Config{Driver: "boxd"})
	if isLocal(r.engine.Driver) {
		t.Fatal("boxd is a local driver")
	}
	for _, opts := range []sandbox.GuestImageBuildOptions{{}, {SourceDir: t.TempDir(), RestartHost: true}} {
		_, err := r.BuildGuestImage(context.Background(), nil, &model.Pool{ID: "pool-12"}, opts)
		if !errors.Is(err, sandbox.ErrGuestImageBuildUnsupported) || !strings.Contains(err.Error(), "driver's base") {
			t.Fatalf("BuildGuestImage(%+v) on boxd = %v, want ErrGuestImageBuildUnsupported naming the base", opts, err)
		}
	}
	local := openRuntime(t, fakeConfig(t))
	_, err := local.BuildGuestImage(context.Background(), nil, &model.Pool{ID: "pool-12"}, sandbox.GuestImageBuildOptions{SourceDir: t.TempDir()})
	if !errors.Is(err, sandbox.ErrGuestImageBuildUnsupported) || !strings.Contains(err.Error(), "no VM image is defined for the fake driver") {
		t.Fatalf("BuildGuestImage() on a local driver = %v, want ErrGuestImageBuildUnsupported", err)
	}
}

// The console and the log are read from inside the machine, so a pool with no
// machine says so rather than hanging.
func TestFakePoolWithoutAMachineHasNoConsoleOrLog(t *testing.T) {
	isolateStateRoot(t)
	r := openRuntime(t, fakeConfig(t))
	r.host = fakePoolMachine(r.engine, "")
	pool := &model.Pool{ID: "pool-3"}
	if _, err := r.OpenConsole(context.Background(), nil, pool, sandbox.ConsoleOptions{}); !errors.Is(err, errNoPoolMachine) {
		t.Fatalf("OpenConsole() error = %v, want not found", err)
	}
	if _, err := r.OpenLogs(context.Background(), nil, pool, sandbox.PoolLogOptions{}); !errors.Is(err, errNoPoolMachine) {
		t.Fatalf("OpenLogs() error = %v, want not found", err)
	}
	if _, err := r.AcquirePoolAgentClient(context.Background(), pool); !errors.Is(err, errNoPoolMachine) {
		t.Fatalf("AcquirePoolAgentClient() error = %v, want not found", err)
	}
}

// A host pool agent (vz's shape) has no console to open, and its log is the
// file the supervised agent writes.
func TestHostAgentRefusesAConsoleAndReadsTheAgentLog(t *testing.T) {
	h := &hostAgent{root: t.TempDir()}
	pool := &model.Pool{ID: "pool-4"}
	ctx := context.Background()
	if _, err := h.openConsole(ctx, pool, sandbox.ConsoleOptions{}); !errors.Is(err, sandbox.ErrPoolConsoleUnsupported) {
		t.Fatalf("openConsole() error = %v, want ErrPoolConsoleUnsupported", err)
	}
	if err := h.ensurePoolHost(ctx, pool, nil, nil); !errors.Is(err, errHostAgentNotStaged) {
		t.Fatalf("ensurePoolHost() error = %v, want not staged yet", err)
	}
	if err := os.MkdirAll(h.poolDir(pool.ID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.poolDir(pool.ID), "pool-agent.log"), []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs, err := h.openLogs(ctx, pool, sandbox.PoolLogOptions{Tail: 1})
	if err != nil {
		t.Fatalf("openLogs() error = %v", err)
	}
	text, _ := io.ReadAll(logs)
	_ = logs.Close()
	if string(text) != "three\n" {
		t.Fatalf("openLogs() read %q, want the last line", text)
	}
	if err := h.removePoolHost(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.poolDir(pool.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pool directory survived removal: %v", err)
	}
}

func TestValidateNamesTheDriversInThisBuild(t *testing.T) {
	for _, tc := range []struct{ config, want string }{
		{`{}`, "driver is required"},
		{`{"driver":"hyperv"}`, `driver "hyperv" is not available in this build`},
	} {
		err := Validate(json.RawMessage(tc.config))
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "boxd") {
			t.Errorf("Validate(%s) = %v, want %q naming boxd", tc.config, err, tc.want)
		}
	}
}

// A remote driver needs its key and the URL its pool machines reach the
// server at, said rather than guessed; a local driver takes neither.
func TestValidateWhatADriverNeeds(t *testing.T) {
	for _, tc := range []struct{ config, want string }{
		{`{"driver":"boxd"}`, "apiKey"},
		{`{"driver":"boxd","apiKey":"k"}`, "controlPlaneUrl"},
		{`{"driver":"boxd","apiKey":"k","controlPlaneUrl":"unix:///run/discobox.sock"}`, "http or https"},
		{`{"driver":"boxd","apiKey":"k","controlPlaneUrl":"https://discobox.example.com","deliverProviderKey":true}`, ""},
		{`{"driver":"fake","apiKey":"k"}`, "takes no API key"},
		{`{"driver":"fake"}`, ""},
	} {
		err := Validate(json.RawMessage(tc.config))
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("Validate(%s) = %v, want %q", tc.config, err, tc.want)
		}
	}
}

// The factory the server registers builds a pool-backed provider over the
// runtime, whose shims are this binary.
func TestFactoryBuildsAPoolProvider(t *testing.T) {
	isolateStateRoot(t)
	provider, err := FactoryWithPoolManager(nil)(context.Background(), fakeInstance(t))
	if err != nil {
		t.Fatalf("factory error = %v", err)
	}
	defer provider.Close()
	if _, ok := provider.(*poolruntime.Provider); !ok {
		t.Fatalf("factory built %T, want *poolruntime.Provider", provider)
	}
	if provider.Definition().Name != "vm" {
		t.Fatalf("Definition().Name = %q", provider.Definition().Name)
	}
	isolateStateRoot(t)
	cfg, err := driverConfig("fake", localProject, stateRoot("fake"), nil)
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	if want := shimCommand(exe, stateRoot("fake"), "fake", localProject); strings.Join(cfg.ShimCommand, " ") != strings.Join(want, " ") {
		t.Fatalf("ShimCommand = %q, want %q", cfg.ShimCommand, want)
	}
	if cfg.Project != localProject || cfg.Dir != stateRoot("fake") {
		t.Fatalf("driver config project %q dir %q, want %q in the state root", cfg.Project, cfg.Dir, localProject)
	}
}

// A remote driver calls its service with the provider's API key, from its
// configuration, never the server's environment; a provider with none fails
// every call saying what to set.
func TestDriverKeyIsTheProvidersConfiguration(t *testing.T) {
	t.Setenv(boxd.APIKeyEnv, "from-the-environment")
	key, err := providerKeys(Config{Driver: "boxd", APIKey: " secret "})(context.Background())
	if err != nil || key.Secret != "secret" || key.Driver != "boxd" {
		t.Fatalf("boxd's key = %v, %v, want the configuration's", key, err)
	}
	if _, err := providerKeys(Config{Driver: "boxd"})(context.Background()); err == nil || !strings.Contains(err.Error(), "apiKey") {
		t.Fatalf("a provider with no key = %v, want an error naming apiKey", err)
	}
}

// Where a pool's agent runs follows from what the driver is, not from its
// name: a remote driver's pool is a machine, a local one's is a host agent in
// the driver's own state directory.
func TestPoolHostFollowsTheDriversKind(t *testing.T) {
	remote, err := engine.Open(boxd.New(machine.Config{Project: "prov-test"}), "prov-test")
	if err != nil {
		t.Fatal(err)
	}
	if host, ok := newPoolHost(remote, Config{}).(*poolMachine); !ok {
		t.Fatalf("a remote driver's pool host is %T, want a pool machine", newPoolHost(remote, Config{}))
	} else if host.shell[0] != "/bin/bash" || host.logs(sandbox.PoolLogOptions{})[0] != "journalctl" {
		t.Fatalf("a pool machine's console %q and log %q are not the Linux machine's", host.shell, host.logs(sandbox.PoolLogOptions{}))
	}
	isolateStateRoot(t)
	local := openRuntime(t, fakeConfig(t))
	if host, ok := local.host.(*hostAgent); !ok {
		t.Fatalf("a local driver's pool host is %T, want a host agent", local.host)
	} else if host.root != stateRoot("fake") {
		t.Fatalf("host agent root = %q, want the driver's state root %q", host.root, stateRoot("fake"))
	}
}

// A remote driver's engine is the provider instance's own disco-vm project,
// spelled from its ID, so two servers or two providers on one account do not
// take each other's machines for their own. A local driver's is one for every
// provider, so the host's running-guest cap is counted over all of them.
func TestEngineProjectIsTheProvidersOnARemoteDriver(t *testing.T) {
	project, err := remoteProject("prov_0123456789abcdef")
	if err != nil || project != "prov-0123456789abcdef" {
		t.Fatalf("remoteProject(prov_…) = %q, %v, want prov-0123456789abcdef", project, err)
	}
	if _, _, err := openDriver("boxd", "Not A Project!", nil); err == nil {
		t.Fatal("openDriver accepted an ID outside disco-vm's project rule")
	}
	isolateStateRoot(t)
	remote := openRuntime(t, Config{Driver: "boxd"})
	if remote.engine.Project != "prov-test" {
		t.Fatalf("boxd engine project = %q, want the provider's", remote.engine.Project)
	}
	local := openRuntime(t, fakeConfig(t))
	if _, project, err := openDriver("fake", "Not A Project!", nil); err != nil || project != localProject || local.engine.Project != localProject {
		t.Fatalf("a local driver's project = %q, %v (engine %q), want %q for every provider", project, err, local.engine.Project, localProject)
	}
}

// Only a service that cannot be reached is waited on; a credential the driver
// is missing or refused is an error the pool shows.
func TestUnreachableIsATransportFailureOnly(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{fmt.Errorf("boxd: %w", context.DeadlineExceeded), true},
		{fmt.Errorf("boxd: api: %w", grpcstatus.Error(codes.Unavailable, "no route")), true},
		{fmt.Errorf("boxd: api: %w", grpcstatus.Error(codes.Unauthenticated, "bad key")), false},
		{errors.New("boxd: no credentials: set BOXD_API_KEY"), false},
	} {
		if got := unreachable(tc.err); got != tc.want {
			t.Errorf("unreachable(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// The provider the server's factory builds for a local driver refuses a
// console, as a host pool agent has no host to open one on.
func TestFactoryProviderOnALocalDriverRefusesAConsole(t *testing.T) {
	isolateStateRoot(t)
	built, err := FactoryWithPoolManager(nil)(context.Background(), fakeInstance(t))
	if err != nil {
		t.Fatalf("factory error = %v", err)
	}
	t.Cleanup(func() { _ = built.Close() })
	provider, ok := built.(sandbox.PoolRuntime)
	if !ok {
		t.Fatalf("factory built %T, which is no sandbox.PoolRuntime", built)
	}
	instance := fakeInstance(t)
	if _, err := provider.OpenConsole(context.Background(), instance, &model.Pool{ID: "pool-7"}, sandbox.ConsoleOptions{}); !errors.Is(err, sandbox.ErrPoolConsoleUnsupported) {
		t.Fatalf("OpenConsole() error = %v, want ErrPoolConsoleUnsupported", err)
	}
	if err := provider.RemovePool(context.Background(), nil, nil, instance, &model.Pool{ID: "pool-7"}); err != nil {
		t.Fatalf("RemovePool() of a pool with nothing staged = %v", err)
	}
}

// waitForOutput reads a console until it has printed want.
func waitForOutput(t *testing.T, r io.Reader, want string) {
	t.Helper()
	found := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(r)
		var seen strings.Builder
		for {
			b, err := reader.ReadByte()
			if err != nil {
				found <- errors.New("console ended before printing " + want + ": " + seen.String())
				return
			}
			seen.WriteByte(b)
			if strings.Contains(seen.String(), want) {
				found <- nil
				return
			}
		}
	}()
	select {
	case err := <-found:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("console did not print %q", want)
	}
}
