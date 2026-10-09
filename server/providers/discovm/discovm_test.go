package discovm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/adrg/xdg"
	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine/boxd"

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

func fakeInstance(t *testing.T) *model.SandboxProviderInstance {
	t.Helper()
	config, err := json.Marshal(Config{Driver: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	return &model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: ProviderType, Config: config}
}

// newFakeRuntime is a runtime on the fake driver that hosts its pools in
// machines, as a remote driver's are, and whose pool image is built from a
// checkout holding the fake driver's spec, through BuildGuestImage.
func newFakeRuntime(t *testing.T) *Runtime {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake guest's console and log are POSIX shell commands")
	}
	ctx := context.Background()
	isolateStateRoot(t)
	r, err := newRuntime(Config{Driver: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	r.host = fakePoolMachine(r.engine)
	checkout := t.TempDir()
	spec := filepath.Join(checkout, filepath.FromSlash(fakeImage))
	if err := os.MkdirAll(filepath.Dir(spec), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spec, []byte("from:\n  install: {os: "+runtime.GOOS+", media: latest}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	build, err := r.BuildGuestImage(ctx, nil, &model.Pool{ID: "pool-1"}, sandbox.GuestImageBuildOptions{SourceDir: checkout})
	if err != nil {
		t.Fatalf("BuildGuestImage() error = %v", err)
	}
	out, err := io.ReadAll(build)
	_ = build.Close()
	if err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	if build.Destination != r.engine.Images.Root {
		t.Fatalf("Destination = %q, want the engine's image store %q", build.Destination, r.engine.Images.Root)
	}
	if _, err := r.engine.Images.Resolve(poolImage); err != nil {
		t.Fatalf("the build tagged no %s image: %v", poolImage, err)
	}
	return r
}

// The issue's done-when, for a pool hosted in a machine: the runtime creates
// and removes the pool, opens its logs, and opens its console.
//
// The pool is created through the runtime, because the provider's
// ReconcilePool also hands the pool agent its known pools, and a discovm pool
// runs no agent yet. Logs, console and removal go through the pool provider the
// server's factory wraps a runtime in (poolruntime.New), around this one.
func TestFakePoolMachineLifecycle(t *testing.T) {
	r := newFakeRuntime(t)
	ctx := context.Background()
	instance := fakeInstance(t)
	provider := poolruntime.New(r, Definition(), nil)
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })

	begun := 0
	begin := func(context.Context) error { begun++; return nil }
	if err := r.EnsurePool(ctx, nil, nil, pool, nil, nil, begin); err != nil {
		t.Fatalf("EnsurePool() error = %v", err)
	}
	inst, err := r.engine.Get(poolMachineName(pool.ID))
	if err != nil {
		t.Fatalf("no machine named for the pool: %v", err)
	}
	if state := r.engine.State(ctx, inst); state != engine.Running {
		t.Fatalf("pool machine is %s, want running", state)
	}
	if begun != 1 || pool.State != model.PoolStateRegistering || pool.Ready || pool.Schedulable {
		t.Fatalf("after create: begun=%d state=%q ready=%v schedulable=%v, want 1 registering false false", begun, pool.State, pool.Ready, pool.Schedulable)
	}
	var recorded poolRuntimeState
	if err := json.Unmarshal(pool.RuntimeState, &recorded); err != nil || recorded.Instance != inst.ID {
		t.Fatalf("RuntimeState = %s (%v), want instance %s", pool.RuntimeState, err, inst.ID)
	}

	// A drift check of a running machine starts nothing and closes nothing.
	pool.SetState(model.PoolStateActive)
	if err := r.EnsurePool(ctx, nil, nil, pool, nil, nil, begin); err != nil {
		t.Fatalf("second EnsurePool() error = %v", err)
	}
	if begun != 1 || pool.State != model.PoolStateActive {
		t.Fatalf("a running machine was treated as new: begun=%d state=%q", begun, pool.State)
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
	if _, err := r.engine.Get(poolMachineName(pool.ID)); !errors.Is(err, engine.ErrNotFound) {
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

// Repair restarts the pool's machine in place, on the same disk.
func TestFakePoolMachineRepairKeepsTheMachine(t *testing.T) {
	r := newFakeRuntime(t)
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-2", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	if err := r.EnsurePool(ctx, nil, nil, pool, nil, nil, nil); err != nil {
		t.Fatalf("EnsurePool() error = %v", err)
	}
	before, err := r.engine.Get(poolMachineName(pool.ID))
	if err != nil {
		t.Fatal(err)
	}
	pool.SetState(model.PoolStateActive)
	if err := r.RepairPool(ctx, nil, nil, pool, nil, "test", nil, nil); err != nil {
		t.Fatalf("RepairPool() error = %v", err)
	}
	after, err := r.engine.Get(poolMachineName(pool.ID))
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID || r.engine.State(ctx, after) != engine.Running {
		t.Fatalf("repair replaced or stopped the machine: %s -> %s", before.ID, after.ID)
	}
	if pool.State != model.PoolStateRegistering {
		t.Fatalf("pool state = %q after repair, want registering", pool.State)
	}
}

// The console and the log are read from inside the machine, so a pool with no
// machine says so rather than hanging.
func TestFakePoolWithoutAMachineHasNoConsoleOrLog(t *testing.T) {
	isolateStateRoot(t)
	r, err := newRuntime(Config{Driver: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	r.host = fakePoolMachine(r.engine)
	pool := &model.Pool{ID: "pool-3"}
	if _, err := r.OpenConsole(context.Background(), nil, pool, sandbox.ConsoleOptions{}); !errors.Is(err, errNoPoolMachine) {
		t.Fatalf("OpenConsole() error = %v, want not found", err)
	}
	if _, err := r.OpenLogs(context.Background(), nil, pool, sandbox.PoolLogOptions{}); !errors.Is(err, errNoPoolMachine) {
		t.Fatalf("OpenLogs() error = %v, want not found", err)
	}
	if _, err := r.AcquirePoolAgentClient(context.Background(), pool); !errors.Is(err, errNoPoolAgent) {
		t.Fatalf("AcquirePoolAgentClient() error = %v, want no pool agent", err)
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
	if err := h.ensurePoolHost(ctx, pool, nil); !errors.Is(err, errHostAgentNotStaged) {
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

// Which images a driver has is the checkout's to say: one with no specs for it
// has nothing to build.
func TestBuildGuestImageWithNoImagesIsUnsupported(t *testing.T) {
	isolateStateRoot(t)
	r, err := newRuntime(Config{Driver: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.BuildGuestImage(context.Background(), nil, &model.Pool{ID: "pool-5"}, sandbox.GuestImageBuildOptions{SourceDir: t.TempDir()})
	if !errors.Is(err, sandbox.ErrGuestImageBuildUnsupported) {
		t.Fatalf("BuildGuestImage() error = %v, want ErrGuestImageBuildUnsupported", err)
	}
}

// A machine is cloned from its image, so a build cannot restart a pool onto
// the new one, and says so as its own refusal rather than as "no image".
func TestBuildGuestImageRefusesToRestartTheHost(t *testing.T) {
	isolateStateRoot(t)
	r, err := newRuntime(Config{Driver: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.BuildGuestImage(context.Background(), nil, &model.Pool{ID: "pool-6"}, sandbox.GuestImageBuildOptions{SourceDir: t.TempDir(), RestartHost: true})
	if !errors.Is(err, sandbox.ErrGuestImageRestartUnsupported) || errors.Is(err, sandbox.ErrGuestImageBuildUnsupported) {
		t.Fatalf("BuildGuestImage(RestartHost) error = %v, want ErrGuestImageRestartUnsupported alone", err)
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
	if err := Validate(json.RawMessage(`{"driver":"boxd"}`)); err != nil {
		t.Errorf("Validate(boxd) = %v", err)
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
	if provider.Definition().Name != "disco-vm" {
		t.Fatalf("Definition().Name = %q", provider.Definition().Name)
	}
	isolateStateRoot(t)
	r, err := newRuntime(Config{Driver: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	if want := shimCommand(exe, r.engine.Root, "fake"); strings.Join(r.engine.ShimCommand, " ") != strings.Join(want, " ") {
		t.Fatalf("ShimCommand = %q, want %q", r.engine.ShimCommand, want)
	}
}

// A driver builds the twins it has a spec for, parents first, each from its
// own context, so a twin FROM another finds it by the tag the chain gives it.
func TestBuildGuestImageBuildsTheTwinChain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake guest's build steps are POSIX shell commands")
	}
	isolateStateRoot(t)
	r, err := newRuntime(Config{Driver: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	checkout := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(checkout, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("base-image/fake.yaml", "from:\n  install: {os: "+runtime.GOOS+", media: latest}\n")
	write("sandbox-agent/fake.yaml", "from:\n  image: discobox/base\n")
	// A twin another driver has is not this driver's to build.
	write("harness/codex-cli/boxd.yaml", "from:\n  image: nothing/here\n")

	build, err := r.BuildGuestImage(context.Background(), nil, &model.Pool{ID: "pool-8"}, sandbox.GuestImageBuildOptions{SourceDir: checkout})
	if err != nil {
		t.Fatalf("BuildGuestImage() error = %v", err)
	}
	out, err := io.ReadAll(build)
	_ = build.Close()
	if err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	for _, tag := range []string{"discobox/base", "discobox/sandbox-agent"} {
		if _, err := r.engine.Images.Resolve(tag); err != nil {
			t.Errorf("the chain built no %s: %v", tag, err)
		}
	}
	if _, err := r.engine.Images.Resolve("discobox/codex"); err == nil {
		t.Error("built another driver's twin")
	}
}

// One chain at a time per engine root: a second waits, says so, and gives up
// when its caller does.
func TestLockChainSerializesBuildsPerRoot(t *testing.T) {
	root := t.TempDir()
	release, err := lockChain(context.Background(), root, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var said strings.Builder
	waiting, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := lockChain(waiting, root, &said); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second chain on a busy root = %v, want it to wait until its context ends", err)
	}
	if !strings.Contains(said.String(), "waiting for another image build") {
		t.Fatalf("the waiting chain said %q", said.String())
	}
	other, err := lockChain(context.Background(), t.TempDir(), io.Discard)
	if err != nil {
		t.Fatalf("another root was held by this one: %v", err)
	}
	other()
	release()
	again, err := lockChain(context.Background(), root, io.Discard)
	if err != nil {
		t.Fatalf("the root was not released: %v", err)
	}
	again()
}

// The server's chain is the one `build:boxd-images` builds: the same specs,
// contexts and tags, so a twin that builds by hand builds here too.
func TestTwinsMatchTheTaskfile(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "Taskfile.yml"))
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`--driver boxd build -t (\S+) -f (\S+) (\S+)'`)
	var fromTaskfile []string
	for _, m := range pattern.FindAllStringSubmatch(string(data), -1) {
		fromTaskfile = append(fromTaskfile, m[1]+" "+m[2]+" "+m[3])
	}
	if len(fromTaskfile) == 0 {
		t.Fatal("found no build:boxd-images commands in Taskfile.yml")
	}
	repo, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	have, err := driverTwins(repo, "boxd")
	if err != nil {
		t.Fatal(err)
	}
	var fromTwins []string
	for _, tw := range have {
		fromTwins = append(fromTwins, tw.Tag+" "+tw.Spec("boxd")+" "+tw.Context)
	}
	if strings.Join(fromTwins, "\n") != strings.Join(fromTaskfile, "\n") {
		t.Fatalf("twins builds\n%s\nbut build:boxd-images builds\n%s", strings.Join(fromTwins, "\n"), strings.Join(fromTaskfile, "\n"))
	}
}

// Where a pool's agent runs follows from what the driver reports, not from its
// name: a remote driver's pool is a machine, a local one's is a host agent.
func TestPoolHostFollowsTheDriversCapabilities(t *testing.T) {
	remote, err := engine.Open(t.TempDir(), boxd.New())
	if err != nil {
		t.Fatal(err)
	}
	if host, ok := newPoolHost(remote).(*poolMachine); !ok {
		t.Fatalf("a remote driver's pool host is %T, want a pool machine", newPoolHost(remote))
	} else if host.shell[0] != "/bin/bash" || host.logs(sandbox.PoolLogOptions{})[0] != "journalctl" {
		t.Fatalf("a pool machine's console %q and log %q are not the Linux machine's", host.shell, host.logs(sandbox.PoolLogOptions{}))
	}
	isolateStateRoot(t)
	local, err := newRuntime(Config{Driver: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := local.host.(*hostAgent); !ok {
		t.Fatalf("a local driver's pool host is %T, want a host agent", local.host)
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
