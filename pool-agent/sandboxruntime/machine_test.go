package sandboxruntime

import (
	"archive/tar"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aidanwoods.dev/go-paseto"
	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pool-agent/buildkitagent"
	"github.com/discobox-ai/discobox/pool-agent/provisioner"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"
	"github.com/discobox-ai/discobox/pool-agent/vmtest"

	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
)

// machineTestPool is the disco-vm pool these tests' pools make their
// sandboxes' machines in.
const machineTestPool = "pool-machine-test"

// machineFixture is a machine pool's runtime over disco-vm's fake driver,
// whose engine calls with a key the driver issued, as a pool machine's does
// (ADR 26-10-23-551 §4), and the sandbox agents its machines run.
type machineFixture struct {
	runtime *MachineSandboxRuntime
	// driver is the pool's driver, which a test changes what it can do on.
	driver *agentDriver
	// owner is the driver as the account's owner, which a test reads the
	// machines through behind the pool's back.
	owner  machine.Driver
	agents *fakeMachineAgents
	key    ed25519.PrivateKey
}

// newMachineFixture is a machine runtime on disco-vm's fake driver, made
// with config (nil is its defaults).
func newMachineFixture(t *testing.T, config json.RawMessage) *machineFixture {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	owner := vmtest.FakeDriver(t, dir, machineTestPool, nil, config)
	key, err := owner.IssueKey(ctx, machine.IssueRequest{Name: "pool-1", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	agents := &fakeMachineAgents{servers: map[string]*fakeMachineAgent{}}
	driver := &agentDriver{Driver: vmtest.IngressOptional(vmtest.FakeDriver(t, dir, machineTestPool, machine.StaticKey(key), config)), agents: agents}
	e, err := engine.Open(driver, machineTestPool)
	if err != nil {
		t.Fatal(err)
	}
	_, identity, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	agents.poolKey = identity.Public().(ed25519.PublicKey)
	r, err := NewMachineSandboxRuntime(MachineSandboxRuntimeConfig{
		Audit:       &fakeAuditPurger{},
		ProjectID:   "proj_1",
		PoolID:      "pool_1",
		Root:        layout.ContainerAt(t.TempDir()),
		Engine:      e,
		IdentityKey: identity,
		Platform:    platform.Current(),
		AgentLayers: &fakeAgentLayers{digest: "sha256:" + strings.Repeat("c", 64), layer: gzippedAgentLayer(t, "agent-v1")},
		// Where a snapshot build reaches the pool, which its plan names.
		PoolEndpoints: proxyagent.PoolEndpoints(""),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		instances, _ := owner.Instances(context.Background())
		for _, inst := range instances {
			_ = (&engine.Engine{Driver: owner, Pool: machineTestPool}).Remove(context.Background(), inst, true)
		}
		agents.closeAll()
	})
	return &machineFixture{runtime: r, driver: driver, owner: owner, agents: agents, key: identity}
}

func machineCreate(sandboxID string) *workerapimodel.PoolSandboxCreateRequest {
	return &workerapimodel.PoolSandboxCreateRequest{
		SandboxId: sandboxID,
		Config: workerapimodel.SandboxConfig{
			SpecFingerprint: workerclient.NewOptString("spec-1"),
			Start:           workerclient.NewOptBool(true),
		},
		ResolvedHarnessConfig: workerclient.NewOptResolvedHarnessConfig(workerclient.ResolvedHarnessConfig{}),
		Agent:                 workerclient.NewOptBundlePin(testAgentPin),
		Bundles: workerclient.NewOptNilBundlePinArray([]workerapimodel.BundlePin{
			{Name: "discobox-platform", Digest: "sha256:" + strings.Repeat("a", 64)},
		}),
	}
}

// The issue's done-when: a Linux guest on the fake driver goes through
// create, provision, intake, attach, stop and start, clear-cache and delete,
// with the engine opened by an issued fake key.
func TestMachineSandboxLifecycle(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime

	// Create: a machine named by the sandbox's ID, booted from the driver's
	// base, provisioned and delivered its document.
	sb, err := r.CreateSandbox(ctx, machineCreate("sbx_1"))
	if err != nil {
		t.Fatal(err)
	}
	if sb.Status != StatusRunning {
		t.Fatalf("created sandbox is %s, want running", sb.Status)
	}
	inst, err := f.owner.Instance(ctx, "sbx-1")
	if err != nil {
		t.Fatalf("the sandbox's machine, by its name: %v", err)
	}
	if inst.From != "" {
		t.Fatalf("machine %+v, want one made fresh from the driver's base", inst)
	}
	if inst.Data == nil || inst.Cache == nil || inst.Cache.Key != "sbx-1" {
		t.Fatalf("machine volumes = %+v, %+v; want a data volume and a cache of its own", inst.Data, inst.Cache)
	}
	// The credentials broker is told the sandbox's variables from the pool's
	// own record of its file secrets, as for a container.
	if _, ok, err := r.readFileSecretsRecord("sbx_1"); err != nil || !ok {
		t.Fatalf("file secrets record = %v, %v; want it written at create", ok, err)
	}
	agent := f.agents.get(t, "sbx_1")
	if got := agent.revisions(); len(got) != 1 {
		t.Fatalf("runtime-config deliveries = %v, want one at boot", got)
	}

	// What was provisioned: the bootstrap, naming where the driver's service
	// listens, the agent from its layer, its units and its pins.
	guest, err := r.engine.Guest(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	var bootstrap sandboxconfig.Config
	if err := json.Unmarshal([]byte(readMachineFile(t, guest, "/etc/discobox/sandbox.json")), &bootstrap); err != nil {
		t.Fatal(err)
	}
	listen, err := f.driver.Listen(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if "tcp:"+bootstrap.AgentRuntime.ListenAddress != listen || bootstrap.SandboxID != "sbx_1" {
		t.Fatalf("bootstrap listens on %q for %q, want the service's %q for sbx_1", bootstrap.AgentRuntime.ListenAddress, bootstrap.SandboxID, listen)
	}
	if got := readMachineFile(t, guest, agentlayer.AgentPath); got != "agent-v1" {
		t.Fatalf("agent = %q, want the layer's", got)
	}
	if !strings.Contains(readMachineFile(t, guest, agentlayer.PinsFile), "discobox-platform") {
		t.Fatal("pins do not name the platform bundle")
	}
	readMachineFile(t, guest, "/etc/systemd/system/discobox-sandbox-agent.service")

	// Attach: the pool reaches the agent at the machine's URL.
	dial, err := r.SandboxDialer(ctx, "sbx_1", SandboxAgentPort)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sandboxAgentHealth(ctx, dial); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if _, err := r.SandboxDialer(ctx, "sbx_1", 8080); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("dial another port = %v, want it unsupported", err)
	}

	// Stop and start: the engine's power, and every start places again and
	// delivers again.
	if err := r.StopSandbox(ctx, "sbx_1", nil); err != nil {
		t.Fatal(err)
	}
	if sb, err := r.GetSandbox(ctx, "sbx_1"); err != nil || sb.Status != StatusStopped {
		t.Fatalf("stopped sandbox = %+v, %v; want stopped", sb, err)
	}
	if err := r.StartSandbox(ctx, "sbx_1", nil); err != nil {
		t.Fatal(err)
	}
	if sb, err := r.GetSandbox(ctx, "sbx_1"); err != nil || sb.Status != StatusRunning {
		t.Fatalf("started sandbox = %+v, %v; want running", sb, err)
	}
	if got := f.agents.get(t, "sbx_1").revisions(); len(got) != 1 {
		t.Fatalf("deliveries after the restart = %v, want one to the agent that booted", got)
	}

	// Clear-cache: through the sandbox's agent, stopping nothing.
	stopped, err := r.ClearCache(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stopped == nil || len(stopped) != 0 {
		t.Fatalf("clear-cache stopped %#v, want an empty list", stopped)
	}
	if got := f.agents.get(t, "sbx_1").cacheClears(); got != 1 {
		t.Fatalf("cache clears = %d, want 1", got)
	}

	// Delete: confirmed only once the machine is gone, and the images the
	// sandbox's builds left in the pool registry go with it.
	published := filepath.Join(buildkitagent.RegistryRoot(r.root, r.projectID, r.poolID), "docker/registry/v2/repositories/sbx_1/discobox-build/0123")
	if err := os.MkdirAll(published, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.DeleteSandbox(ctx, "sbx_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Instance(ctx, "sbx-1"); !errors.Is(err, machine.ErrNotFound) {
		t.Fatalf("machine after delete: %v, want it gone", err)
	}
	// And its audit trail in the pool proxy (ADR 26-11-14-896 §5).
	if purged := r.audit.(*fakeAuditPurger).purges(); !slices.Equal(purged, []string{"sbx_1"}) {
		t.Fatalf("audit trails purged = %v, want the sandbox's", purged)
	}
	if r.hostsSandbox("sbx_1") {
		t.Fatal("the sandbox's tree outlived its delete")
	}
	if _, err := os.Stat(published); !os.IsNotExist(err) {
		t.Fatalf("the sandbox's build result outlived its delete: %v", err)
	}
	if _, err := r.GetSandbox(ctx, "sbx_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted sandbox: %v, want not found", err)
	}
}

// A machine boots before anything is placed in it, so its first placement
// is applied in place: apply wires the running machine and starts the agent,
// and the machine is started once (ADR 26-11-23-458 §1). It is placed the
// wire unit's machine drop-in, which mounts at every boot after, and the
// unit apply wires in place with. A start after that places and starts the
// agent at the boot it has, applying with nothing to mount.
func TestAMachinesFirstPlacementIsAppliedInPlace(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_9")); err != nil {
		t.Fatal(err)
	}
	if got, want := f.driver.takeEvents("sbx-9"), []string{"boot", "apply in place"}; !slices.Equal(got, want) {
		t.Fatalf("create = %v, want %v: one start, provisioned while it runs", got, want)
	}
	// Made running by its create (engine.CreateOptions.Start), and not
	// started again: the boot above is the create's.
	if got := f.driver.createdStarted.Load(); got != 1 {
		t.Fatalf("creates asked to start = %d, want the sandbox's one", got)
	}
	inst, err := f.owner.Instance(ctx, "sbx-9")
	if err != nil {
		t.Fatal(err)
	}
	guest, err := r.engine.Guest(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if conf := readMachineFile(t, guest, "/etc/systemd/system/discobox-wire.service.d/discobox-machine.conf"); !strings.Contains(conf, "ConditionPathExists=/etc/discobox/sandbox.json") {
		t.Fatalf("the wire unit's machine drop-in:\n%s", conf)
	}
	readMachineFile(t, guest, "/etc/systemd/system/"+agentlayer.WireInPlaceUnit)

	if err := r.StopSandbox(ctx, "sbx_9", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.StartSandbox(ctx, "sbx_9", nil); err != nil {
		t.Fatal(err)
	}
	if got, want := f.driver.takeEvents("sbx-9"), []string{"boot", "apply"}; !slices.Equal(got, want) {
		t.Fatalf("start = %v, want %v", got, want)
	}
}

// A running machine whose placement moved -- here onto a newer agent layer
// -- is placed and applied in place, its agent restarted onto the new layer,
// and the machine is not restarted (ADR 26-11-23-458 §4). A start with
// nothing moved applies with nothing to restart.
func TestARunningMachineMovedOntoANewAgentIsNotRestarted(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_m")); err != nil {
		t.Fatal(err)
	}
	f.driver.takeEvents("sbx-m")
	newer := "sha256:" + strings.Repeat("d", 64)
	r.agentLayers = &fakeAgentLayers{digest: newer, layer: gzippedAgentLayer(t, "agent-v2")}
	if err := r.EnsureSandboxRunning(ctx, "sbx_m"); err != nil {
		t.Fatal(err)
	}
	if got, want := f.driver.takeEvents("sbx-m"), []string{"apply in place"}; !slices.Equal(got, want) {
		t.Fatalf("a request after the agent moved = %v, want %v: placed while it runs", got, want)
	}
	if got := f.agents.get(t, "sbx_m").layer; got != newer {
		t.Fatalf("the agent runs from %s, want %s: restarted onto what was placed", got, newer)
	}
	if placed, ok, err := r.readPlaced("sbx_m"); err != nil || !ok || placed.Layer != newer {
		t.Fatalf("placement record = %+v, %v, %v; want layer %s", placed, ok, err, newer)
	}
	if err := r.EnsureSandboxRunning(ctx, "sbx_m"); err != nil {
		t.Fatal(err)
	}
	if got := f.driver.takeEvents("sbx-m"); len(got) != 0 {
		t.Fatalf("a request with nothing moved = %v, want nothing", got)
	}
}

// An apply in place cut short fails the start, which stops the machine and
// records no placement; the next start boots it and applies in place again,
// which finishes it (ADR 26-11-23-458 §5): nothing needs a further restart.
func TestAnApplyInPlaceCutShortIsFinishedByTheNextStart(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	f.driver.inPlaceFailures.Store(1)
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_c")); err == nil || !strings.Contains(err.Error(), "cut short") {
		t.Fatalf("create = %v, want the apply's failure", err)
	}
	if got, want := f.driver.takeEvents("sbx-c"), []string{"boot", "apply in place"}; !slices.Equal(got, want) {
		t.Fatalf("create = %v, want %v", got, want)
	}
	if _, ok, err := r.readPlaced("sbx_c"); err != nil || ok {
		t.Fatalf("placement record = %v, %v; want none for an apply cut short", ok, err)
	}
	if sb, err := r.GetSandbox(ctx, "sbx_c"); err != nil || sb.Status != StatusStopped {
		t.Fatalf("sandbox after the failed start = %+v, %v; want it stopped", sb, err)
	}
	if err := r.StartSandbox(ctx, "sbx_c", nil); err != nil {
		t.Fatal(err)
	}
	if got, want := f.driver.takeEvents("sbx-c"), []string{"boot", "apply in place"}; !slices.Equal(got, want) {
		t.Fatalf("the next start = %v, want %v", got, want)
	}
	if _, ok, err := r.readPlaced("sbx_c"); err != nil || !ok {
		t.Fatalf("placement record = %v, %v; want it once the apply finished", ok, err)
	}
}

// An agent too old to wire a running machine in place is placed and the
// machine restarted onto it, as before (ADR 26-11-23-458 §6): its apply would
// mount the volumes under the services already running on those paths
// (ADR 26-11-02-023 §3).
func TestAMachinePlacedAnAgentThatCannotMountInPlaceIsRestartedOntoIt(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	f.driver.cannotMountInPlace = true
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_o")); err != nil {
		t.Fatal(err)
	}
	if got, want := f.driver.takeEvents("sbx-o"), []string{"boot", "boot"}; !slices.Equal(got, want) {
		t.Fatalf("create = %v, want %v: a restart before anything is applied", got, want)
	}
	if err := r.StopSandbox(ctx, "sbx_o", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.StartSandbox(ctx, "sbx_o", nil); err != nil {
		t.Fatal(err)
	}
	if got, want := f.driver.takeEvents("sbx-o"), []string{"boot", "apply"}; !slices.Equal(got, want) {
		t.Fatalf("start = %v, want %v", got, want)
	}
}

// A machine moved onto a new agent layer that cannot wire it in place is
// restarted onto it; one whose
// orderly shutdown times out is forced off, and the copy made before the
// restart may never have reached its disk. The older agent, still on that
// disk, boots and answers. The pool must not then record the new layer as
// placed: every later start would trust that record, skip copying the layer,
// and start no agent. It records nothing, or the layer the answering agent
// runs from, so its next request places the new layer again.
func TestAMachineForcedOffOntoANewAgentLayerIsNotRecordedAsPlacedOnIt(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	// Only a machine whose agent cannot wire it in place is restarted
	// onto a placement (ADR 26-11-23-458 §6).
	f.driver.cannotMountInPlace = true
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_6")); err != nil {
		t.Fatal(err)
	}
	if err := r.StopSandbox(ctx, "sbx_6", nil); err != nil {
		t.Fatal(err)
	}
	f.driver.takeEvents("sbx-6")
	older := r.agentLayers.(*fakeAgentLayers).digest
	newer := "sha256:" + strings.Repeat("d", 64)
	r.agentLayers = &fakeAgentLayers{digest: newer, layer: gzippedAgentLayer(t, "agent-v2")}

	// The forced stop loses the copy of the newer layer: its directory, and
	// the link that would have the machine boot from it.
	f.driver.forcedOff = func(ctx context.Context, guest machine.Machine) error {
		script := `root="$DISCO_VM_GUEST_ROOT"; rm -rf "$root` + agentlayer.LayerDir(newer) + `" "$root` + agentlayer.LayerComplete(newer) + `" && ln -sfn bundles/layers/` + agentlayer.Hex(older) + ` "$root` + agentlayer.CurrentLink + `"`
		process, err := guest.Exec(ctx, machine.ExecRequest{Argv: []string{"sh", "-c", script}})
		if err != nil {
			return err
		}
		if code, err := process.Wait(io.Discard, io.Discard); err != nil || code != 0 {
			return fmt.Errorf("lose the newer layer: exit %d: %w", code, err)
		}
		return nil
	}
	// The start succeeds: the older agent answers.
	if err := r.StartSandbox(ctx, "sbx_6", nil); err != nil {
		t.Fatal(err)
	}
	if got, want := f.driver.takeEvents("sbx-6"), []string{"boot", "boot"}; !slices.Equal(got, want) {
		t.Fatalf("start onto the newer layer = %v, want %v: a restart onto it", got, want)
	}

	placed, ok, err := r.readPlaced("sbx_6")
	if err != nil {
		t.Fatal(err)
	}
	if ok && placed.Layer == newer {
		t.Fatalf("placement record says layer %s, which the forced-off machine lost; the older agent answered from %s", newer, older)
	}

	// Its placement is not current, so the next request places the newer
	// layer again, restarts onto it in order, and records it once its agent
	// runs from it.
	if err := r.EnsureSandboxRunning(ctx, "sbx_6"); err != nil {
		t.Fatal(err)
	}
	if got, want := f.driver.takeEvents("sbx-6"), []string{"boot"}; !slices.Equal(got, want) {
		t.Fatalf("next request = %v, want %v: the newer layer placed again and restarted onto", got, want)
	}
	if got := f.agents.get(t, "sbx_6").layer; got != newer {
		t.Fatalf("the agent after the next start runs from %s, want %s", got, newer)
	}
	if placed, ok, err := r.readPlaced("sbx_6"); err != nil || !ok || placed.Layer != newer {
		t.Fatalf("placement record after the next start = %+v, %v, %v; want layer %s", placed, ok, err, newer)
	}
}

// On a first placement of an agent that cannot wire the machine in place,
// restarted onto it, an agent answering after a forced restart proves the
// layer, since a lost copy would leave none to answer: an agent too old to
// name its layer is recorded there all the same, so its machine is not
// restarted at every request.
func TestAForcedFirstPlacementIsRecordedThoughItsAgentNamesNoLayer(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	f.driver.cannotMountInPlace = true
	f.driver.shutdownRefused.Store(true)
	f.agents.unnamed = true
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_8")); err != nil {
		t.Fatal(err)
	}
	if got, want := f.driver.takeEvents("sbx-8"), []string{"boot", "boot"}; !slices.Equal(got, want) {
		t.Fatalf("create = %v, want %v", got, want)
	}
	if placed, ok, err := r.readPlaced("sbx_8"); err != nil || !ok || placed.Layer != r.agentLayers.(*fakeAgentLayers).digest {
		t.Fatalf("placement record = %+v, %v, %v; want the layer placed", placed, ok, err)
	}
	if err := r.EnsureSandboxRunning(ctx, "sbx_8"); err != nil {
		t.Fatal(err)
	}
	if got := f.driver.takeEvents("sbx-8"); len(got) != 0 {
		t.Fatalf("a request after the forced first placement = %v, want nothing", got)
	}
}

// A create whose answer was lost made the machine already: the next one finds
// it by name rather than making a second.
func TestMachineCreateFindsALostCreatesMachineByName(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	e := &engine.Engine{Driver: f.owner, Pool: machineTestPool}
	lost, err := e.Create(ctx, engine.CreateOptions{Name: "sbx-2"})
	if err != nil {
		t.Fatal(err)
	}
	sb, err := f.runtime.CreateSandbox(ctx, machineCreate("sbx_2"))
	if err != nil {
		t.Fatal(err)
	}
	if sb.ID != lost.ID {
		t.Fatalf("sandbox runs on machine %s, want the lost create's %s", sb.ID, lost.ID)
	}
	all, err := f.owner.Instances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("machines = %d, want the one", len(all))
	}
}

