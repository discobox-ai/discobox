package discovm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/imagecache"
	poolagent "github.com/discobox-ai/discobox/pool-agent"
	"github.com/discobox-ai/discobox/pool-agent/bundle"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// poolTestDriver is the fake driver as a remote driver's pool machine sees
// one: it isolates and places machines on networks, which the fake driver
// cannot, so what was asked is recorded and the rest made as the fake makes
// it; and its machines answer the two commands that make a pool's box. A fake
// guest runs a command on the host, so the agent placed in it has nothing to
// run as: `provision identity` and `provision apply` are answered here, once
// the agent, its pins, the bootstrap and the key are checked to be in the
// guest.
type poolTestDriver struct {
	machine.Driver

	mu sync.Mutex
	// created is every spec Create was asked for.
	created []machine.InstanceSpec
	// applied is every bootstrap an apply found, and keys every
	// provisioning key it found beside it.
	applied []poolagent.Bootstrap
	keys    []machine.Key
	// restarts is every systemctl command the machines were asked to run.
	restarts [][]string
	// issueErr, when set, is what IssueKey answers.
	issueErr error
	// applyFails makes apply exit non-zero.
	applyFails bool
}

func (d *poolTestDriver) Capabilities() machine.Capabilities {
	caps := d.Driver.Capabilities()
	caps.Isolation = true
	return caps
}

func (d *poolTestDriver) Create(ctx context.Context, spec machine.InstanceSpec) (machine.Instance, error) {
	d.mu.Lock()
	d.created = append(d.created, spec)
	d.mu.Unlock()
	spec.Isolated, spec.Networks = false, nil
	return d.Driver.Create(ctx, spec)
}

func (d *poolTestDriver) IssueKey(ctx context.Context, req machine.IssueRequest) (machine.Key, error) {
	d.mu.Lock()
	err := d.issueErr
	d.mu.Unlock()
	if err != nil {
		return machine.Key{}, err
	}
	return d.Driver.IssueKey(ctx, req)
}

func (d *poolTestDriver) Start(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	m, err := d.Driver.Start(ctx, inst, opts)
	if err != nil {
		return nil, err
	}
	return &poolTestMachine{Machine: m, driver: d}, nil
}

func (d *poolTestDriver) Attach(ctx context.Context, inst machine.Instance) (machine.Machine, error) {
	m, err := d.Driver.Attach(ctx, inst)
	if err != nil {
		return nil, err
	}
	return &poolTestMachine{Machine: m, driver: d}, nil
}

func (d *poolTestDriver) applies() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.applied)
}

// lastKey is the provisioning key the last check of the machine found.
func (d *poolTestDriver) lastKey(t *testing.T) machine.Key {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.keys) == 0 {
		t.Fatal("no provisioning key was found on the machine")
	}
	return d.keys[len(d.keys)-1]
}

// testPoolKey is the public key every test box makes.
const testPoolKey = "test-pool-public-key"

type poolTestMachine struct {
	machine.Machine
	driver *poolTestDriver
}

