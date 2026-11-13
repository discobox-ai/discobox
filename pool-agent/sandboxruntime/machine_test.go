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
	"net"
	"net/http"
	"os"
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

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/layout"
	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pool-agent/buildkitagent"
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
	driver := &agentDriver{Driver: vmtest.FakeDriver(t, dir, machineTestPool, machine.StaticKey(key), config), agents: agents}
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
	service, err := f.driver.Service(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if "tcp:"+bootstrap.AgentRuntime.ListenAddress != service.Listen || bootstrap.SandboxID != "sbx_1" {
		t.Fatalf("bootstrap listens on %q for %q, want the service's %q for sbx_1", bootstrap.AgentRuntime.ListenAddress, bootstrap.SandboxID, service.Listen)
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
	if err := sandboxAgentHealthy(ctx, dial); err != nil {
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

// A machine boots before anything is placed in it, its provider's services
// with it, so its first placement starts nothing and restarts it: the wiring
// runs at that boot, before those services and the agent (ADR 26-11-02-023
// §3). It is placed the wire unit's machine drop-in. A start after that
// places and starts the agent at the boot it has, with no restart.
func TestAMachinesFirstPlacementRestartsItBeforeAnythingRuns(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	r := f.runtime
	if _, err := r.CreateSandbox(ctx, machineCreate("sbx_9")); err != nil {
		t.Fatal(err)
	}
	if got, want := f.driver.takeEvents("sbx-9"), []string{"boot", "boot"}; !slices.Equal(got, want) {
		t.Fatalf("create = %v, want %v: a restart before anything is applied", got, want)
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

	if err := r.StopSandbox(ctx, "sbx_9", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.StartSandbox(ctx, "sbx_9", nil); err != nil {
		t.Fatal(err)
	}
	if got, want := f.driver.takeEvents("sbx-9"), []string{"boot", "apply"}; !slices.Equal(got, want) {
		t.Fatalf("start = %v, want %v", got, want)
	}

	// A machine placed before with another agent -- one provisioned before
	// its volumes were mounted, given the agent that mounts them -- starts
	// nothing on the running machine either: that agent would mount them
	// under the provider's services already running on those paths.
	placed, ok, err := r.readPlaced("sbx_9")
	if err != nil || !ok {
		t.Fatalf("placement record: %v %v", ok, err)
	}
	placed.Layer = "sha256:an-older-agent"
	if err := r.writePlaced("sbx_9", placed); err != nil {
		t.Fatal(err)
	}
	if err := r.StopSandbox(ctx, "sbx_9", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.StartSandbox(ctx, "sbx_9", nil); err != nil {
		t.Fatal(err)
	}
	if got, want := f.driver.takeEvents("sbx-9"), []string{"boot", "boot"}; !slices.Equal(got, want) {
		t.Fatalf("start onto a new agent = %v, want %v: a restart before anything is applied", got, want)
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
	if err := sandboxAgentHealthy(ctx, mustDialer(t, r, "sbx_7")); err != nil {
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

func mustDialer(t *testing.T, r *MachineSandboxRuntime, sandboxID string) Dialer {
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

// A machine cannot be frozen, so a suspend is refused, and a machine's tree is
// its disk, so an export is.
func TestMachineRefusesWhatItCannotDo(t *testing.T) {
	f := newMachineFixture(t, nil)
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
	_, err = NewMachineSandboxRuntime(MachineSandboxRuntimeConfig{Engine: e, Isolate: true, Network: "pool-net", Root: layout.ContainerAt(t.TempDir())})
	if err == nil || !strings.Contains(err.Error(), "cannot isolate") {
		t.Fatalf("runtime on the fake driver, isolated = %v, want refused", err)
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
	// rootData is an agent that keeps the sandbox's data on the machine's
	// root, as one that predates wiring a data volume does: it records
	// nothing on the volume (sandboxconfig.MachineDataWiredFile).
	rootData bool
	// upgradeFailures is how many upgrades to fail, as one cut short by the
	// pool agent going away, or by the provider, fails.
	upgradeFailures int
	upgrades        int
	// shutdownRefused has a machine refuse to power off in order, so its
	// stop is forced.
	shutdownRefused atomic.Bool
	mu              sync.Mutex
	// events is each machine's boots and applies, in order.
	events map[string][]string
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
// later would race every other loopback listener on the host.
func (d *agentDriver) Service(ctx context.Context, inst machine.Instance) (machine.Service, error) {
	_, serviceErr := d.Driver.Service(ctx, inst)
	if serviceErr != nil && !errors.Is(serviceErr, machine.ErrNotRunning) {
		return machine.Service{}, serviceErr
	}
	service, err := d.agents.service(ctx, inst.Name)
	if err != nil {
		return machine.Service{}, err
	}
	return service, serviceErr
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
	m, err := d.Driver.Start(ctx, inst, opts)
	if err != nil {
		return nil, err
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
	if !slices.Equal(req.Argv, []string{agentlayer.AgentPath, "provision", "apply"}) {
		return m.Machine.Exec(ctx, req)
	}
	m.driver.record(m.inst.Name, "apply")
	process, err := m.Machine.Exec(ctx, machine.ExecRequest{Argv: []string{"sh", "-c", m.agentCheck()}})
	if err != nil {
		return nil, err
	}
	return &applyProcess{Process: process, machine: m}, nil
}

func (m *agentMachine) Shutdown(ctx context.Context, reboot bool) error {
	if m.driver.shutdownRefused.Load() {
		return errors.New("the guest refused to power off")
	}
	m.driver.agents.stop(m.inst.Name)
	return m.Machine.Shutdown(ctx, reboot)
}

func (m *agentMachine) Kill(ctx context.Context) error {
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
		line := cfg.SandboxID + " " + token + "\n"
		if err := m.CopyTo(context.Background(), "/.discobox/data", fileTree(nil, ".discobox-wired", line)); err != nil {
			return err
		}
	}
	return m.driver.agents.start(m.inst.Name, cfg)
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
	servers                     map[string]*fakeMachineAgent
	endpoints                   map[string]*fakeMachineEndpoint
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
	return machine.Service{Listen: "tcp:" + address, URL: "http://" + address, PrivateURL: "http://" + address}, nil
}

func (a *fakeMachineAgents) start(name string, cfg sandboxconfig.Config) error {
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
	agent := &fakeMachineAgent{sandboxID: cfg.SandboxID, poolKey: a.poolKey}
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

	mu        sync.Mutex
	delivered []int64
	clears    int
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
		w.WriteHeader(http.StatusOK)
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

// The reaper removes only a machine that a sandbox ID would name and that no
// sandbox of this pool or of the control plane names, and only once it has
// been so for the retention a tree is kept for: a machine of another name in
// the project is nothing this runtime made.
func TestMachineOrphansAreRemovedAfterRetention(t *testing.T) {
	f := newMachineFixture(t, nil)
	ctx := context.Background()
	if _, err := f.runtime.CreateSandbox(ctx, machineCreate("sbx_kept")); err != nil {
		t.Fatal(err)
	}
	e := &engine.Engine{Driver: f.owner, Pool: machineTestPool}
	for _, name := range []string{"sbx-orphan", "sbx-held", "pool-own-machine"} {
		if _, err := e.Create(ctx, engine.CreateOptions{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	held := func(context.Context) ([]string, error) { return []string{"sbx_held"}, nil }
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
	if got := names(); !slices.Equal(got, []string{"pool-own-machine", "sbx-held", "sbx-kept", "sbx-orphan"}) {
		t.Fatalf("machines after the first pass = %v, want every one kept", got)
	}
	f.runtime.reapOrphanMachines(ctx, quietLogger(), held, now.Add(sandboxVolumeRetention+time.Minute))
	if got := names(); !slices.Equal(got, []string{"pool-own-machine", "sbx-held", "sbx-kept"}) {
		t.Fatalf("machines after retention = %v, want only the orphan removed", got)
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
	if err := sandboxAgentHealthy(ctx, dial); err != nil {
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
	driver := &fencingDriver{Driver: vmtest.FakeDriver(t, t.TempDir(), machineTestPool, nil, nil), fences: map[string]*machine.Egress{}}
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
	r, err := NewMachineSandboxRuntime(MachineSandboxRuntimeConfig{Engine: e, Egress: fence, Root: layout.ContainerAt(t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}

	inst, _, err := r.ensureInstance(ctx, "sbx_fenced", nil)
	if err != nil {
		t.Fatalf("ensure a fenced machine: %v", err)
	}
	if got := driver.fences[inst.Name]; got == nil || len(got.Allow) != 0 {
		t.Fatalf("the machine was made with fence %+v, want one that reaches nothing beyond its network", got)
	}
	if _, _, err := r.ensureInstance(ctx, "sbx_fenced", nil); err != nil {
		t.Fatalf("find the fenced machine again: %v", err)
	}

	if _, err := e.Create(ctx, engine.CreateOptions{Name: "sbx-open"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.ensureInstance(ctx, "sbx_open", nil); err == nil || !strings.Contains(err.Error(), "not fenced") {
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
	_, err = NewMachineSandboxRuntime(MachineSandboxRuntimeConfig{Engine: e, Egress: &machine.Egress{}, Root: layout.ContainerAt(t.TempDir())})
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