// A spec that moves under a running machine makes it again in place: the
// same machine, its disk kept, a new bootstrap, and a restart onto it.
func TestMachineSpecChangeRebuildsInPlace(t *testing.T) {
	f := newMachineFixture(t, nil)
	f.driver.noVolumes = true
	ctx := context.Background()
	r := f.runtime
	first, err := r.CreateSandbox(ctx, machineCreate("sbx_3"))
	if err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-3")
	if err != nil {
		t.Fatal(err)
	}
	guest, err := r.engine.Guest(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if err := guest.CopyTo(ctx, "/home", fileTree(t, "kept.txt", "the sandbox's own")); err != nil {
		t.Fatal(err)
	}

	req := machineCreate("sbx_3")
	req.Config.SpecFingerprint = workerclient.NewOptString("spec-2")
	req.Config.Description = workerclient.NewOptString("moved")
	second, err := r.CreateSandbox(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Status != StatusRunning {
		t.Fatalf("rebuilt sandbox = %+v, want the same machine running", second)
	}
	guest, err = r.engine.Guest(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if got := readMachineFile(t, guest, "/home/kept.txt"); got != "the sandbox's own" {
		t.Fatalf("kept file = %q, want the disk kept", got)
	}
	var bootstrap sandboxconfig.Config
	if err := json.Unmarshal([]byte(readMachineFile(t, guest, "/etc/discobox/sandbox.json")), &bootstrap); err != nil {
		t.Fatal(err)
	}
	if bootstrap.Description != "moved" {
		t.Fatalf("bootstrap description = %q, want the new spec's", bootstrap.Description)
	}
}

// writeMachineFiles writes a file to the sandbox's data volume, its cache
// volume, and its root outside every declared volume and source, which the
// data contract does not keep (pool-agent/DESIGN.md).
func writeMachineFiles(t *testing.T, r *MachineSandboxRuntime, inst machine.Instance) {
	t.Helper()
	ctx := context.Background()
	guest, err := r.engine.Guest(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	for dir, content := range map[string]string{"/.discobox/data": "the sandbox's data", "/.discobox/cache": "a warm cache", "/opt": "the old root's"} {
		if err := guest.CopyTo(ctx, dir, fileTree(t, "kept.txt", content)); err != nil {
			t.Fatal(err)
		}
	}
}

// machineHasFile reports whether the running machine has path.
func machineHasFile(t *testing.T, r *MachineSandboxRuntime, inst machine.Instance, path string) bool {
	t.Helper()
	guest, err := r.engine.Guest(context.Background(), inst)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := guest.CopyFrom(context.Background(), path)
	if err != nil {
		return false
	}
	defer rc.Close()
	_, err = tar.NewReader(rc).Next()
	return err == nil
}

// assertNewRootKeptVolumes checks the running machine was given a new root
// with its volumes kept, and placed again on it.
func assertNewRootKeptVolumes(t *testing.T, r *MachineSandboxRuntime, inst machine.Instance) {
	t.Helper()
	guest, err := r.engine.Guest(context.Background(), inst)
	if err != nil {
		t.Fatal(err)
	}
	if got := readMachineFile(t, guest, "/.discobox/data/kept.txt"); got != "the sandbox's data" {
		t.Fatalf("data volume file = %q, want it kept", got)
	}
	if got := readMachineFile(t, guest, "/.discobox/cache/kept.txt"); got != "a warm cache" {
		t.Fatalf("cache volume file = %q, want it kept", got)
	}
	if machineHasFile(t, r, inst, "/opt/kept.txt") {
		t.Fatal("the old root's file is in the machine, want a new root")
	}
	if got := readMachineFile(t, guest, agentlayer.AgentPath); got != "agent-v1" {
		t.Fatalf("agent on the new root = %q, want the layer placed again", got)
	}
	if record, _, err := r.readMachineRecord(strings.ReplaceAll(inst.Name, "-", "_")); err != nil || record.NewRoot || !record.DataOnVolume {
		t.Fatalf("machine record = %+v, %v; want no upgrade left to do, and its data on its volume", record, err)
	}
}

// A spec that moves under a running machine that keeps its data on a volume
// gives it a new root from the driver's base (disco-vm ADR 26-11-03-782):
// the same machine, its data and cache volumes kept, what was on its old root
// gone, and everything placed again on the new one.
func TestMachineSpecChangeGivesANewRootAndKeepsItsVolumes(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_7")); err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-7")
	if err != nil {
		t.Fatal(err)
	}
	writeMachineFiles(t, r, inst)

	req := machineCreate("sbx_7")
	req.Config.SpecFingerprint = workerclient.NewOptString("spec-2")
	req.Config.Description = workerclient.NewOptString("moved")
	sb, err := r.CreateSandbox(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Status != StatusRunning || f.driver.upgrades != 1 {
		t.Fatalf("rebuilt sandbox = %+v after %d upgrades, want it running on one", sb, f.driver.upgrades)
	}
	if inst, err = f.owner.Instance(ctx, "sbx-7"); err != nil {
		t.Fatal(err)
	}
	assertNewRootKeptVolumes(t, r, inst)
	guest, err := r.engine.Guest(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	var bootstrap sandboxconfig.Config
	if err := json.Unmarshal([]byte(readMachineFile(t, guest, "/etc/discobox/sandbox.json")), &bootstrap); err != nil {
		t.Fatal(err)
	}
	if bootstrap.Description != "moved" {
		t.Fatalf("bootstrap description = %q, want the new spec's", bootstrap.Description)
	}
	if _, err := sandboxAgentHealth(ctx, mustDialer(t, r, "sbx_7")); err != nil {
		t.Fatalf("the agent on the new root: %v", err)
	}

	// The same spec again is no rebuild.
	if _, err := r.CreateSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	if f.driver.upgrades != 1 {
		t.Fatalf("upgrades = %d after a create of the same spec, want still one", f.driver.upgrades)
	}
}

// A stopped machine whose spec moves stays stopped, and is given its new
// root at its next start.
func TestMachineStoppedSpecChangeGivesANewRootAtItsStart(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_8")); err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-8")
	if err != nil {
		t.Fatal(err)
	}
	writeMachineFiles(t, r, inst)
	if err := r.StopSandbox(ctx, "sbx_8", nil); err != nil {
		t.Fatal(err)
	}
	req := machineCreate("sbx_8")
	req.Config.SpecFingerprint = workerclient.NewOptString("spec-2")
	req.Config.Start = workerclient.NewOptBool(false)
	sb, err := r.CreateSandbox(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Status != StatusStopped || f.driver.upgrades != 0 {
		t.Fatalf("sandbox = %+v after %d upgrades, want it stopped on none", sb, f.driver.upgrades)
	}
	if err := r.StartSandbox(ctx, "sbx_8", nil); err != nil {
		t.Fatal(err)
	}
	if f.driver.upgrades != 1 {
		t.Fatalf("upgrades = %d after the start, want one", f.driver.upgrades)
	}
	assertNewRootKeptVolumes(t, r, inst)
}

// A create not asked to start makes its machine stopped, as it was, and the
// start that follows boots it once and places it while it runs.
func TestAMachineCreateNotAskedToStartMakesItStopped(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	req := machineCreate("sbx_s")
	req.Config.Start = workerclient.NewOptBool(false)
	sb, err := r.CreateSandbox(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if sb.Status != StatusCreated || f.driver.createdStarted.Load() != 0 {
		t.Fatalf("sandbox = %+v after %d creates asked to start, want it made and none", sb, f.driver.createdStarted.Load())
	}
	if got := f.driver.takeEvents("sbx-s"); len(got) != 0 {
		t.Fatalf("create = %v, want nothing booted", got)
	}
	if err := r.StartSandbox(ctx, "sbx_s", nil); err != nil {
		t.Fatal(err)
	}
	if got, want := f.driver.takeEvents("sbx-s"), []string{"boot", "apply in place"}; !slices.Equal(got, want) {
		t.Fatalf("start = %v, want %v", got, want)
	}
}

// A create that made its machine running and fails before its start has
// begun its own boot stops it, and leaves no boot mark: a machine running with nothing
// placed would answer nothing until it was deleted. The next create starts
// it and places it.
func TestAMachineMadeRunningWhoseCreateFailsIsStopped(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	// The engine's started create reads it to tell the guest; the pool's
	// read after it, for the bootstrap, fails.
	f.driver.listenRefused.Store(true)
	f.driver.listenAllowed.Store(1)
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_f")); err == nil {
		t.Fatal("a create whose machine's service address cannot be read succeeded")
	}
	if got := f.driver.createdStarted.Load(); got != 1 {
		t.Fatalf("creates asked to start = %d, want one", got)
	}
	inst, err := f.owner.Instance(ctx, "sbx-f")
	if err != nil {
		t.Fatal(err)
	}
	if state := r.engine.State(ctx, inst); state != engine.Stopped {
		t.Fatalf("the machine is %s after its create failed, want stopped", state)
	}
	if r.SandboxBooting("sbx_f") {
		t.Fatal("the failed create left the sandbox marked booting")
	}
	f.driver.takeEvents("sbx-f")
	f.driver.listenRefused.Store(false)
	sb, err := r.CreateSandbox(ctx, machineCreate("sbx_f"))
	if err != nil {
		t.Fatal(err)
	}
	if sb.Status != StatusRunning {
		t.Fatalf("sandbox = %+v after the create again, want running", sb)
	}
	if got, want := f.driver.takeEvents("sbx-f"), []string{"boot", "apply in place"}; !slices.Equal(got, want) {
		t.Fatalf("create again = %v, want %v", got, want)
	}
}

// A create whose caller goes away while its new machine's agent comes up
// leaves the machine booting, as a start's does ("Running is not ready"):
// the start has taken it over, and its boot goes on detached. The machine is
// not stopped under it.
func TestAMachineMadeRunningWhoseCallerGoesMidBootKeepsBooting(t *testing.T) {
	f := newMachineFixture(t, nil)
	r := f.runtime
	f.agents.healthzHeld, f.agents.healthzAsked = make(chan struct{}), make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := r.CreateSandbox(ctx, machineCreate("sbx_g"))
		done <- err
	}()
	select {
	case <-f.agents.healthzAsked:
	case err := <-done:
		t.Fatalf("the create returned %v before its agent was asked whether it is up", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the agent was never asked whether it is up")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("the create whose caller went = %v, want canceled", err)
	}
	inst, err := f.owner.Instance(context.Background(), "sbx-g")
	if err != nil {
		t.Fatal(err)
	}
	if state := r.engine.State(context.Background(), inst); state != engine.Running {
		t.Fatalf("the machine is %s once its caller went, want running, still booting", state)
	}
	if !r.SandboxBooting("sbx_g") {
		t.Fatal("the sandbox is not marked booting while its agent comes up")
	}
	close(f.agents.healthzHeld)
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer waitCancel()
	if err := r.AwaitSandboxBoot(waitCtx, "sbx_g"); err != nil {
		t.Fatalf("the boot the caller left = %v, want it finished", err)
	}
	if state := r.engine.State(context.Background(), inst); state != engine.Running {
		t.Fatalf("the machine is %s after its boot, want running", state)
	}
}

// An upgrade cut short fails its start, and the next start finishes it: the
// pool's record asks for it until the driver has given the root.
func TestMachineUpgradeCutShortIsFinishedByTheNextStart(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_9")); err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-9")
	if err != nil {
		t.Fatal(err)
	}
	writeMachineFiles(t, r, inst)
	f.driver.upgradeFailures = 1
	req := machineCreate("sbx_9")
	req.Config.SpecFingerprint = workerclient.NewOptString("spec-2")
	if _, err := r.CreateSandbox(ctx, req); err == nil || !strings.Contains(err.Error(), "cut short") {
		t.Fatalf("create on an upgrade cut short = %v, want it failed", err)
	}
	if record, _, err := r.readMachineRecord("sbx_9"); err != nil || !record.NewRoot {
		t.Fatalf("machine record = %+v, %v; want the upgrade still asked for", record, err)
	}
	if sb, err := r.GetSandbox(ctx, "sbx_9"); err != nil || sb.Status != StatusStopped {
		t.Fatalf("sandbox after the failed upgrade = %+v, %v; want it stopped", sb, err)
	}
	if err := r.StartSandbox(ctx, "sbx_9", nil); err != nil {
		t.Fatal(err)
	}
	if f.driver.upgrades != 2 {
		t.Fatalf("upgrades = %d, want the one cut short and the one that finished it", f.driver.upgrades)
	}
	assertNewRootKeptVolumes(t, r, inst)
}

// A machine made before its driver attached a data volume keeps its data on
// its root, so it is made again in place, never given a new root.
func TestMachineWithoutADataVolumeIsRebuiltInPlace(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	e := &engine.Engine{Driver: f.owner, Pool: machineTestPool}
	if _, err := e.Create(ctx, engine.CreateOptions{Name: "sbx-10"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_10")); err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-10")
	if err != nil {
		t.Fatal(err)
	}
	guest, err := r.engine.Guest(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if err := guest.CopyTo(ctx, "/home", fileTree(t, "kept.txt", "the sandbox's own")); err != nil {
		t.Fatal(err)
	}
	req := machineCreate("sbx_10")
	req.Config.SpecFingerprint = workerclient.NewOptString("spec-2")
	if _, err := r.CreateSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	if f.driver.upgrades != 0 {
		t.Fatalf("upgrades = %d, want none of a machine with no data volume", f.driver.upgrades)
	}
	if guest, err = r.engine.Guest(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if got := readMachineFile(t, guest, "/home/kept.txt"); got != "the sandbox's own" {
		t.Fatalf("kept file = %q, want the root kept", got)
	}
}

// A machine whose agent keeps the sandbox's data on its root, as one pinned
// to an agent that predates wiring a data volume does, is made again in
// place, though it has a data volume: the pool gives a new root only to a
// machine whose volume says its data is there.
func TestMachineWhoseAgentKeepsItsDataOnItsRootIsRebuiltInPlace(t *testing.T) {
	f := newMachineFixture(t, nil)
	f.driver.rootData = true
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_11")); err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-11")
	if err != nil {
		t.Fatal(err)
	}
	if record, _, err := r.readMachineRecord("sbx_11"); err != nil || record.DataOnVolume {
		t.Fatalf("machine record = %+v, %v; want no data on its volume recorded", record, err)
	}
	writeMachineFiles(t, r, inst)
	req := machineCreate("sbx_11")
	req.Config.SpecFingerprint = workerclient.NewOptString("spec-2")
	if _, err := r.CreateSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	if f.driver.upgrades != 0 {
		t.Fatalf("upgrades = %d, want none of a machine whose data is on its root", f.driver.upgrades)
	}
	if !machineHasFile(t, r, inst, "/opt/kept.txt") {
		t.Fatal("the root's file is gone, want the root kept")
	}
}

// A machine whose agent wired its data volume once, then is placed an agent
// that does not (a pin moved back), is rebuilt in place: the volume's line
// names a start that is over, and the pool reads it again at every start.
func TestMachineAgentThatStopsWiringIsRebuiltInPlace(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_12")); err != nil {
		t.Fatal(err)
	}
	if record, _, err := r.readMachineRecord("sbx_12"); err != nil || !record.DataOnVolume {
		t.Fatalf("machine record = %+v, %v; want its data on its volume", record, err)
	}
	f.driver.rootData = true
	if err := r.RestartSandbox(ctx, "sbx_12", nil); err != nil {
		t.Fatal(err)
	}
	if record, _, err := r.readMachineRecord("sbx_12"); err != nil || record.DataOnVolume {
		t.Fatalf("machine record = %+v, %v; want its data on its volume no longer recorded", record, err)
	}
	req := machineCreate("sbx_12")
	req.Config.SpecFingerprint = workerclient.NewOptString("spec-2")
	if _, err := r.CreateSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	if f.driver.upgrades != 0 {
		t.Fatalf("upgrades = %d, want none once the agent stopped wiring", f.driver.upgrades)
	}
}

// #1156: a machine whose agent wired its data volume but left the working
// root on the machine's root, as an agent from before ADR 26-11-14-302 did,
// is rebuilt on the root it has: a new root would take the work of a box with
// no source with it. The boot on the new agent moves the working root onto the
// volume and says so, and only then is a rebuild given a new root (§4).
func TestMachineWhoseWorkingRootIsOnItsRootIsRebuiltInPlace(t *testing.T) {
	f := newMachineFixture(t, nil)
	f.driver.workingRootOnRoot = true
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_13")); err != nil {
		t.Fatal(err)
	}
	if record, _, err := r.readMachineRecord("sbx_13"); err != nil || record.DataOnVolume {
		t.Fatalf("machine record = %+v, %v; want its data not all on its volume", record, err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-13")
	if err != nil {
		t.Fatal(err)
	}
	writeMachineFiles(t, r, inst)
	// The rebuild runs the agent that keeps the working root on the volume.
	f.driver.workingRootOnRoot = false
	req := machineCreate("sbx_13")
	req.Config.SpecFingerprint = workerclient.NewOptString("spec-2")
	if _, err := r.CreateSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	if f.driver.upgrades != 0 {
		t.Fatalf("upgrades = %d, want none while the working root was on the root", f.driver.upgrades)
	}
	if !machineHasFile(t, r, inst, "/opt/kept.txt") {
		t.Fatal("the root's file is gone, want the root kept")
	}
	if record, _, err := r.readMachineRecord("sbx_13"); err != nil || !record.DataOnVolume {
		t.Fatalf("machine record = %+v, %v; want its data on its volume once its agent moved the working root there", record, err)
	}
	req.Config.SpecFingerprint = workerclient.NewOptString("spec-3")
	if _, err := r.CreateSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	if f.driver.upgrades != 1 {
		t.Fatalf("upgrades = %d, want the next rebuild given a new root", f.driver.upgrades)
	}
}

// A machine.json an older pool wrote says "dataOnVolume" from a line that did
// not ask about the working root. It is not taken for the answer this pool
// gives: a rebuild before the machine's next start on this pool keeps the
// root, and the box's working root on it (ADR 26-11-14-302 §4).
func TestMachineRecordFromAnOlderPoolIsRebuiltInPlace(t *testing.T) {
	f := newMachineFixture(t, nil)
	f.driver.workingRootOnRoot = true
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_14")); err != nil {
		t.Fatal(err)
	}
	path := r.machineRecordPath("sbx_14")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	// As the older pool left it, from the two-field line.
	record["dataOnVolume"] = true
	raw, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if saved, _, err := r.readMachineRecord("sbx_14"); err != nil || saved.DataOnVolume {
		t.Fatalf("machine record = %+v, %v; want an older pool's dataOnVolume not read as this one", saved, err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-14")
	if err != nil {
		t.Fatal(err)
	}
	writeMachineFiles(t, r, inst)
	// The rebuild runs the agent that keeps the working root on the volume,
	// which only its own start can say.
	f.driver.workingRootOnRoot = false
	req := machineCreate("sbx_14")
	req.Config.SpecFingerprint = workerclient.NewOptString("spec-2")
	if _, err := r.CreateSandbox(ctx, req); err != nil {
		t.Fatal(err)
	}
	if f.driver.upgrades != 0 {
		t.Fatalf("upgrades = %d, want none from an older pool's record", f.driver.upgrades)
	}
	if !machineHasFile(t, r, inst, "/opt/kept.txt") {
		t.Fatal("the root's file is gone, want the root kept")
	}
}

func mustDialer(t *testing.T, r *MachineSandboxRuntime, sandboxID string) *Dialer {
	t.Helper()
	dial, err := r.SandboxDialer(context.Background(), sandboxID, SandboxAgentPort)
	if err != nil {
		t.Fatal(err)
	}
	return dial
}

// Archiving keeps the machine, stopped, and its unarchiving create starts the
// same one again.
func TestMachineArchiveKeepsTheMachine(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	first, err := r.CreateSandbox(ctx, machineCreate("sbx_4"))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ArchiveSandbox(ctx, "sbx_4"); err != nil {
		t.Fatal(err)
	}
	if err := r.StartSandbox(ctx, "sbx_4", nil); !errors.Is(err, ErrArchived) {
		t.Fatalf("start an archived sandbox = %v, want ErrArchived", err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-4")
	if err != nil {
		t.Fatalf("archived sandbox's machine: %v, want it kept", err)
	}
	if state := r.engine.State(ctx, inst); state != engine.Stopped {
		t.Fatalf("archived machine is %s, want stopped", state)
	}
	sb, err := r.CreateSandbox(ctx, machineCreate("sbx_4"))
	if err != nil {
		t.Fatal(err)
	}
	if sb.ID != first.ID || sb.Status != StatusRunning {
		t.Fatalf("unarchived sandbox = %+v, want machine %s running again", sb, first.ID)
	}
}

// A pool machine made again has none of the trees the one before it kept, but
// the sandboxes' machines are still there under their names: archiving one
// stops it, rather than reporting it not found and leaving it running. Only a
// sandbox with neither a tree nor a machine is not found.
func TestMachineArchiveWithoutItsTreeStopsTheMachine(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_6")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(r.sandboxRoot("sbx_6")); err != nil {
		t.Fatal(err)
	}
	if err := r.ArchiveSandbox(ctx, "sbx_6"); err != nil {
		t.Fatalf("archive a sandbox whose tree was lost = %v, want its machine stopped", err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-6")
	if err != nil {
		t.Fatalf("archived sandbox's machine: %v, want it kept", err)
	}
	if state := r.engine.State(ctx, inst); state != engine.Stopped {
		t.Fatalf("archived machine is %s, want stopped", state)
	}
	if !r.isArchived("sbx_6") {
		t.Fatal("the sandbox is not marked archived")
	}
	if err := r.ArchiveSandbox(ctx, "sbx_none"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("archive a sandbox with no tree and no machine = %v, want ErrNotFound", err)
	}
}

// A sandbox's machine whose base carries no systemd is refused before its
// agent or bootstrap is placed, naming what the base lacks (ADR 26-11-16-619
// §4).
func TestMachineSandboxWhoseBaseHasNoSystemd(t *testing.T) {
	f := newMachineFixture(t, nil)
	f.driver.noSystemd = true
	_, err := f.runtime.CreateSandbox(context.Background(), machineCreate("sbx_1"))
	if !errors.Is(err, provisioner.ErrNoSystemd) || !strings.Contains(err.Error(), provisioner.SystemdPath) {
		t.Fatalf("a base without systemd = %v, want it refused naming %s", err, provisioner.SystemdPath)
	}
	if events := f.driver.events["sbx-1"]; slices.Contains(events, "apply") {
		t.Fatalf("a machine without systemd was applied: %v", events)
	}
}

// A machine on a driver that cannot freeze one is refused a suspend (ADR
// 26-11-21-395 §3), and a machine's tree is its disk, so an export is refused.
func TestMachineRefusesWhatItCannotDo(t *testing.T) {
	f := newMachineFixture(t, json.RawMessage(`{"noPause":true}`))
	ctx := context.Background()
	if _, err := f.runtime.CreateSandbox(ctx, machineCreate("sbx_5")); err != nil {
		t.Fatal(err)
	}
	if err := f.runtime.SuspendSandbox(ctx, "sbx_5", nil); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("suspend = %v, want ErrUnsupported", err)
	}
	if _, err := f.runtime.ExportTree(ctx, "sbx_5", delivery(machineCreate("sbx_5"))); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("export = %v, want ErrUnsupported", err)
	}
	if sb, err := f.runtime.GetSandbox(ctx, "sbx_5"); err != nil || sb.Status != StatusRunning {
		t.Fatalf("sandbox after the refusals = %+v, %v; want it running as it was", sb, err)
	}
}

// A driver that cannot isolate a machine is refused for a pool that must
// (ADR 26-10-23-551 §4).
func TestMachineRuntimeRefusesADriverThatCannotIsolate(t *testing.T) {
	e, err := engine.Open(vmtest.FakeDriver(t, t.TempDir(), machineTestPool, nil, nil), machineTestPool)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewMachineSandboxRuntime(MachineSandboxRuntimeConfig{Audit: &fakeAuditPurger{}, Engine: e, HoldToPool: true, Network: "pool-net", Root: layout.ContainerAt(t.TempDir())})
	if err == nil || !strings.Contains(err.Error(), "cannot carry a machine pool") {
		t.Fatalf("runtime on the fake driver, isolated = %v, want refused", err)
	}
}

// edgeDriver is the fencing fake as a driver that fences at its provider's
// edge, as archil reports itself: no networks, nothing of the account in a
// machine, an egress fence that can close DNS, and URLs that answer only a
// credential (ADR 26-11-16-619 §1).
type edgeDriver struct {
	*fencingDriver
}

func (d edgeDriver) Capabilities() machine.Capabilities {
	caps := d.fencingDriver.Capabilities()
	caps.Bare, caps.Networks = machine.BareAlways, false
	caps.Egress = append(caps.Egress, machine.EgressCloseDNS)
	caps.URLs.Credential = true
	return caps
}

// On a driver that fences at its provider's edge, an isolated runtime makes
// each sandbox's machine on no network, asks nothing stripped of a driver
// that gives nothing, and fences it to the pool's host with DNS closed; a
// machine found by name whose fence leaves DNS open is refused. The runtime
// refuses a network, which such a driver has none of, and a fence that
// leaves DNS open (ADR 26-11-16-619 §1).
func TestMachineRuntimeFencesASandboxAtTheEdge(t *testing.T) {
	ctx := context.Background()
	driver := edgeDriver{&fencingDriver{Driver: vmtest.IngressOptional(vmtest.FakeDriver(t, t.TempDir(), machineTestPool, nil, nil)), fences: map[string]*machine.Egress{}}}
	e, err := engine.Open(driver, machineTestPool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		instances, _ := driver.Instances(context.Background())
		for _, inst := range instances {
			_ = e.Remove(context.Background(), inst, true)
		}
	})
	fence := &machine.Egress{Allow: []string{"7302-pool.zone.archil.app"}, CloseDNS: true}
	config := func(network string, egress *machine.Egress) MachineSandboxRuntimeConfig {
		return MachineSandboxRuntimeConfig{Audit: &fakeAuditPurger{}, Engine: e, HoldToPool: true, Network: network, Egress: egress, Root: layout.ContainerAt(t.TempDir())}
	}
	if _, err := NewMachineSandboxRuntime(config("pool-net", fence)); err == nil || !strings.Contains(err.Error(), "on no network") {
		t.Fatalf("an edge runtime given a network = %v, want refused", err)
	}
	for _, open := range []*machine.Egress{nil, {}, {Allow: fence.Allow}} {
		if _, err := NewMachineSandboxRuntime(config("", open)); err == nil || !strings.Contains(err.Error(), "DNS closed") {
			t.Fatalf("an edge runtime fenced %+v = %v, want refused", open, err)
		}
	}
	r, err := NewMachineSandboxRuntime(config("", fence))
	if err != nil {
		t.Fatal(err)
	}
	if opts := r.createOptions("sbx-1", MachineSize{}); len(opts.Networks) != 0 || opts.Bare || opts.Egress != fence {
		t.Fatalf("an edge sandbox's create = %+v, want no network, nothing stripped and the pool's fence", opts)
	}
	inst, _, err := r.ensureInstance(ctx, "sbx_edge", nil, false)
	if err != nil {
		t.Fatalf("ensure a machine at the edge: %v", err)
	}
	if got := driver.fences[inst.Name]; got == nil || !got.CloseDNS || len(got.Allow) != 1 || got.Allow[0] != fence.Allow[0] {
		t.Fatalf("the machine was made with fence %+v, want the pool's host alone with DNS closed", got)
	}
	if _, err := driver.Create(ctx, machine.InstanceSpec{Name: "sbx-dns", Egress: &machine.Egress{Allow: fence.Allow}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ensureInstance(ctx, "sbx_dns", nil, false); err == nil || !strings.Contains(err.Error(), "not fenced") {
		t.Fatalf("ensure a machine made with DNS open = %v, want it refused", err)
	}
}

func readMachineFile(t *testing.T, guest machine.Guest, path string) string {
	t.Helper()
	rc, err := guest.CopyFrom(context.Background(), path)
	if err != nil {
		t.Fatalf("read %s from the machine: %v", path, err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	if _, err := tr.Next(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func fileTree(t *testing.T, name, content string) io.Reader {
	if t != nil {
		t.Helper()
	}
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))})
		_, _ = tw.Write([]byte(content))
		pw.CloseWithError(tw.Close())
	}()
	return pr
}

// agentDriver is the fake driver with a sandbox agent in its machines. A fake
// guest runs a command on the host, so the agent the pool places has nothing
// to run as; `discobox-provision apply` is answered here instead: what it
// would start — the placed agent, on its bootstrap — is checked to be in the
// guest, and a fake agent is served where the instance's service listens,
// until the machine stops.
type agentDriver struct {
	machine.Driver
	agents *fakeMachineAgents
	// noVolumes is a driver that attaches no volume, and so upgrades no
	// machine, as boxd is.
	noVolumes bool
	// noSystemd is a driver whose base carries no systemd.
	noSystemd bool
	// cannotMountInPlace is an agent from before ADR 26-11-23-458, which
	// does not wire a running machine in place: asked, it answers its
	// usage, and the pool restarts the machine onto it.
	cannotMountInPlace bool
	// inPlaceFailures is how many applies in place to fail, as one cut short
	// part way through its mounts fails.
	inPlaceFailures atomic.Int32
	// rootData is an agent that keeps the sandbox's data on the machine's
	// root, as one that predates wiring a data volume does: it records
	// nothing on the volume (sandboxconfig.MachineDataWiredFile).
	rootData bool
	// workingRootOnRoot is an agent that wires the data volume but leaves the
	// working root of a box with no source on the machine's root, as one from
	// before ADR 26-11-14-302 does: its line on the volume does not name the
	// working root.
	workingRootOnRoot bool
	// upgradeFailures is how many upgrades to fail, as one cut short by the
	// pool agent going away, or by the provider, fails.
	upgradeFailures int
	upgrades        int
	// shutdownRefused has a machine refuse to power off in order, so its
	// stop is forced.
	shutdownRefused atomic.Bool
	// startRefused has every start refused, as a provider that will not
	// resume a paused machine refuses it.
	startRefused atomic.Bool
	// pauseRefused has every pause refused, as a provider that fails one
	// refuses it, leaving the machine running.
	pauseRefused atomic.Bool
	// pauseLate has every pause take effect and then report a failure, as
	// one that timed out after its provider paused the machine does.
	pauseLate atomic.Bool
	// stateUnknown has every power-state read fail, as a provider that
	// cannot be asked does.
	stateUnknown atomic.Bool
	// resumeLost has a resume of a paused machine end with the machine
	// stopped, as a host restart or a provider that expired the pause, or
	// the engine stopping a resume it could not finish, leaves it.
	resumeLost atomic.Bool
	// stateHangs has every power-state read wait until its caller gives up,
	// as a driver whose provider stops answering does.
	stateHangs atomic.Bool
	// stateHangsAfterLoss sets stateHangs once a resume is lost
	// (resumeLost): the provider stops answering just as the pool reads
	// what the lost resume left.
	stateHangsAfterLoss atomic.Bool
	// copyRefused has every create from a committed VM refused with
	// ErrUnsupported, as archil refuses one at a size other than its
	// source's: a copy keeps its source's size.
	copyRefused atomic.Bool
	// createdStarted counts the creates asked to start the machine they
	// make (machine.InstanceSpec.Start).
	createdStarted atomic.Int32
	// listenRefused has every read of where a machine's service listens
	// fail, as a driver that cannot be asked does, once listenAllowed more
	// reads have answered.
	listenRefused atomic.Bool
	listenAllowed atomic.Int32
	mu            sync.Mutex
	// events is each machine's boots and applies, in order.
	events map[string][]string
	// forcedOff, when set, is a machine whose orderly shutdown times out, so
	// it is forced off, and loses what forcedOff undoes in its guest: what
	// was copied in and never reached its disk.
	forcedOff func(ctx context.Context, guest machine.Machine) error
}

// Create asked to start (spec.Start) makes the machine and starts it as this
// driver's Start does, so it boots as a start's machine boots: a create and a
// start, as disco-vm's local drivers make one. A start that fails removes it.
func (d *agentDriver) Create(ctx context.Context, spec machine.InstanceSpec) (machine.Instance, error) {
	if d.copyRefused.Load() && spec.From != nil {
		return machine.Instance{}, fmt.Errorf("a VM made from %s has its size: %w", spec.From.Name, machine.ErrUnsupported)
	}
	start := spec.Start
	spec.Start = nil
	inst, err := d.Driver.Create(ctx, spec)
	if err != nil || start == nil {
		return inst, err
	}
	d.createdStarted.Add(1)
	if _, err := d.Start(ctx, inst, *start); err != nil {
		return machine.Instance{}, errors.Join(err, d.Destroy(context.WithoutCancel(ctx), inst))
	}
	return inst, nil
}

func (d *agentDriver) record(name, event string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.events == nil {
		d.events = map[string][]string{}
	}
	d.events[name] = append(d.events[name], event)
}

// takeEvents is what happened to the machine name since it was last asked.
func (d *agentDriver) takeEvents(name string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	events := d.events[name]
	delete(d.events, name)
	return events
}

// Service owns a bound endpoint for the fixture's lifetime. disco-vm's fake
// driver saves a port number after closing its reservation; using that number
// later would race every other loopback listener on the host. The ingress URL
// is withheld where the driver withholds it (vmtest.IngressOptional), so a
// pool that reached its sandbox there would fail here.
func (d *agentDriver) Service(ctx context.Context, inst machine.Instance) (machine.Service, error) {
	driverService, serviceErr := d.Driver.Service(ctx, inst)
	if serviceErr != nil && !errors.Is(serviceErr, machine.ErrNotRunning) {
		return machine.Service{}, serviceErr
	}
	service, err := d.agents.service(ctx, inst.Name)
	if err != nil {
		return machine.Service{}, err
	}
	if serviceErr == nil && driverService.IngressURL == "" {
		service.IngressURL = ""
	}
	return service, serviceErr
}

func (d *agentDriver) Listen(ctx context.Context, inst machine.Instance) (string, error) {
	if d.listenRefused.Load() && d.listenAllowed.Add(-1) < 0 {
		return "", fmt.Errorf("listen %s: refused: %w", inst.Name, machine.ErrUnavailable)
	}
	service, err := d.agents.service(ctx, inst.Name)
	return "tcp:" + strings.TrimPrefix(service.PoolURL, "http://"), err
}

func (d *agentDriver) Capabilities() machine.Capabilities {
	caps := d.Driver.Capabilities()
	if d.noVolumes {
		caps.Volumes = nil
	}
	return caps
}

func (d *agentDriver) Upgrade(ctx context.Context, inst machine.Instance, from *machine.Instance) (machine.Instance, error) {
	d.upgrades++
	if d.upgradeFailures > 0 {
		d.upgradeFailures--
		return machine.Instance{}, errors.New("the upgrade was cut short")
	}
	return d.Driver.Upgrade(ctx, inst, from)
}

func (d *agentDriver) Start(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	if d.startRefused.Load() {
		return nil, fmt.Errorf("start %s: refused: %w", inst.Name, machine.ErrUnavailable)
	}
	// A paused machine is resumed, booting nothing: its guest, its /run and
	// its agent carry on as they were.
	state, err := d.State(ctx, inst)
	if err != nil {
		return nil, err
	}
	m, err := d.Driver.Start(ctx, inst, opts)
	if err != nil {
		return nil, err
	}
	if state == machine.StatePaused && d.resumeLost.Load() {
		d.agents.stop(inst.Name)
		if err := m.Kill(ctx); err != nil {
			return nil, err
		}
		if d.stateHangsAfterLoss.Load() {
			d.stateHangs.Store(true)
		}
		return nil, fmt.Errorf("resume %s: lost, and stopped: %w", inst.Name, machine.ErrUnavailable)
	}
	if state == machine.StatePaused {
		d.record(inst.Name, "resume")
		return &agentMachine{Machine: m, driver: d, inst: inst}, nil
	}
	// A fake guest's root is a directory that outlives a start, so the new
	// tmpfs a real start mounts at /run is stood in for: no start's token.
	if err := m.CopyTo(ctx, "/run/discobox", fileTree(nil, "machine-wired", "")); err != nil {
		return nil, err
	}
	d.record(inst.Name, "boot")
	am := &agentMachine{Machine: m, driver: d, inst: inst}
	// A machine its agent was placed in, with the link that has systemd start
	// it, starts it at boot, as its units do.
	if err := am.bootAgent(ctx); err != nil {
		return nil, err
	}
	return am, nil
}

// bootAgent starts the agent a boot finds placed with its start link.
func (m *agentMachine) bootAgent(ctx context.Context) error {
	var link string
	for _, unit := range agentlayer.Units {
		if unit.Name == agentlayer.AgentStartUnit {
			link, _ = unit.Link()
		}
	}
	// The link is absolute, to the unit in the guest's /etc: test -e would
	// follow it out of the fake guest's root to the host's, so a host with
	// no such unit would never start the agent. That the link is there is
	// what has systemd start it.
	process, err := m.Machine.Exec(ctx, machine.ExecRequest{Argv: []string{"sh", "-c", `test -L "$DISCO_VM_GUEST_ROOT` + link + `" || exit 0; ` + m.agentCheck()}})
	if err != nil {
		return err
	}
	var bootstrap strings.Builder
	if code, err := process.Wait(&bootstrap, io.Discard); err != nil || code != 0 || bootstrap.Len() == 0 {
		return err
	}
	return m.startAgent(bootstrap.String())
}

// agentCheck is a script that prints the bootstrap of a machine whose agent
// and bootstrap are placed, and fails where they are not.
func (m *agentMachine) agentCheck() string {
	check := `root="$DISCO_VM_GUEST_ROOT"; test -x "$root` + agentlayer.AgentPath + `" && test -f "$root/etc/discobox/sandbox.json" && cat "$root/etc/discobox/sandbox.json"`
	if m.driver.agents.sealedNeverAnswers() {
		// An agent that never comes up in a box made from a snapshot: it
		// starts, and nothing answers.
		check = `if test -e "$DISCO_VM_GUEST_ROOT` + agentlayer.SealedFile + `"; then exit 0; fi; ` + check
	}
	return check
}

func (d *agentDriver) Attach(ctx context.Context, inst machine.Instance) (machine.Machine, error) {
	m, err := d.Driver.Attach(ctx, inst)
	if err != nil {
		return nil, err
	}
	return &agentMachine{Machine: m, driver: d, inst: inst}, nil
}

type agentMachine struct {
	machine.Machine
	driver *agentDriver
	inst   machine.Instance
}

func (m *agentMachine) Exec(ctx context.Context, req machine.ExecRequest) (machine.Process, error) {
	if slices.Equal(req.Argv, snapshotCommand) {
		return m.Machine.Exec(ctx, machine.ExecRequest{Argv: []string{"sh", "-c", m.driver.agents.snapshotScript()}})
	}
	if slices.Equal(req.Argv, provisioner.SystemdCheck) {
		// A fake guest's command runs on the host, whose systemd is not the
		// machine's: the base has it unless the test says otherwise.
		if m.driver.noSystemd {
			return m.Machine.Exec(ctx, machine.ExecRequest{Argv: []string{"false"}})
		}
		return m.Machine.Exec(ctx, machine.ExecRequest{Argv: []string{"true"}})
	}
	if slices.Equal(req.Argv, mountsInPlaceCommand) {
		if m.driver.cannotMountInPlace {
			return m.Machine.Exec(ctx, machine.ExecRequest{Argv: []string{"sh", "-c", "exit 2"}})
		}
		return m.Machine.Exec(ctx, machine.ExecRequest{Argv: []string{"true"}})
	}
	// Apply on a placement that moved restarts the agent onto it: the fake
	// agent started below replaces the one running.
	switch {
	case slices.Equal(req.Argv, applyCommand):
		m.driver.record(m.inst.Name, "apply")
	case slices.Equal(req.Argv, applyInPlaceCommand):
		m.driver.record(m.inst.Name, "apply in place")
		if m.driver.inPlaceFailures.Add(-1) >= 0 {
			return m.Machine.Exec(ctx, machine.ExecRequest{Argv: []string{"sh", "-c", "echo 'wire sandbox in place: cut short' >&2; exit 1"}})
		}
	default:
		return m.Machine.Exec(ctx, req)
	}
	process, err := m.Machine.Exec(ctx, machine.ExecRequest{Argv: []string{"sh", "-c", m.agentCheck()}})
	if err != nil {
		return nil, err
	}
	return &applyProcess{Process: process, machine: m}, nil
}

func (m *agentMachine) Pause(ctx context.Context) error {
	if m.driver.pauseRefused.Load() {
		return fmt.Errorf("pause %s: refused: %w", m.inst.Name, machine.ErrUnavailable)
	}
	if err := m.Machine.Pause(ctx); err != nil {
		return err
	}
	if m.driver.pauseLate.Load() {
		return fmt.Errorf("pause %s: %w", m.inst.Name, context.DeadlineExceeded)
	}
	return nil
}

func (d *agentDriver) State(ctx context.Context, inst machine.Instance) (machine.State, error) {
	if d.stateUnknown.Load() {
		return "", fmt.Errorf("state of %s: %w", inst.Name, machine.ErrUnavailable)
	}
	if d.stateHangs.Load() {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return d.Driver.State(ctx, inst)
}

func (m *agentMachine) Shutdown(ctx context.Context, reboot bool) error {
	if m.driver.shutdownRefused.Load() {
		return errors.New("the guest refused to power off")
	}
	if m.driver.forcedOff != nil {
		return errors.New("the guest did not power off in time")
	}
	m.driver.agents.stop(m.inst.Name)
	return m.Machine.Shutdown(ctx, reboot)
}

func (m *agentMachine) Kill(ctx context.Context) error {
	if lose := m.driver.forcedOff; lose != nil {
		m.driver.forcedOff = nil
		if err := lose(ctx, m.Machine); err != nil {
			return err
		}
	}
	m.driver.agents.stop(m.inst.Name)
	return m.Machine.Kill(ctx)
}

// applyProcess is the check standing in for `apply`; once it passes, the
// agent the bootstrap it printed names is served.
type applyProcess struct {
	machine.Process
	machine *agentMachine
}

func (p *applyProcess) Wait(_, stderr io.Writer) (int, error) {
	var bootstrap strings.Builder
	code, err := p.Process.Wait(&bootstrap, stderr)
	if err != nil || code != 0 {
		return code, err
	}
	if bootstrap.Len() == 0 {
		// Applied, and no agent comes up (sealedBroken).
		return 0, nil
	}
	if err := p.machine.startAgent(bootstrap.String()); err != nil {
		return 1, err
	}
	return 0, nil
}

// startAgent starts the machine's agent on the bootstrap it read, as its
// units do: apply's start, or the boot of a machine its agent was placed in.
func (m *agentMachine) startAgent(bootstrap string) error {
	var cfg sandboxconfig.Config
	if err := json.Unmarshal([]byte(bootstrap), &cfg); err != nil {
		return err
	}
	// The agent's provisioning wires a data volume and says so on it.
	if !m.driver.rootData && m.inst.Data != nil {
		// As the agent does: a token of this start in /run, and the line
		// naming it on the volume.
		token := fmt.Sprintf("start-%d", time.Now().UnixNano())
		if err := m.CopyTo(context.Background(), "/run/discobox", fileTree(nil, "machine-wired", token+"\n")); err != nil {
			return err
		}
		line := sandboxconfig.MachineDataWiredLine(cfg.SandboxID, token, !m.driver.workingRootOnRoot) + "\n"
		if err := m.CopyTo(context.Background(), "/.discobox/data", fileTree(nil, ".discobox-wired", line)); err != nil {
			return err
		}
	}
	layer, err := m.agentLayer()
	if err != nil {
		return err
	}
	return m.driver.agents.start(m.inst.Name, cfg, layer)
}

// agentLayer is the layer an agent started now runs from: where the current
// link points, as the real agent names it by the file it was started from.
func (m *agentMachine) agentLayer() (string, error) {
	process, err := m.Machine.Exec(context.Background(), machine.ExecRequest{Argv: []string{"sh", "-c", `readlink "$DISCO_VM_GUEST_ROOT` + agentlayer.CurrentLink + `"`}})
	if err != nil {
		return "", err
	}
	var target strings.Builder
	if code, err := process.Wait(&target, io.Discard); err != nil || code != 0 {
		return "", fmt.Errorf("read the current link: exit %d: %w", code, err)
	}
	layer, _ := agentlayer.RunningLayer(path.Join(path.Dir(agentlayer.CurrentLink), strings.TrimSpace(target.String()), agentlayer.AgentFile))
	return layer, nil
}

// fakeMachineAgents are the sandbox agents of a test's machines, by machine
// name.
type fakeMachineAgents struct {
	poolKey ed25519.PublicKey
	mu      sync.Mutex
	// snapshotFails makes the snapshot command fail, saying why on stderr;
	// sealedBroken makes an agent never come up in a box made from a
	// snapshot.
	snapshotFails, sealedBroken bool
	// unnamed has every agent name no layer on /healthz, as one older than
	// agentlayer.Health's layer does.
	unnamed bool
	// healthzHeld, when set, holds every agent's /healthz until it is
	// closed, as an agent still coming up does; healthzAsked is told of
	// each request it holds.
	healthzHeld, healthzAsked chan struct{}
	servers                   map[string]*fakeMachineAgent
	endpoints                 map[string]*fakeMachineEndpoint
	// last is each sandbox's agent that ran last, kept after its machine
	// stops, so a test reads what it was told.
	last map[string]*fakeMachineAgent
}

// A stopped agent answers 503, but its listener stays owned so the bootstrap's
// address is stable across boots. Only closeAll releases the endpoints.
type fakeMachineEndpoint struct {
	listener net.Listener
	server   *http.Server
	done     chan struct{}
	err      error // written before done closes
}

func (a *fakeMachineAgents) service(ctx context.Context, name string) (machine.Service, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.endpoints == nil {
		a.endpoints = map[string]*fakeMachineEndpoint{}
	}
	ep := a.endpoints[name]
	if ep == nil {
		listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
		if err != nil {
			return machine.Service{}, err
		}
		ep = &fakeMachineEndpoint{listener: listener, done: make(chan struct{})}
		ep.server = &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			a.mu.Lock()
			agent := a.servers[name]
			a.mu.Unlock()
			if agent == nil {
				http.Error(w, "fake agent is stopped", http.StatusServiceUnavailable)
				return
			}
			agent.ServeHTTP(w, r)
		})}
		a.endpoints[name] = ep
		go func() {
			ep.err = ep.server.Serve(listener)
			close(ep.done)
		}()
	}
	address := ep.listener.Addr().String()
	return machine.Service{IngressURL: "http://" + address, PoolURL: "http://" + address}, nil
}

func (a *fakeMachineAgents) start(name string, cfg sandboxconfig.Config, layer string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	ep := a.endpoints[name]
	if ep == nil || ep.listener.Addr().String() != cfg.AgentRuntime.ListenAddress {
		return fmt.Errorf("fake agent of %s has no reserved listener on %s", name, cfg.AgentRuntime.ListenAddress)
	}
	select {
	case <-ep.done:
		return fmt.Errorf("fake agent of %s cannot start: %w", name, ep.err)
	default:
	}
	if a.unnamed {
		layer = ""
	}
	agent := &fakeMachineAgent{sandboxID: cfg.SandboxID, poolKey: a.poolKey, layer: layer, healthzHeld: a.healthzHeld, healthzAsked: a.healthzAsked}
	if a.servers == nil {
		a.servers = map[string]*fakeMachineAgent{}
	}
	a.servers[name] = agent
	if a.last == nil {
		a.last = map[string]*fakeMachineAgent{}
	}
	a.last[cfg.SandboxID] = agent
	return nil
}

// snapshotScript stands in for the agent's snapshot command in a fake guest:
// it needs the agent placed and the plan, with its client key, on its
// standard input, then provisions (a file at the guest's root says so),
// empties /etc/discobox and seals the box.
func (a *fakeMachineAgents) snapshotScript() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.snapshotFails {
		return `echo "the docker feature failed to provision: exit status 1" >&2; exit 1`
	}
	return `root="$DISCO_VM_GUEST_ROOT"; test -x "$root` + agentlayer.AgentPath + `" || exit 3
plan=$(cat); case "$plan" in *'"runtimeConfig"'*PRIVATE\ KEY*) ;; *) echo "no plan on stdin" >&2; exit 4;; esac
echo provisioned > "$root/provisioned" && rm -rf "$root` + agentlayer.SnapshotDir + `"/* && : > "$root` + agentlayer.SealedFile + `"`
}

func (a *fakeMachineAgents) sealedNeverAnswers() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sealedBroken
}

func (a *fakeMachineAgents) stop(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.servers, name)
}

func (a *fakeMachineAgents) get(t *testing.T, sandboxID string) *fakeMachineAgent {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	agent := a.last[sandboxID]
	if agent == nil {
		t.Fatalf("no agent ran for %s", sandboxID)
	}
	return agent
}

func (a *fakeMachineAgents) closeAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	clear(a.servers)
	for _, ep := range a.endpoints {
		_ = ep.listener.Close()
		_ = ep.server.Close()
		<-ep.done
	}
}

// fakeMachineAgent answers the routes the pool calls, each on a token the
// pool signed for its scope alone.
type fakeMachineAgent struct {
	sandboxID string
	poolKey   ed25519.PublicKey
	// layer is the agent layer it runs from, which its /healthz names.
	layer string
	// healthzHeld and healthzAsked are fakeMachineAgents'.
	healthzHeld, healthzAsked chan struct{}

	mu        sync.Mutex
	delivered []int64
	clears    int
	// confirms and resumes are the power-downs it confirmed and the thaws
	// it was told of (autostop/confirm, autostop/resumed).
	confirms int
	resumes  int
}

// autostopCalls is how many power-downs the agent confirmed and how many
// thaws it was told of.
func (a *fakeMachineAgent) autostopCalls() (confirms, resumes int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.confirms, a.resumes
}

func (a *fakeMachineAgent) revisions() []int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.delivered)
}