func (m *poolTestMachine) Exec(ctx context.Context, req machine.ExecRequest) (machine.Process, error) {
	if len(req.Argv) > 0 && req.Argv[0] == "systemctl" {
		// A fake guest's command runs on the host, whose systemd is not the
		// machine's: what was asked is recorded, and answered.
		m.driver.mu.Lock()
		m.driver.restarts = append(m.driver.restarts, req.Argv)
		m.driver.mu.Unlock()
		return m.Machine.Exec(ctx, machine.ExecRequest{Argv: []string{"true"}})
	}
	if len(req.Argv) == 3 && req.Argv[0] == "rm" && req.Argv[1] == "-f" {
		// A fake guest's command runs on the host: the path is its root's.
		return m.Machine.Exec(ctx, machine.ExecRequest{Argv: []string{"sh", "-c", `rm -f "$DISCO_VM_GUEST_ROOT$0"`, req.Argv[2]}})
	}
	if len(req.Argv) < 2 || req.Argv[0] != agentlayer.AgentPath || req.Argv[1] != "provision" {
		return m.Machine.Exec(ctx, req)
	}
	check := `root="$DISCO_VM_GUEST_ROOT"; test -x "$root` + agentlayer.AgentPath + `" && test -f "$root` + agentlayer.PinsFile + `"`
	switch req.Argv[2] {
	case "identity":
		return m.Machine.Exec(ctx, machine.ExecRequest{Argv: []string{"sh", "-c", check + " && echo " + testPoolKey}})
	case "apply":
		if m.driver.applyFails {
			return m.Machine.Exec(ctx, machine.ExecRequest{Argv: []string{"sh", "-c", "echo apply failed >&2; exit 3"}})
		}
		check += ` && test -f "$root` + bundle.BootstrapFile + `" && test -f "$root` + bundle.ProvisioningKeyFile + `"`
		process, err := m.Machine.Exec(ctx, machine.ExecRequest{Argv: []string{"sh", "-c", check}})
		if err != nil {
			return nil, err
		}
		return &applyCheck{Process: process, machine: m}, nil
	}
	return m.Machine.Exec(ctx, req)
}

// applyCheck is the check standing in for `apply`; once it passes, the
// bootstrap and key it found are recorded.
type applyCheck struct {
	machine.Process
	machine *poolTestMachine
}

func (p *applyCheck) Wait(stdout, stderr io.Writer) (int, error) {
	code, err := p.Process.Wait(stdout, stderr)
	if err != nil || code != 0 {
		return code, err
	}
	var bootstrap poolagent.Bootstrap
	var key machine.Key
	for _, file := range []struct {
		path string
		into any
	}{{bundle.BootstrapFile, &bootstrap}, {bundle.ProvisioningKeyFile, &key}} {
		data, err := readGuestFile(p.machine.Machine, file.path)
		if err != nil {
			return 1, err
		}
		if err := json.Unmarshal(data, file.into); err != nil {
			return 1, err
		}
	}
	p.machine.driver.mu.Lock()
	p.machine.driver.applied = append(p.machine.driver.applied, bootstrap)
	p.machine.driver.keys = append(p.machine.driver.keys, key)
	p.machine.driver.mu.Unlock()
	return 0, nil
}

// readGuestFile reads one file out of a guest.
func readGuestFile(g machine.Guest, path string) ([]byte, error) {
	rc, err := g.CopyFrom(context.Background(), path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	if _, err := tr.Next(); err != nil {
		return nil, err
	}
	return io.ReadAll(tr)
}

// guestKey is the provisioning key in the pool's machine now.
func guestKey(t *testing.T, r *Runtime, pool *model.Pool) machine.Key {
	t.Helper()
	inst, err := r.host.(*poolMachine).instance(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := r.engine.Guest(context.Background(), inst)
	if err != nil {
		t.Fatal(err)
	}
	data, err := readGuestFile(guest, bundle.ProvisioningKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	var key machine.Key
	if err := json.Unmarshal(data, &key); err != nil {
		t.Fatal(err)
	}
	return key
}

// testMaker is a PoolMaker whose box is an agent and one pool bundle at
// digest, and which records what it enrolled.
type testMaker struct {
	digest   string
	enrolled []string
	platform imagecache.Platform
}

func (m *testMaker) Bootstrap(context.Context) (poolagent.Bootstrap, error) {
	return poolagent.Bootstrap{ProjectID: "project-1", ControlPlaneKey: "control-plane-key"}, nil
}

func (m *testMaker) Box(_ context.Context, platform imagecache.Platform) (sandbox.PoolBox, error) {
	m.platform = platform
	layer := agentLayer()
	return sandbox.PoolBox{
		Agent:          sandbox.BundlePin{Name: "discobox-agent", Digest: "sha256:" + strings.Repeat("a", 64)},
		AgentLayer:     "sha256:" + strings.Repeat("b", 64),
		OpenAgentLayer: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(layer)), nil },
		Bundles:        []sandbox.BundlePin{{Name: "discobox-pool", Digest: m.digest}},
	}, nil
}

func (m *testMaker) Enroll(_ context.Context, publicKey string) error {
	m.enrolled = append(m.enrolled, publicKey)
	return nil
}

func newTestMaker() *testMaker { return &testMaker{digest: "sha256:" + strings.Repeat("1", 64)} }

// agentLayer is an agent bundle's layer: a gzipped tar holding the agent.
func agentLayer() []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	agent := "#!/bin/sh\n"
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "features/", Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "features/core/", Mode: 0o755})
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: agentlayer.AgentFile, Mode: 0o755, Size: int64(len(agent))})
	_, _ = tw.Write([]byte(agent))
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

