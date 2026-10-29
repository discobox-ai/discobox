package sandboxruntime

import (
	"archive/tar"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	"github.com/discobox-ai/discobox/pool-agent/vmtest"

	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
)

// machineTestProject is the disco-vm project these tests' pools make their
// sandboxes' machines in.
const machineTestProject = "pool-machine-test"

// machineFixture is a machine pool's runtime over disco-vm's fake driver,
// whose engine calls with a key the driver issued, as a pool machine's does
// (ADR 26-10-23-551 §4), and the sandbox agents its machines run.
type machineFixture struct {
	runtime *MachineSandboxRuntime
	// owner is the driver as the account's owner, which a test reads the
	// machines through behind the pool's back.
	owner  machine.Driver
	agents *fakeMachineAgents
	key    ed25519.PrivateKey
}

func newMachineFixture(t *testing.T) *machineFixture {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	owner := vmtest.FakeDriver(t, dir, machineTestProject, nil)
	key, err := owner.IssueKey(ctx, machine.IssueRequest{Name: "pool-1", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	agents := &fakeMachineAgents{t: t, servers: map[string]*fakeMachineAgent{}}
	driver := &agentDriver{Driver: vmtest.FakeDriver(t, dir, machineTestProject, machine.StaticKey(key)), agents: agents}
	e, err := engine.Open(driver, machineTestProject)
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
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		instances, _ := owner.Instances(context.Background())
		for _, inst := range instances {
			_ = (&engine.Engine{Driver: owner, Project: machineTestProject}).Remove(context.Background(), inst, true)
		}
		agents.closeAll()
	})
	return &machineFixture{runtime: r, owner: owner, agents: agents, key: identity}
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
	f := newMachineFixture(t)
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
	if inst.Base == nil || inst.Base.ID != vmtest.Base {
		t.Fatalf("machine %+v, want one of the driver's base", inst)
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
	service, err := f.owner.Service(ctx, inst)
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

// A create whose answer was lost made the machine already: the next one finds
// it by name rather than making a second.
func TestMachineCreateFindsALostCreatesMachineByName(t *testing.T) {
	f := newMachineFixture(t)
	ctx := context.Background()
	e := &engine.Engine{Driver: f.owner, Project: machineTestProject}
	lost, err := e.Create(ctx, vmtest.Base, engine.CreateOptions{Name: "sbx-2"})
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
	f := newMachineFixture(t)
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

// Archiving keeps the machine, stopped, and its unarchiving create starts the
// same one again.
func TestMachineArchiveKeepsTheMachine(t *testing.T) {
	f := newMachineFixture(t)
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

// A machine cannot be frozen, so a suspend is refused, and a machine's tree is
// its disk, so an export is.
func TestMachineRefusesWhatItCannotDo(t *testing.T) {
	f := newMachineFixture(t)
	ctx := context.Background()
	if _, err := f.runtime.CreateSandbox(ctx, machineCreate("sbx_5")); err != nil {
		t.Fatal(err)
	}
	if err := f.runtime.SuspendSandbox(ctx, "sbx_5"); !errors.Is(err, ErrUnsupported) {
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
	e, err := engine.Open(vmtest.FakeDriver(t, t.TempDir(), machineTestProject, nil), machineTestProject)
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
	t.Helper()
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
}

func (d *agentDriver) Start(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	m, err := d.Driver.Start(ctx, inst, opts)
	if err != nil {
		return nil, err
	}
	return &agentMachine{Machine: m, driver: d, inst: inst}, nil
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
	if !slices.Equal(req.Argv, []string{agentlayer.AgentPath, "provision", "apply"}) {
		return m.Machine.Exec(ctx, req)
	}
	check := `root="$DISCO_VM_GUEST_ROOT"; test -x "$root` + agentlayer.AgentPath + `" && test -f "$root/etc/discobox/sandbox.json" && cat "$root/etc/discobox/sandbox.json"`
	process, err := m.Machine.Exec(ctx, machine.ExecRequest{Argv: []string{"sh", "-c", check}})
	if err != nil {
		return nil, err
	}
	return &applyProcess{Process: process, machine: m}, nil
}

func (m *agentMachine) Shutdown(ctx context.Context, reboot bool) error {
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
	var cfg sandboxconfig.Config
	if err := json.Unmarshal([]byte(bootstrap.String()), &cfg); err != nil {
		return 1, err
	}
	p.machine.driver.agents.start(p.machine.inst.Name, cfg)
	return 0, nil
}

// fakeMachineAgents are the sandbox agents of a test's machines, by machine
// name.
type fakeMachineAgents struct {
	t       *testing.T
	poolKey ed25519.PublicKey
	mu      sync.Mutex
	servers map[string]*fakeMachineAgent
	// last is each sandbox's agent that ran last, kept after its machine
	// stops, so a test reads what it was told.
	last map[string]*fakeMachineAgent
}

func (a *fakeMachineAgents) start(name string, cfg sandboxconfig.Config) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if old := a.servers[name]; old != nil {
		old.close()
	}
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", cfg.AgentRuntime.ListenAddress)
	if err != nil {
		a.t.Errorf("fake agent of %s listens on %s: %v", name, cfg.AgentRuntime.ListenAddress, err)
		return
	}
	agent := &fakeMachineAgent{sandboxID: cfg.SandboxID, poolKey: a.poolKey}
	agent.server = &http.Server{Handler: agent, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = agent.server.Serve(listener) }()
	a.servers[name] = agent
	if a.last == nil {
		a.last = map[string]*fakeMachineAgent{}
	}
	a.last[cfg.SandboxID] = agent
}

func (a *fakeMachineAgents) stop(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if agent := a.servers[name]; agent != nil {
		agent.close()
		delete(a.servers, name)
	}
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
	for _, agent := range a.servers {
		agent.close()
	}
}

// fakeMachineAgent answers the routes the pool calls, each on a token the
// pool signed for its scope alone.
type fakeMachineAgent struct {
	sandboxID string
	poolKey   ed25519.PublicKey
	server    *http.Server

	mu        sync.Mutex
	delivered []int64
	clears    int
}

func (a *fakeMachineAgent) close() { _ = a.server.Close() }

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
	f := newMachineFixture(t)
	ctx := context.Background()
	if _, err := f.runtime.CreateSandbox(ctx, machineCreate("sbx_kept")); err != nil {
		t.Fatal(err)
	}
	e := &engine.Engine{Driver: f.owner, Project: machineTestProject}
	for _, name := range []string{"sbx-orphan", "sbx-held", "pool-own-machine"} {
		if _, err := e.Create(ctx, vmtest.Base, engine.CreateOptions{Name: name}); err != nil {
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
	f := newMachineFixture(t)
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
	f := newMachineFixture(t)
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