func (a *fakeMachineAgent) cacheClears() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.clears
}

func (a *fakeMachineAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	prefix := "/api/projects/proj_1/sandboxes/" + a.sandboxID
	switch {
	case r.URL.Path == "/healthz":
		if a.healthzHeld != nil {
			select {
			case a.healthzAsked <- struct{}{}:
			default:
			}
			select {
			case <-a.healthzHeld:
			case <-r.Context().Done():
				return
			}
		}
		if err := json.NewEncoder(w).Encode(agentlayer.Health{OK: true, Layer: a.layer}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	case r.Method == http.MethodPut && r.URL.Path == prefix+"/runtime-config":
		if !a.authorized(r, sandboxconfig.RuntimeConfigScope) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var doc sandboxconfig.RuntimeConfig
		if err := json.NewDecoder(r.Body).Decode(&doc); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		a.mu.Lock()
		a.delivered = append(a.delivered, doc.Revision)
		a.mu.Unlock()
		if err := json.NewEncoder(w).Encode(doc); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	case r.Method == http.MethodPost && r.URL.Path == prefix+"/cache/clear":
		if !a.authorized(r, sandboxconfig.CacheClearScope) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		a.mu.Lock()
		a.clears++
		a.mu.Unlock()
		_, _ = io.WriteString(w, `{"paths":[]}`)
	case r.Method == http.MethodPost && (r.URL.Path == prefix+"/autostop/confirm" || r.URL.Path == prefix+"/autostop/resumed"):
		if !a.authorized(r, sandboxconfig.AutostopConfirmScope) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		a.mu.Lock()
		if strings.HasSuffix(r.URL.Path, "/confirm") {
			a.confirms++
		} else {
			a.resumes++
		}
		a.mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	case r.Method == http.MethodGet && r.URL.Path == prefix+"/sources":
		_, _ = io.WriteString(w, `{"sources":[]}`)
	default:
		http.NotFound(w, r)
	}
}

func (a *fakeMachineAgent) authorized(r *http.Request, scope string) bool {
	key, err := paseto.NewV4AsymmetricPublicKeyFromEd25519(a.poolKey)
	if err != nil {
		return false
	}
	parser := paseto.NewParser()
	parser.AddRule(paseto.ForAudience(sandboxAgentAudience))
	token, err := parser.ParseV4Public(key, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), nil)
	if err != nil {
		return false
	}
	var scopes []string
	if err := token.Get("scopes", &scopes); err != nil {
		return false
	}
	sandboxID, _ := token.GetString("sandbox_id")
	return sandboxID == a.sandboxID && slices.Equal(scopes, []string{scope})
}