// issuedKeys is the pool's keys the driver has issued, by ID.
func issuedKeys(t *testing.T, r *Runtime, pool *model.Pool) []string {
	t.Helper()
	keys, err := r.engine.Driver.IssuedKeys(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := keyPrefix(pool)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, key := range keys {
		if strings.HasPrefix(key.Name, prefix) {
			ids = append(ids, key.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

func recordOf(t *testing.T, pool *model.Pool) poolRuntimeState {
	t.Helper()
	var state poolRuntimeState
	if err := json.Unmarshal(pool.RuntimeState, &state); err != nil {
		t.Fatalf("RuntimeState %s: %v", pool.RuntimeState, err)
	}
	return state
}

// A new pool's machine is made from the base on the pool's network alone, not
// isolated; given a key issued for the pool, placed before anything else; and
// made the pool's box with a bootstrap naming its own engine. The pool
// records all of it, and shows the key its host holds.
func TestFakePoolMachineIsMadeThePoolsBox(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool_0123456789abcdef", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	maker := newTestMaker()
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatalf("EnsurePool() error = %v", err)
	}
	const project = "pool-0123456789abcdef"
	if len(driver.created) != 1 || driver.created[0].Name != project || driver.created[0].Isolated || !slices.Equal(driver.created[0].Networks, []string{project}) {
		t.Fatalf("created %+v, want one machine named %s, not isolated, on network %s alone", driver.created, project, project)
	}
	if len(maker.enrolled) != 1 || maker.enrolled[0] != testPoolKey {
		t.Fatalf("enrolled %q, want the key the box made", maker.enrolled)
	}
	if driver.applies() != 1 {
		t.Fatalf("applied %d times, want once", driver.applies())
	}
	bootstrap := driver.applied[0]
	if bootstrap.Runtime != poolagent.RuntimeMachine || bootstrap.Driver != "fake" || bootstrap.MachineProject != project || bootstrap.MachineNetwork != project ||
		bootstrap.ControlPlaneURL != testControlPlaneURL || bootstrap.PoolID != pool.ID || bootstrap.ProjectID != "project-1" || bootstrap.ControlPlaneKey != "control-plane-key" ||
		!strings.HasPrefix(bootstrap.AgentListenURL, "http://") {
		t.Fatalf("bootstrap = %+v, want the machine runtime on the pool's project and network, listening where the service does", bootstrap)
	}
	key := driver.lastKey(t)
	if !key.Issued || key.Driver != "fake" || key.Secret == "" || !strings.HasPrefix(key.Name, project+"-") {
		t.Fatalf("provisioning key = %v issued %v, want one the driver issued for the pool", key, key.Issued)
	}
	record := recordOf(t, pool)
	if record.Project != project || record.Box.Agent == "" || record.Key == nil || record.Key.ID != key.ID || !record.Key.Issued {
		t.Fatalf("record = %+v, want the project, the box and the key placed", record)
	}
	if strings.Contains(string(pool.RuntimeState), key.Secret) {
		t.Fatal("the pool's record holds the key's secret")
	}
	credential := pool.HostCredential
	if credential == nil || credential.Key != model.PoolHostKeyIssued || credential.KeyID != key.ID || credential.ExpiresAt == nil || !credential.MachineToken {
		t.Fatalf("host credential = %+v, want the issued key and the driver's machine token", credential)
	}
	if pool.State != model.PoolStateRegistering {
		t.Fatalf("pool state = %q, want registering", pool.State)
	}

	// The pool agent is reached at the machine's one URL.
	lease, err := r.AcquirePoolAgentClient(ctx, pool)
	if err != nil {
		t.Fatalf("AcquirePoolAgentClient() error = %v", err)
	}
	inst, _ := r.host.(*poolMachine).instance(ctx, pool)
	if url, _ := r.engine.URL(ctx, inst); lease.BaseURL != url {
		t.Fatalf("lease base URL = %q, want the machine's %q", lease.BaseURL, url)
	}

	// Deleting the pool removes its machine and revokes its keys.
	if err := r.RemovePool(ctx, nil, nil, pool); err != nil {
		t.Fatalf("RemovePool() error = %v", err)
	}
	if ids := issuedKeys(t, r, pool); len(ids) != 0 {
		t.Fatalf("keys %v outlived the pool", ids)
	}
	if pool.HostCredential != nil || pool.RuntimeState != nil {
		t.Fatalf("a removed pool kept %s and %+v", pool.RuntimeState, pool.HostCredential)
	}
}

// On every reconcile the provider compares its own record with what it wants
// (ADR 26-10-23-551 §1): nothing moved runs nothing, moved bundles are applied
// again in the running machine, a stopped machine is started on the disk it
// kept, and a machine lost outside discobox is made again, its old keys
// revoked.
func TestFakePoolMachineIsReadopted(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-5", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	maker := newTestMaker()
	begun := 0
	begin := func(context.Context) error { begun++; return nil }
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, begin); err != nil {
		t.Fatal(err)
	}
	first := recordOf(t, pool)

	pool.SetState(model.PoolStateActive)
	begun = 0
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, begin); err != nil {
		t.Fatal(err)
	}
	if begun != 0 || driver.applies() != 1 || pool.State != model.PoolStateActive || recordOf(t, pool).Key.ID != first.Key.ID {
		t.Fatalf("an unmoved pool: begun %d, applies %d, state %q; want nothing done", begun, driver.applies(), pool.State)
	}

	maker.digest = "sha256:" + strings.Repeat("2", 64)
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, begin); err != nil {
		t.Fatal(err)
	}
	if begun != 1 || driver.applies() != 2 || recordOf(t, pool).Box.Bundles[0] != "discobox-pool@"+maker.digest || len(driver.created) != 1 {
		t.Fatalf("moved bundles: begun %d, applies %d, record %+v; want them applied in the same machine", begun, driver.applies(), recordOf(t, pool).Box)
	}

	inst, err := r.host.(*poolMachine).instance(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.engine.Stop(ctx, inst, time.Minute); err != nil {
		t.Fatal(err)
	}
	pool.SetState(model.PoolStateActive)
	begun = 0
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, begin); err != nil {
		t.Fatal(err)
	}
	if r.engine.State(ctx, inst) != engine.Running || begun != 1 || driver.applies() != 2 || len(driver.created) != 1 || pool.State != model.PoolStateRegistering {
		t.Fatalf("a stopped machine: state %s, begun %d, applies %d, created %d, pool %q; want it started, nothing placed", r.engine.State(ctx, inst), begun, driver.applies(), len(driver.created), pool.State)
	}

	if err := r.engine.Remove(ctx, inst, true); err != nil {
		t.Fatal(err)
	}
	oldKey := recordOf(t, pool).Key.ID
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, begin); err != nil {
		t.Fatal(err)
	}
	ids := issuedKeys(t, r, pool)
	if len(driver.created) != 2 || driver.applies() != 3 || len(ids) != 1 || ids[0] == oldKey || recordOf(t, pool).Key.ID != ids[0] {
		t.Fatalf("a lost machine: created %d, applies %d, keys %v (old %s); want it made again with a new key alone", len(driver.created), driver.applies(), ids, oldKey)
	}
}

