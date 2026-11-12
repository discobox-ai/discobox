package discovm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	cfg := fakeConfig(t)
	cfg.Base = base
	r := openRuntime(t, cfg)
	driver := &poolTestDriver{Driver: r.engine.Driver}
	e, err := engine.Open(driver, r.engine.Pool)
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
	provider := poolruntime.New(r, Definition(), nil, nil)
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
	if inst.From != "" {
		t.Fatalf("pool machine %+v, want one made fresh from the driver's base", inst)
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
	if text := readToEnd(t, console); !strings.Contains(text, "console-42") {
		t.Fatalf("console printed %q, want console-42", text)
	}
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

// Command argv runs through the login environment and keeps streams separate.
func TestPoolConsoleCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, _ := newFakePool(t, "fake")
	pool := &model.Pool{ID: "pool-command", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatal(err)
	}
	console, err := r.OpenConsole(ctx, nil, pool, sandbox.ConsoleOptions{Command: []string{"sh", "-c", `cat; printf '%s' "$1" >&2; exit 37`, "sh", "literal ; $HOME"}})
	if err != nil {
		t.Fatal(err)
	}
	defer console.Close()
	if _, err := io.WriteString(console, "input\n"); err != nil {
		t.Fatal(err)
	}
	if err := console.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	stderr := make(chan string, 1)
	go func() { b, _ := io.ReadAll(console.Stderr()); stderr <- string(b) }()
	out, err := io.ReadAll(console)
	if err != nil || string(out) != "input\n" {
		t.Fatalf("stdout=%q err=%v", out, err)
	}
	if got := <-stderr; got != "literal ; $HOME" {
		t.Fatalf("stderr=%q", got)
	}
	if code, err := console.Wait(ctx); err != nil || code != 37 {
		t.Fatalf("Wait=%d %v", code, err)
	}
}