// The reaper removes only a machine that a generated sandbox ID would name and
// that no sandbox of this pool or of the control plane names, and only once it
// has been so for the retention a tree is kept for: a machine of another name
// in the project, one merely beginning `sbx-` or spelling a short ID among
// them, is nothing this runtime made.
func TestMachineOrphansAreRemovedAfterRetention(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	if _, err := f.runtime.CreateSandbox(ctx, machineCreate("sbx_kept")); err != nil {
		t.Fatal(err)
	}
	e := &engine.Engine{Driver: f.owner, Pool: machineTestPool}
	for _, name := range []string{"sbx-dead000000000000", "sbx-taken00000000000", "sbx-my-box", "sbx-test", "pool-own-machine"} {
		if _, err := e.Create(ctx, engine.CreateOptions{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	// The orphan goes as a delete takes its sandbox, with what else the pool
	// keeps for it: its proxy material here.
	if _, err := proxyagent.EnsureSandboxMaterial(f.runtime.root, f.runtime.projectID, f.runtime.poolID, "sbx_dead000000000000"); err != nil {
		t.Fatal(err)
	}
	held := func(context.Context) ([]string, error) { return []string{"sbx_taken00000000000"}, nil }
	names := func() []string {
		t.Helper()
		all, err := f.owner.Instances(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, inst := range all {
			out = append(out, inst.Name)
		}
		slices.Sort(out)
		return out
	}
	now := time.Now()
	f.runtime.reapOrphanMachines(ctx, quietLogger(), held, now)
	if got := names(); !slices.Equal(got, []string{"pool-own-machine", "sbx-dead000000000000", "sbx-kept", "sbx-my-box", "sbx-taken00000000000", "sbx-test"}) {
		t.Fatalf("machines after the first pass = %v, want every one kept", got)
	}
	f.runtime.reapOrphanMachines(ctx, quietLogger(), held, now.Add(sandboxVolumeRetention+time.Minute))
	if got := names(); !slices.Equal(got, []string{"pool-own-machine", "sbx-kept", "sbx-my-box", "sbx-taken00000000000", "sbx-test"}) {
		t.Fatalf("machines after retention = %v, want only the orphan removed", got)
	}
	if _, err := os.Stat(filepath.Join(proxyagent.PoolSandboxMaterialRoot(f.runtime.root, f.runtime.projectID, f.runtime.poolID), "sbx_dead000000000000")); !os.IsNotExist(err) {
		t.Fatalf("orphan's proxy material after it was removed: stat err = %v, want it gone", err)
	}
	// Its audit trail is left to the proxy's age sweep (ADR 26-11-14-896 §5).
	if purged := f.runtime.audit.(*fakeAuditPurger).purges(); len(purged) != 0 {
		t.Fatalf("the orphan sweep purged audit trails %v, want none", purged)
	}
}

// A start whose placement fails stops the machine it started: left running
// with nothing placed, every later start would find it running and place
// nothing.
func TestMachineStartThatFailsStopsTheMachine(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	f.runtime.agentLayers = &fakeAgentLayers{digest: "sha256:" + strings.Repeat("c", 64), layer: []byte("not a layer")}
	if _, err := f.runtime.CreateSandbox(ctx, machineCreate("sbx_6")); err == nil {
		t.Fatal("create with an agent layer that does not read succeeded")
	}
	inst, err := f.owner.Instance(ctx, "sbx-6")
	if err != nil {
		t.Fatal(err)
	}
	if state := f.runtime.engine.State(ctx, inst); state != engine.Stopped {
		t.Fatalf("machine after a failed start is %s, want stopped", state)
	}
}

// A machine that runs with nothing finished placing in it — its pool agent
// gone mid-start — is placed when it is next asked to run, not answered as
// running: the pool's record of what it placed is what says it was.
func TestMachineRunningWithoutItsPlacementIsPlacedAgain(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_7")); err != nil {
		t.Fatal(err)
	}
	// The agent this pool started is gone, and so is its record of placing
	// it, as when the pool agent died between the machine's start and the
	// end of its placement; and a restarted pool agent has seen nothing.
	f.agents.stop("sbx-7")
	if err := os.Remove(r.placedRecordPath("sbx_7")); err != nil {
		t.Fatal(err)
	}
	// What a restarted pool agent's first poll sees: the machine running.
	// That is a power state, and no answer that it is started.
	r.observe("sbx_7", StatusRunning)

	if err := r.EnsureSandboxRunning(ctx, "sbx_7"); err != nil {
		t.Fatal(err)
	}
	dial, err := r.SandboxDialer(ctx, "sbx_7", SandboxAgentPort)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sandboxAgentHealth(ctx, dial); err != nil {
		t.Fatalf("agent after the machine was placed again: %v", err)
	}
	if _, ok, err := r.readPlaced("sbx_7"); err != nil || !ok {
		t.Fatalf("placement record = %v, %v; want it written again", ok, err)
	}
}

// fencingDriver is the fake driver as one that fences a machine's egress: it
// lists Egress, keeps each machine's fence itself, since the fake driver
// refuses one, and reports it as the provider would.
type fencingDriver struct {
	machine.Driver
	mu     sync.Mutex
	fences map[string]*machine.Egress
}

func (d *fencingDriver) Capabilities() machine.Capabilities {
	caps := d.Driver.Capabilities()
	caps.Egress = []machine.EgressFence{machine.EgressNone, machine.EgressAllow}
	return caps
}

func (d *fencingDriver) Create(ctx context.Context, spec machine.InstanceSpec) (machine.Instance, error) {
	fence := spec.Egress
	spec.Egress = nil
	inst, err := d.Driver.Create(ctx, spec)
	if err != nil {
		return inst, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fences[inst.Name] = fence
	inst.Egress = fence
	return inst, nil
}

func (d *fencingDriver) Instance(ctx context.Context, name string) (machine.Instance, error) {
	inst, err := d.Driver.Instance(ctx, name)
	d.mu.Lock()
	defer d.mu.Unlock()
	inst.Egress = d.fences[inst.Name]
	return inst, err
}

// A sandbox's machine is made with the pool's fence, which the driver sets
// before the create returns; one found by name that is not fenced so, made
// before the fence or by hand, is refused rather than run (ADR 26-10-29-481
// §4).
func TestMachineCreateFencesTheMachine(t *testing.T) {
	ctx := context.Background()
	driver := &fencingDriver{Driver: vmtest.IngressOptional(vmtest.FakeDriver(t, t.TempDir(), machineTestPool, nil, nil)), fences: map[string]*machine.Egress{}}
	e, err := engine.Open(driver, machineTestPool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		instances, _ := driver.Instances(context.Background())
		for _, inst := range instances {
			_ = e.Remove(context.Background(), inst, true)
		}
	})
	fence := &machine.Egress{}
	r, err := NewMachineSandboxRuntime(MachineSandboxRuntimeConfig{Audit: &fakeAuditPurger{}, Engine: e, Egress: fence, Root: layout.ContainerAt(t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}

	inst, _, err := r.ensureInstance(ctx, "sbx_fenced", nil, false)
	if err != nil {
		t.Fatalf("ensure a fenced machine: %v", err)
	}
	if got := driver.fences[inst.Name]; got == nil || len(got.Allow) != 0 {
		t.Fatalf("the machine was made with fence %+v, want one that reaches nothing beyond its network", got)
	}
	if _, _, err := r.ensureInstance(ctx, "sbx_fenced", nil, false); err != nil {
		t.Fatalf("find the fenced machine again: %v", err)
	}

	if _, err := e.Create(ctx, engine.CreateOptions{Name: "sbx-open"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ensureInstance(ctx, "sbx_open", nil, false); err == nil || !strings.Contains(err.Error(), "not fenced") {
		t.Fatalf("ensure a machine made without the fence = %v, want it refused", err)
	}
}

// A runtime asked to fence its sandboxes' machines refuses a driver that
// cannot (ADR 26-10-29-481 §4).
func TestMachineRuntimeRefusesADriverThatCannotFence(t *testing.T) {
	e, err := engine.Open(vmtest.FakeDriver(t, t.TempDir(), machineTestPool, nil, nil), machineTestPool)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewMachineSandboxRuntime(MachineSandboxRuntimeConfig{Audit: &fakeAuditPurger{}, Engine: e, Egress: &machine.Egress{}, Root: layout.ContainerAt(t.TempDir())})
	if err == nil || !strings.Contains(err.Error(), "cannot fence") {
		t.Fatalf("runtime on the fake driver, fenced = %v, want refused", err)
	}
}

func TestSameEgressComparesAllowedHostsAsASet(t *testing.T) {
	for _, tc := range []struct {
		name      string
		got, want *machine.Egress
		same      bool
	}{
		{name: "both unfenced", same: true},
		{name: "unfenced is not fenced", want: &machine.Egress{}},
		{name: "fenced to nothing", got: &machine.Egress{}, want: &machine.Egress{}, same: true},
		{name: "in any order and case", got: &machine.Egress{Allow: []string{"B.example", "a.example"}}, want: &machine.Egress{Allow: []string{"a.example", "b.example"}}, same: true},
		{name: "a host more", got: &machine.Egress{Allow: []string{"a.example", "b.example"}}, want: &machine.Egress{Allow: []string{"a.example"}}},
		{name: "a host fewer", got: &machine.Egress{Allow: []string{"a.example"}}, want: &machine.Egress{Allow: []string{"a.example", "b.example"}}},
		{name: "DNS open where it is closed", got: &machine.Egress{Allow: []string{"a.example"}}, want: &machine.Egress{Allow: []string{"a.example"}, CloseDNS: true}},
		{name: "DNS closed where it is open", got: &machine.Egress{Allow: []string{"a.example"}, CloseDNS: true}, want: &machine.Egress{Allow: []string{"a.example"}}},
		{name: "an empty list closes DNS whatever it says", got: &machine.Egress{CloseDNS: true}, want: &machine.Egress{}, same: true},
		{name: "DNS closed in both", got: &machine.Egress{Allow: []string{"a.example"}, CloseDNS: true}, want: &machine.Egress{Allow: []string{"a.example"}, CloseDNS: true}, same: true},
	} {
		if got := sameEgress(tc.got, tc.want); got != tc.same {
			t.Errorf("%s: sameEgress = %t, want %t", tc.name, got, tc.same)
		}
	}
}

// A machine is reached as its bootstrap told its agent: its pool turning TLS
// on rewrites nothing of a running machine, which takes the setting when its
// next start places its bootstrap again (ADR 26-10-29-481 §3).
func TestARunningMachineIsReachedAsItWasMade(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_1")); err != nil {
		t.Fatal(err)
	}
	if serves, err := r.agentServesTLS("sbx_1"); err != nil || serves {
		t.Fatalf("agentServesTLS = %v, %v; want a machine made plain reached plain", serves, err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-1")
	if err != nil {
		t.Fatal(err)
	}
	if current, err := r.placementCurrent(ctx, "sbx_1", inst.ID); err != nil || !current {
		t.Fatalf("placementCurrent = %v, %v; want the machine as it was placed", current, err)
	}

	r.agentTLS = true
	record, _, err := r.readMachineRecord("sbx_1")
	if err != nil {
		t.Fatal(err)
	}
	if record.Bootstrap.Runtime.AgentRuntime.TLS {
		t.Fatal("turning the pool's TLS on rewrote a running machine's bootstrap")
	}
	if serves, err := r.agentServesTLS("sbx_1"); err != nil || serves {
		t.Fatalf("agentServesTLS = %v, %v; want a running machine still reached as it was made", serves, err)
	}
}

// A machine sandbox's sources are served from the same git-origins route as a
// container's, so staging it records which of them come from the pool's cache:
// without the record the route serves an empty repository of the sandbox's
// own, and its clone waits forever on a source the cache holds.
func TestMachineStagingRecordsSourceCaches(t *testing.T) {
	f := newMachineFixture(t, nil)
	req := machineCreate(deliveryTestSandboxID)
	req.Config.Source = originTestRequest("/home/dev/project").Config.Source
	if err := os.MkdirAll(f.runtime.sandboxRoot(deliveryTestSandboxID), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runtime.stageSandbox(context.Background(), deliveryTestSandboxID, req); err != nil {
		t.Fatal(err)
	}
	caches, err := f.runtime.readSourceCaches(deliveryTestSandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if caches["primary"] != originTestCacheKey {
		t.Fatalf("recorded caches = %v, want the primary's %q", caches, originTestCacheKey)
	}
}

// A machine forced off on its way to booting on what was placed may have
// lost the copy, so that placement is recorded only once its agent has
// answered, which a lost layer would not let it do. It is recorded then, so
// the running machine is not restarted again by the next request.
func TestAPlacementAfterAForcedRestartIsRecordedOnceItsAgentAnswers(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	f.driver.cannotMountInPlace = true
	f.driver.shutdownRefused.Store(true)
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_9")); err != nil {
		t.Fatal(err)
	}
	if got, want := f.driver.takeEvents("sbx-9"), []string{"boot", "boot"}; !slices.Equal(got, want) {
		t.Fatalf("create = %v, want %v", got, want)
	}
	if _, ok, err := r.readPlaced("sbx_9"); err != nil || !ok {
		t.Fatalf("placement record once the agent answered: %v %v", ok, err)
	}
	if err := r.EnsureSandboxRunning(ctx, "sbx_9"); err != nil {
		t.Fatal(err)
	}
	if got := f.driver.takeEvents("sbx-9"); len(got) != 0 {
		t.Fatalf("a request after a forced restart = %v, want nothing: the machine runs what was placed", got)
	}

	// The placement itself records nothing: it hands the record back.
	if err := os.Remove(r.placedRecordPath("sbx_9")); err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-9")
	if err != nil {
		t.Fatal(err)
	}
	record, _, err := r.readMachineRecord("sbx_9")
	if err != nil {
		t.Fatal(err)
	}
	moved, pending, err := r.place(ctx, "sbx_9", inst, record, restartedForced)
	if err != nil || moved || pending == nil || pending.Container != inst.ID {
		t.Fatalf("place after a forced restart = %v, %+v, %v; want the placement handed back", moved, pending, err)
	}
	if _, ok, err := r.readPlaced("sbx_9"); err != nil || ok {
		t.Fatalf("placement record before the agent answered: %v %v, want none", ok, err)
	}
}

// A sandbox's machine, and a snapshot build's, asks for its pool URL on and
// its ingress URL off, both strictly: the pool, its peer, reaches it at its
// pool URL alone, and an ingress URL would be an inbound path from outside
// the pool that the guest's root answers and the pool's fence never sees
// (disco-vm ADR 26-11-14-610).
func TestMachineAsksPoolURLAndNoIngressURL(t *testing.T) {
	want := machine.URLRequests{
		Ingress: machine.URLRequest{Mode: machine.URLOff, Strict: true},
		Pool:    machine.URLRequest{Mode: machine.URLOn, Strict: true},
	}
	for _, r := range []*MachineSandboxRuntime{{bare: true, network: "pool-0123456789abcdef"}, {}} {
		if opts := r.createOptions("sbx-1", MachineSize{}); opts.URLs != want {
			t.Fatalf("create options ask URLs %+v, want ingress off and pool on, both strictly", opts.URLs)
		}
	}
}

func TestCanceledCallerLeavesMachineEndpointRefreshRunning(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	driver := &endpointRefreshDriver{service: func(ctx context.Context) (machine.Service, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-ctx.Done():
			return machine.Service{}, ctx.Err()
		case <-release:
			if err := ctx.Err(); err != nil {
				return machine.Service{}, err
			}
			return machine.Service{
				PoolURL: "https://edge.example.test",
				Credential: &machine.Credential{
					Header: "X-Edge-Token", Token: "new-token", Expires: time.Now().Add(time.Hour),
				},
			}, nil
		}
	}}
	r := &MachineSandboxRuntime{
		sandboxCore: sandboxCore{root: layout.ContainerAt(t.TempDir())},
		engine:      &engine.Engine{Driver: driver},
	}
	const sandboxID = "sbx_1"
	if err := os.MkdirAll(r.sandboxRoot(sandboxID), 0o755); err != nil {
		t.Fatal(err)
	}
	cache := &machineEndpointCache{}
	cache.endpoint.Store(&machine.Endpoint{Credential: &machine.Credential{Expires: time.Now().Add(-time.Minute)}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := r.agentEndpoint(ctx, sandboxID, cache)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider lookup did not start")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled caller: %v", err)
	}
	close(release)
	waitCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	endpoint, err := r.agentEndpoint(waitCtx, sandboxID, cache)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.Credential.Token != "new-token" || calls.Load() != 1 {
		t.Fatalf("expected one refresh and a fresh credential, saw %d provider calls", calls.Load())
	}
	if cached, err := r.agentEndpoint(waitCtx, sandboxID, cache); err != nil || cached != endpoint {
		t.Fatalf("expected the refreshed endpoint to be cached, got error %v", err)
	}
}

type endpointRefreshDriver struct {
	machine.Driver
	service func(context.Context) (machine.Service, error)
}

func (d *endpointRefreshDriver) Instance(_ context.Context, name string) (machine.Instance, error) {
	return machine.Instance{Name: name}, nil
}

func (d *endpointRefreshDriver) Capabilities() machine.Capabilities {
	return machine.Capabilities{URLs: machine.URLCapabilities{Credential: true}}
}

func (d *endpointRefreshDriver) Service(ctx context.Context, _ machine.Instance) (machine.Service, error) {
	return d.service(ctx)
}

// A machine its provider paused (disco-vm ADR 26-11-21-438) reads as paused
// however often it is read, which wakes nothing, and is reported as a pause,
// though not one the pool made (StatusPaused, not StatusSuspended; ADR
// 26-11-16-619 §5). A use resumes it on the machine it had, and so does the
// state poll's reconcile.
func TestAPausedMachineIsNoSleepAndIsResumed(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	first, err := r.CreateSandbox(ctx, machineCreate("sbx_9"))
	if err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-9")
	if err != nil {
		t.Fatal(err)
	}
	pause := func() {
		t.Helper()
		if err := fake.Pause(ctx, f.owner, inst); err != nil {
			t.Fatal(err)
		}
	}
	running := func(how string) {
		t.Helper()
		sb, err := r.GetSandbox(ctx, "sbx_9")
		if err != nil {
			t.Fatal(err)
		}
		if sb.ID != first.ID || sb.Status != StatusRunning {
			t.Fatalf("sandbox resumed by %s = %+v, want machine %s running", how, sb, first.ID)
		}
	}
	pause()
	for _, when := range []string{"paused", "paused, read again"} {
		sb, err := r.GetSandbox(ctx, "sbx_9")
		if err != nil {
			t.Fatal(err)
		}
		if sb.Status != StatusPaused {
			t.Fatalf("a machine %s reads %s, want %s", when, sb.Status, StatusPaused)
		}
		if state := stateFromStatus(sb.Status); state != StateSuspended {
			t.Fatalf("a machine %s is reported %s, want %s: a provider's pause is reported as the pause it is", when, state, StateSuspended)
		}
	}
	if err := r.EnsureSandboxRunning(ctx, "sbx_9"); err != nil {
		t.Fatalf("use a paused machine's sandbox: %v", err)
	}
	running("a use")

	pause()
	if _, err := r.GetSandbox(ctx, "sbx_9"); err != nil {
		t.Fatal(err)
	}
	if err := r.resumePaused(ctx, "sbx_9"); err != nil {
		t.Fatalf("the state poll's resume: %v", err)
	}
	running("the state poll")
}

// The state poll's resume starts only a machine still paused: a sandbox
// stopped since stays stopped.
func TestTheStatePollDoesNotStartAStoppedMachine(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_10")); err != nil {
		t.Fatal(err)
	}
	if err := r.StopSandbox(ctx, "sbx_10", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.resumePaused(ctx, "sbx_10"); err != nil {
		t.Fatal(err)
	}
	sb, err := r.GetSandbox(ctx, "sbx_10")
	if err != nil {
		t.Fatal(err)
	}
	if sb.Status != StatusStopped {
		t.Fatalf("a stopped sandbox after the poll's resume is %s, want stopped", sb.Status)
	}
}

// A paused machine owed a new root is stopped for it, as a running one is,
// rather than handed to an upgrade that refuses a machine not stopped.
func TestAPausedMachineOwedANewRootIsStoppedForIt(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_11")); err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-11")
	if err != nil {
		t.Fatal(err)
	}
	writeMachineFiles(t, r, inst)
	record, _, err := r.readMachineRecord("sbx_11")
	if err != nil {
		t.Fatal(err)
	}
	record.NewRoot = true
	if err := r.writeMachineRecord("sbx_11", record); err != nil {
		t.Fatal(err)
	}
	if err := fake.Pause(ctx, f.owner, inst); err != nil {
		t.Fatal(err)
	}
	if _, err := r.GetSandbox(ctx, "sbx_11"); err != nil {
		t.Fatal(err)
	}
	if err := r.resumePaused(ctx, "sbx_11"); err != nil {
		t.Fatalf("resume a paused machine owed a new root: %v", err)
	}
	if f.driver.upgrades != 1 {
		t.Fatalf("upgrades = %d, want one", f.driver.upgrades)
	}
	sb, err := r.GetSandbox(ctx, "sbx_11")
	if err != nil {
		t.Fatal(err)
	}
	if sb.Status != StatusRunning {
		t.Fatalf("sandbox = %s, want running on its new root", sb.Status)
	}
	assertNewRootKeptVolumes(t, r, inst)
}

// A resume its provider refuses leaves the machine paused, and so reports it
// paused, never stopped; and the state poll does not ask again until
// resumeRetryInterval has passed.
func TestARefusedResumeIsNoStopAndIsBackedOff(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_12")); err != nil {
		t.Fatal(err)
	}
	inst, err := f.owner.Instance(ctx, "sbx-12")
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.Pause(ctx, f.owner, inst); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var published []string
	r.statePublisher.Store(func(_ context.Context, batch SandboxStateBatch) error {
		mu.Lock()
		defer mu.Unlock()
		for _, s := range batch.States {
			published = append(published, s.State)
		}
		return nil
	})
	f.driver.startRefused.Store(true)
	if _, err := r.GetSandbox(ctx, "sbx_12"); err != nil {
		t.Fatal(err)
	}
	if err := r.resumePaused(ctx, "sbx_12"); err == nil {
		t.Fatal("a refused resume succeeded")
	}
	if state := r.engine.State(ctx, inst); state != engine.Paused {
		t.Fatalf("a machine whose resume was refused is %s, want paused", state)
	}
	mu.Lock()
	last := published[len(published)-1]
	mu.Unlock()
	if last != StateSuspended {
		t.Fatalf("published %v after a refused resume, want it to end %s: still paused, no stop", published, StateSuspended)
	}
	r.resumeFailed.Store("sbx_12", time.Now())
	r.resumeInBackground(ctx, slog.Default(), "sbx_12")
	if _, resuming := r.resuming.Load("sbx_12"); resuming {
		t.Fatal("the state poll resumed again within resumeRetryInterval of a refused resume")
	}
}