// A key is replaced by half its life, placed alone, with nothing restarted;
// the old one is left to expire rather than revoked, and one that has expired
// is swept.
func TestFakePoolMachineRenewsItsKey(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-6", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	maker := newTestMaker()
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	first := recordOf(t, pool).Key
	host := r.host.(*poolMachine)

	host.keys.now = func() time.Time { return time.Now().Add(poolKeyTTL / 4) }
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	if recordOf(t, pool).Key.ID != first.ID {
		t.Fatal("a key in its first half-life was replaced")
	}

	host.keys.now = func() time.Time { return time.Now().Add(poolKeyTTL*3/4 - time.Hour) }
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	second := recordOf(t, pool).Key
	if second.ID == first.ID || guestKey(t, r, pool).ID != second.ID || driver.applies() != 1 {
		t.Fatalf("renewal: key %s (was %s), machine holds %s, applies %d; want a new key placed alone", second.ID, first.ID, guestKey(t, r, pool).ID, driver.applies())
	}
	if ids := issuedKeys(t, r, pool); !slices.Contains(ids, first.ID) || !slices.Contains(ids, second.ID) {
		t.Fatalf("keys after renewal = %v, want the old left to expire beside the new", ids)
	}
	if pool.HostCredential.KeyID != second.ID {
		t.Fatalf("host credential names %s, want the renewed key", pool.HostCredential.KeyID)
	}

	// Past the first key's expiry, the next renewal sweeps it.
	host.keys.now = func() time.Time { return time.Now().Add(poolKeyTTL + time.Hour) }
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	if ids := issuedKeys(t, r, pool); slices.Contains(ids, first.ID) {
		t.Fatalf("keys = %v, want the expired one swept", ids)
	}
}