// A pool machine is made fresh from the base the provider names, which is
// the driver's own configuration (disco-vm ADR 26-11-03-606), and its
// bootstrap names the same base for its sandboxes' machines.
func TestFakePoolMachineBootsTheConfiguredBase(t *testing.T) {
	ctx := context.Background()
	r, driver := newFakePool(t, "fake-other")
	pool := &model.Pool{ID: "pool-10", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatalf("EnsurePool() error = %v", err)
	}
	if _, err := r.host.(*poolMachine).instance(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if bases := fakeBasesMade(t); len(bases) != 1 || bases[0] != "fake-other" {
		t.Fatalf("machines made from bases %v, want the configured one", bases)
	}
	if driver.applied[0].MachineBase != "fake-other" {
		t.Fatalf("bootstrap's machine base = %q, want the configured %q", driver.applied[0].MachineBase, "fake-other")
	}
}

// fakeBasesMade is the base of every machine the fake driver made fresh in
// the test's state root, from the marker it leaves at a machine's root.
func fakeBasesMade(t *testing.T) []string {
	t.Helper()
	var bases []string
	root, err := os.OpenRoot(stateRoot("fake"))
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() != ".disco-vm-install" {
			return err
		}
		data, err := fs.ReadFile(root.FS(), path)
		if err != nil {
			return err
		}
		for line := range strings.Lines(string(data)) {
			if base, ok := strings.CutPrefix(strings.TrimSpace(line), "base="); ok {
				bases = append(bases, base)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return bases
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
	// The agent of a pool whose machine is gone is waited for, not refused:
	// the pool's reconcile creates the machine again (#893).
	if _, err := r.AcquirePoolAgentClient(context.Background(), pool); !errors.Is(err, errNoPoolMachine) || !errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Fatalf("AcquirePoolAgentClient() error = %v, want no machine and not reachable", err)
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

// boxd's cluster endpoints, which disco-vm once read from the server's
// environment itself, are still read from it for the driver, and nothing else
// is: not boxd's region, which is a provider's, nor any field of a driver that
// read none. A value the driver refuses fails validation naming the field and
// the variable it came from, not as a driver missing from the build.
func TestDriverConfigCarriesBoxdsEndpointsFromTheEnvironment(t *testing.T) {
	isolateStateRoot(t)
	t.Setenv(boxd.RegionEnv, "nyc")
	cfg, err := driverConfig("boxd", "prov-test", stateRoot("boxd"), nil)
	if err != nil || cfg.DriverConfig != nil {
		t.Fatalf("boxd's configuration with no endpoint set = %s, %v; want none", cfg.DriverConfig, err)
	}
	t.Setenv(boxd.AddrEnv, "boxd.example.com:9443")
	t.Setenv(boxd.TokenURLEnv, "https://app.example.com/api/v1/auth/token")
	cfg, err = driverConfig("boxd", "prov-test", stateRoot("boxd"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal(cfg.DriverConfig, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"addr": "boxd.example.com:9443", "tokenUrl": "https://app.example.com/api/v1/auth/token"} //nolint:gosec // G101: an endpoint URL, not a credential
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("boxd's configuration = %v, want %v", got, want)
	}
	if cfg, err := driverConfig("fake", localPool, stateRoot("fake"), nil); err != nil || cfg.DriverConfig != nil {
		t.Fatalf("the fake driver's configuration = %s, %v; want none", cfg.DriverConfig, err)
	}
	t.Setenv(boxd.TokenURLEnv, "http://app.example.com/token")
	if err := Validate(json.RawMessage(`{"driver":"boxd","apiKey":"k"}`)); err == nil || !strings.Contains(err.Error(), "tokenUrl") || !strings.Contains(err.Error(), boxd.TokenURLEnv) || strings.Contains(err.Error(), "not available") || strings.Contains(err.Error(), "app.example.com") {
		t.Fatalf("Validate with a plain-http token URL = %v, want an error naming tokenUrl and %s, and not its value", err, boxd.TokenURLEnv)
	}
}

// A remote driver needs its key, said rather than guessed, and may name the
// URL its pool machines reach the server at: without one the server opens a
// channel to each (ADR 26-10-30-875 §4). A local driver takes neither.
func TestValidateWhatADriverNeeds(t *testing.T) {
	for _, tc := range []struct{ config, want string }{
		{`{"driver":"boxd"}`, "apiKey"},
		{`{"driver":"boxd","apiKey":"k"}`, ""},
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

// A pool names the OS its driver boots, as disco-vm reports it: linux on
// boxd, which refuses darwin, and darwin on a driver that reports it, which is
// vz's report, without a vz in this build (ADR 26-11-11-963 §1).
func TestValidatePoolOSIsWhatTheDriverBoots(t *testing.T) {
	if err := ValidatePoolOS(json.RawMessage(`{"driver":"boxd"}`), "linux"); err != nil {
		t.Fatalf("a linux pool on boxd: %v", err)
	}
	if err := ValidatePoolOS(json.RawMessage(`{"driver":"boxd"}`), "darwin"); err == nil || !strings.Contains(err.Error(), "does not boot darwin") {
		t.Fatalf("a darwin pool on boxd = %v, want refused", err)
	}
	if err := ValidatePoolOS(json.RawMessage(`{"driver":"hyperv"}`), "linux"); err == nil {
		t.Fatal("a pool on a driver this build lacks was accepted")
	}
	vz := machine.Capabilities{GuestOS: []machine.OS{machine.Darwin, machine.Linux}}
	for _, os := range []string{"linux", "darwin"} {
		if err := bootsOS("vz", vz, os); err != nil {
			t.Fatalf("a %s pool on a driver that boots darwin and linux: %v", os, err)
		}
	}
}

// The factory the server registers builds a pool-backed provider over the
// runtime, whose shims are this binary.
func TestFactoryBuildsAPoolProvider(t *testing.T) {
	isolateStateRoot(t)
	provider, err := FactoryWithPoolManager(nil, nil)(context.Background(), fakeInstance(t))
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
	cfg, err := driverConfig("fake", localPool, stateRoot("fake"), nil)
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	if want := shimCommand(exe, stateRoot("fake"), "fake", localPool); strings.Join(cfg.ShimCommand, " ") != strings.Join(want, " ") {
		t.Fatalf("ShimCommand = %q, want %q", cfg.ShimCommand, want)
	}
	if cfg.Pool != localPool || cfg.Dir != stateRoot("fake") {
		t.Fatalf("driver config scope %q dir %q, want %q in the state root", cfg.Pool, cfg.Dir, localPool)
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
	driver, err := boxd.New(machine.Config{Pool: "prov-test"})
	if err != nil {
		t.Fatal(err)
	}
	remote, err := engine.Open(driver, "prov-test")
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

// A remote driver's engine is the provider instance's own disco-vm scope,
// spelled from its ID, so two servers or two providers on one account do not
// take each other's machines for their own. A local driver's is one for every
// provider, so the host's running-guest cap is counted over all of them.
func TestEngineProjectIsTheProvidersOnARemoteDriver(t *testing.T) {
	scope, err := remotePool("prov_0123456789abcdef")
	if err != nil || scope != "prov-0123456789abcdef" {
		t.Fatalf("remotePool(prov_…) = %q, %v, want prov-0123456789abcdef", scope, err)
	}
	if _, _, err := openDriver("boxd", "Not A Project!", "", nil); err == nil {
		t.Fatal("openDriver accepted an ID outside disco-vm's scope rule")
	}
	isolateStateRoot(t)
	remote := openRuntime(t, Config{Driver: "boxd"})
	if remote.engine.Pool != "prov-test" {
		t.Fatalf("boxd engine scope = %q, want the provider's", remote.engine.Pool)
	}
	local := openRuntime(t, fakeConfig(t))
	if _, scope, err := openDriver("fake", "Not A Project!", "", nil); err != nil || scope != localPool || local.engine.Pool != localPool {
		t.Fatalf("a local driver's scope = %q, %v (engine %q), want %q for every provider", scope, err, local.engine.Pool, localPool)
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
	built, err := FactoryWithPoolManager(nil, nil)(context.Background(), fakeInstance(t))
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

// readToEnd reads a console until its shell ends, as the server's console
// handler does before it asks for the exit code. A guest process hands its
// output over an unbuffered pipe and records its exit only once all of it is
// read, so a reader that stopped at the first match and then waited would
// hang whenever the shell's last output came in a later write.
func readToEnd(t *testing.T, r io.Reader) string {
	t.Helper()
	read := make(chan string, 1)
	go func() {
		text, _ := io.ReadAll(r)
		read <- string(text)
	}()
	select {
	case text := <-read:
		return text
	case <-time.After(30 * time.Second):
		t.Fatal("console did not end")
		return ""
	}
}

// A held driver is told by what disco-vm reports, never by its name (ADR
// 26-11-11-963 §1), and only its pool machine is fenced (§6).
func TestHeldIsWhatTheDriverReports(t *testing.T) {
	for name, tc := range map[string]struct {
		caps machine.Capabilities
		held bool
	}{
		"libkrun":  {machine.Capabilities{Ports: true, Keys: machine.KeyCapabilities{Mode: machine.KeysNone}}, true},
		"boxd":     {machine.Capabilities{Isolation: true, Keys: machine.KeyCapabilities{Mode: machine.KeysIssue}}, false},
		"fake":     {machine.Capabilities{Ports: true, Keys: machine.KeyCapabilities{Mode: machine.KeysIssue}}, false},
		"no ports": {machine.Capabilities{Keys: machine.KeyCapabilities{Mode: machine.KeysNone}}, false},
	} {
		if got := held(tc.caps); got != tc.held {
			t.Errorf("%s: held is %v, want %v", name, got, tc.held)
		}
	}
}