// A driver that cannot issue a key blocks the pool, saying so, unless the
// provider opts in to delivering its own; an issue that fails otherwise is
// retried, never answered with the provider's key.
func TestFakePoolMachineKeyWhenTheDriverCannotIssue(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-8", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	host := r.host.(*poolMachine)
	host.keys.own = machine.Key{Driver: "fake", Secret: "the-providers-own"}

	driver.issueErr = errors.New("service unavailable")
	host.keys.deliver = true
	err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil)
	if err == nil || !strings.Contains(err.Error(), "service unavailable") || driver.applies() != 0 {
		t.Fatalf("a failed issue with the opt-in = %v, applies %d; want it failed, not downgraded", err, driver.applies())
	}

	driver.issueErr = machine.ErrCannotIssue
	host.keys.deliver = false
	err = r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil)
	if err == nil || !strings.Contains(err.Error(), "blocked") || !strings.Contains(err.Error(), "deliverProviderKey") || driver.applies() != 0 {
		t.Fatalf("no issue and no opt-in = %v, applies %d; want the pool blocked, naming the opt-in", err, driver.applies())
	}

	host.keys.deliver = true
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatalf("no issue with the opt-in: %v", err)
	}
	if key := driver.lastKey(t); key.Issued || key.Secret != "the-providers-own" {
		t.Fatalf("placed %v, want the provider's own key", key)
	}
	if credential := pool.HostCredential; credential == nil || credential.Key != model.PoolHostKeyProvider || credential.ExpiresAt != nil {
		t.Fatalf("host credential = %+v, want the provider's own key shown", credential)
	}
	if strings.Contains(string(pool.RuntimeState), "the-providers-own") {
		t.Fatal("the pool's record holds the provider's key")
	}

	// A key the provider's configuration rotates is placed again.
	host.keys.own.Secret = "rotated"
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatal(err)
	}
	if guestKey(t, r, pool).Secret != "rotated" {
		t.Fatal("a rotated provider key was not placed")
	}
}

// Repair restarts the pool's machine in place, on the same disk, and
// provisions and enrolls it again whatever the record says.
func TestFakePoolMachineRepairKeepsTheMachine(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-2", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	maker := newTestMaker()
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatalf("EnsurePool() error = %v", err)
	}
	before, err := r.host.(*poolMachine).instance(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	key := recordOf(t, pool).Key.ID
	pool.SetState(model.PoolStateActive)
	if err := r.RepairPool(ctx, nil, nil, pool, maker, "test", nil); err != nil {
		t.Fatalf("RepairPool() error = %v", err)
	}
	after, err := r.host.(*poolMachine).instance(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != before.ID || r.engine.State(ctx, after) != engine.Running {
		t.Fatalf("repair replaced or stopped the machine: %s -> %s", before.ID, after.ID)
	}
	if driver.applies() != 2 || len(maker.enrolled) != 2 || recordOf(t, pool).Key.ID != key {
		t.Fatalf("repair: applies %d, enrolled %d, key %s (was %s); want it provisioned and enrolled again on the same key", driver.applies(), len(maker.enrolled), recordOf(t, pool).Key.ID, key)
	}
	if pool.State != model.PoolStateRegistering {
		t.Fatalf("pool state = %q after repair, want registering", pool.State)
	}
}

// A running pool whose moved bundles do not apply keeps what it last
// provisioned (ADR 26-10-24-953 §5); a new machine whose apply fails fails the
// reconcile, with what the command said.
func TestFakePoolMachineApplyThatFails(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-9", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	driver.applyFails = true
	err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil)
	if err == nil || !strings.Contains(err.Error(), "apply failed") {
		t.Fatalf("a new machine's failed apply = %v, want the error with what it said", err)
	}
	if recordOf(t, pool).Key == nil {
		t.Fatal("the key placed before the failed apply was not recorded")
	}
	driver.applyFails = false
	maker := newTestMaker()
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatalf("a retried apply = %v", err)
	}
	driver.applyFails = true
	maker.digest = "sha256:" + strings.Repeat("3", 64)
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatalf("a running pool's failed apply = %v, want it kept as it is", err)
	}
	if recordOf(t, pool).Box.Bundles[0] == "discobox-pool@"+maker.digest {
		t.Fatal("a failed apply was recorded as placed")
	}
}

// A driver that cannot isolate machines makes no pool machine.
func TestFakePoolMachineNeedsIsolation(t *testing.T) {
	r, _ := newFakePool(t, "")
	r.engine.Driver = r.engine.Driver.(*poolTestDriver).Driver
	r.host = fakePoolMachine(r.engine, "")
	err := r.EnsurePool(context.Background(), nil, nil, &model.Pool{ID: "pool-14"}, newTestMaker(), nil)
	if err == nil || !strings.Contains(err.Error(), "cannot isolate") {
		t.Fatalf("EnsurePool() on a driver that cannot isolate = %v", err)
	}
}

// A running pool is provisioned again when what the provider's configuration
// says of it moves — its control plane URL, its policy — not only its
// bundles, and its agent and proxy, which read them at start, are restarted.
func TestFakePoolMachineFollowsItsConfiguration(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-15", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	maker := newTestMaker()
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	host := r.host.(*poolMachine)
	host.controlPlaneURL = "https://discobox.example.com"
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	if driver.applies() != 2 || driver.applied[1].ControlPlaneURL != "https://discobox.example.com" {
		t.Fatalf("a moved control plane URL: applies %d; want it placed again", driver.applies())
	}
	if len(driver.restarts) != 1 || !slices.Equal(driver.restarts[0], []string{"systemctl", "try-restart", "discobox-pool-agent.service", "discobox-proxy.service"}) {
		t.Fatalf("restarts = %q, want the agent and proxy restarted once", driver.restarts)
	}
	host.policy = map[string]string{"DISCOBOX_SANDBOX_IDLE_TIMEOUT": "1h0m0s"}
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	if driver.applies() != 3 || len(driver.restarts) != 2 {
		t.Fatalf("a moved policy: applies %d, restarts %d; want it placed again and the agent restarted, once", driver.applies(), len(driver.restarts))
	}
	// Moved bundles restart what they move through apply, and nothing more.
	maker.digest = "sha256:" + strings.Repeat("4", 64)
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	if driver.applies() != 4 || len(driver.restarts) != 2 {
		t.Fatalf("moved bundles: applies %d, restarts %d; want applied with no restart of its own", driver.applies(), len(driver.restarts))
	}
}

// A renewal that cannot be had leaves a working pool on the key it holds,
// rather than failing the reconcile that would repair it.
func TestFakePoolMachineKeepsItsKeyWhenRenewalFails(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-16", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatal(err)
	}
	first := recordOf(t, pool).Key.ID
	host := r.host.(*poolMachine)
	host.keys.now = func() time.Time { return time.Now().Add(poolKeyTTL * 3 / 4) }
	for _, err := range []error{machine.ErrCannotIssue, errors.New("service unavailable")} {
		driver.issueErr = err
		if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
			t.Fatalf("a failed renewal (%v) failed the pool: %v", driver.issueErr, err)
		}
		if recordOf(t, pool).Key.ID != first || guestKey(t, r, pool).ID != first {
			t.Fatalf("a failed renewal (%v) moved the key", driver.issueErr)
		}
	}
	// Past its expiry, there is nothing left to keep.
	host.keys.now = func() time.Time { return time.Now().Add(poolKeyTTL + time.Hour) }
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err == nil {
		t.Fatal("an expired key whose renewal failed was kept")
	}
}

// The provider's own key is asked to be replaced by an issued one daily, and
// withdrawing the opt-in takes it off the machine and blocks the pool.
func TestFakePoolMachineProviderKeyIsReplacedOrWithdrawn(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-17", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	host := r.host.(*poolMachine)
	host.keys.own = machine.Key{Driver: "fake", Secret: "the-providers-own"}
	host.keys.deliver = true
	driver.issueErr = machine.ErrCannotIssue
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatal(err)
	}

	// The driver can issue now, but it is not asked until a day has passed.
	driver.issueErr = nil
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatal(err)
	}
	if recordOf(t, pool).Key.Issued {
		t.Fatal("the provider's key was replaced before its recheck")
	}
	host.keys.now = func() time.Time { return time.Now().Add(providerKeyRecheck + time.Hour) }
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatal(err)
	}
	if key := guestKey(t, r, pool); !key.Issued || pool.HostCredential.Key != model.PoolHostKeyIssued {
		t.Fatalf("after the recheck the machine holds %v, want an issued key", key)
	}

	// Back to the provider's key, then the opt-in withdrawn.
	r.RemovePool(ctx, nil, nil, pool)
	host.keys.now = time.Now
	driver.issueErr = machine.ErrCannotIssue
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatal(err)
	}
	host.keys.deliver = false
	err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil)
	if err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("a withdrawn opt-in = %v, want the pool blocked", err)
	}
	inst, _ := host.instance(ctx, pool)
	guest, _ := r.engine.Guest(ctx, inst)
	if _, err := readGuestFile(guest, bundle.ProvisioningKeyFile); err == nil {
		t.Fatal("the provider's key stayed on the machine after the opt-in was withdrawn")
	}
	if recordOf(t, pool).Key != nil || pool.HostCredential == nil || pool.HostCredential.Key != "" {
		t.Fatalf("record %+v, credential %+v; want no key shown", recordOf(t, pool).Key, pool.HostCredential)
	}
}
