package discovm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/vm/pkg/engine"
	"github.com/discobox-ai/vm/pkg/machine"
	"github.com/discobox-ai/vm/pkg/machine/fake"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/imagecache"
	poolagent "github.com/discobox-ai/discobox/pool-agent"
	"github.com/discobox-ai/discobox/pool-agent/bundle"
	"github.com/discobox-ai/discobox/pool-agent/provisioner"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

// poolTestDriver is the fake driver as a remote driver's pool machine sees
// one: it isolates and places machines on networks, which the fake driver
// cannot, so what was asked is recorded and the rest made as the fake makes
// it; and its machines answer the two commands that make a pool's box. A fake
// guest runs a command on the host, so the agent placed in it has nothing to
// run as: `provision identity` and `provision apply` are answered here, once
// the agent, its pins, the bootstrap, the key and the pool bundle's placed
// blob are checked to be in the guest.
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
	// unitsApplied counts the pool-agent feature's units step run in the
	// machines after an apply: the box's, which restarts or hands over what
	// moved. handOver is whether the last was asked to hand over.
	unitsApplied int
	handOver     bool
	// issueErr, when set, is what IssueKey answers.
	issueErr error
	// checkErr, when set, is what Check answers: an account the driver
	// refuses, or its service not answering. checks counts the calls.
	checkErr error
	checks   int
	// maxMachines is the machine limit the driver's provider holds, which
	// Capabilities reports only once a Check has read it, as boxd's does;
	// read is what the last Check that answered read.
	maxMachines map[machine.OS]int
	read        map[machine.OS]int
	// name, when set, is the driver's name: boxd's, for a pool that
	// authenticates in-machine, which the fake driver's configuration
	// cannot ask for.
	name string
	// applyFails makes apply exit non-zero.
	applyFails bool
	// noSystemd makes the machines' base one without systemd.
	noSystemd bool
	// lookupErr, when set, is what Instance and Instances answer: the
	// driver's service not answering.
	lookupErr error
	// serviceErrs, while any are left, is what each State and Attach
	// answers in turn, a nil passing it on: the driver's service failing one
	// call of several. Both are calls to it: the engine's state check is
	// State, and its guest is an Attach.
	serviceErrs []error
	// fences is each machine's isolation and networks, by ID, as Create was
	// asked for them: the fake driver under this one keeps neither.
	fences map[string]machine.InstanceSpec
	// keysNone is a driver that issues no keys at all, as archil.
	keysNone bool
	// noPorts is a driver whose guests' ports are not reached through a
	// shim on this host, as a remote driver's (archil's): with keysNone, it
	// is not a held driver, whose pool machine is fenced (held).
	noPorts bool
	// noIngress is a driver that gives no instance an ingress URL, so its
	// caller cannot reach the machine.
	noIngress bool
	// edge is a driver that fences at its provider's edge, as archil
	// reports itself: no networks, nothing of the account in a machine, a
	// fence that closes DNS, and URLs that answer only a credential (ADR
	// 26-11-16-619 §1). Without it, the driver fences on networks, as boxd.
	// Each of its Service calls mints a credential, counted in minted.
	edge   bool
	minted int
	// startDelay is how long a boot takes, Start's or a create's asked to
	// start (spec.Start), before the machine boots, and startBegan and
	// startEnded when the last one did: a boot that outlasts a progress
	// heartbeat.
	startDelay             time.Duration
	startBegan, startEnded time.Time
	// startReport, when set, is called with the boot's context as it begins:
	// a driver reporting from inside the phase held around it.
	startReport func(context.Context)
	// starts counts Start's calls, and startedCreates the creates asked to
	// start.
	starts, startedCreates int
}

func (d *poolTestDriver) Name() string {
	if d.name != "" {
		return d.name
	}
	return d.Driver.Name()
}

func (d *poolTestDriver) Capabilities() machine.Capabilities {
	caps := d.Driver.Capabilities()
	caps.Bare, caps.Networks = machine.BareStrip, true
	caps.Egress = []machine.EgressFence{machine.EgressNone, machine.EgressAllow}
	if d.edge {
		caps.Bare, caps.Networks = machine.BareAlways, false
		caps.Egress = append(caps.Egress, machine.EgressCloseDNS)
		caps.URLs.Credential = true
	}
	if d.noIngress {
		caps.URLs.Ingress = machine.URLNever
	}
	if d.keysNone {
		caps.Keys.Mode = machine.KeysNone
	}
	if d.noPorts {
		caps.Ports = false
	}
	d.mu.Lock()
	caps.MaxMachines = maps.Clone(d.read)
	d.mu.Unlock()
	return caps
}

// Service is the fake's, with a credential minted for it on an edge driver.
func (d *poolTestDriver) Service(ctx context.Context, inst machine.Instance) (machine.Service, error) {
	service, err := d.Driver.Service(ctx, inst)
	if err != nil || !d.edge {
		return service, err
	}
	d.mu.Lock()
	d.minted++
	token := fmt.Sprintf("edge-token-%d", d.minted)
	d.mu.Unlock()
	service.Credential = &machine.Credential{Header: testEdgeHeader, Token: token, Expires: time.Now().Add(24 * time.Hour)}
	return service, nil
}

func (d *poolTestDriver) mints() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.minted
}

func (d *poolTestDriver) Check(ctx context.Context) error {
	d.mu.Lock()
	d.checks++
	err := d.checkErr
	d.mu.Unlock()
	if err != nil {
		return err
	}
	if err := d.Driver.Check(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	d.read = maps.Clone(d.maxMachines)
	d.mu.Unlock()
	return nil
}

func (d *poolTestDriver) Create(ctx context.Context, spec machine.InstanceSpec) (machine.Instance, error) {
	d.mu.Lock()
	d.created = append(d.created, spec)
	if spec.Start != nil {
		d.startedCreates++
	}
	d.mu.Unlock()
	fence := machine.InstanceSpec{Bare: spec.Bare, Networks: spec.Networks}
	spec.Bare, spec.Networks = false, nil
	if spec.Start != nil {
		if err := d.boot(ctx); err != nil {
			return machine.Instance{}, err
		}
	}
	inst, err := d.Driver.Create(ctx, spec)
	if spec.Start != nil {
		d.booted()
	}
	if err != nil {
		return inst, err
	}
	d.mu.Lock()
	if d.fences == nil {
		d.fences = map[string]machine.InstanceSpec{}
	}
	d.fences[inst.ID] = fence
	d.mu.Unlock()
	return d.fenced(inst), nil
}

// fenced is inst with the isolation and networks it was made with, as a
// driver that keeps them reports them.
func (d *poolTestDriver) fenced(inst machine.Instance) machine.Instance {
	d.mu.Lock()
	defer d.mu.Unlock()
	if fence, ok := d.fences[inst.ID]; ok {
		inst.Bare, inst.Networks = fence.Bare, fence.Networks
	}
	return inst
}

func (d *poolTestDriver) Instance(ctx context.Context, ref string) (machine.Instance, error) {
	if err := d.lookup(); err != nil {
		return machine.Instance{}, err
	}
	inst, err := d.Driver.Instance(ctx, ref)
	if err != nil {
		return inst, err
	}
	return d.fenced(inst), nil
}

func (d *poolTestDriver) lookup() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lookupErr
}

func (d *poolTestDriver) Instances(ctx context.Context) ([]machine.Instance, error) {
	if err := d.lookup(); err != nil {
		return nil, err
	}
	all, err := d.Driver.Instances(ctx)
	for i := range all {
		all[i] = d.fenced(all[i])
	}
	return all, err
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

// boot begins a boot, Start's or a started create's: it reports, and takes
// startDelay.
func (d *poolTestDriver) boot(ctx context.Context) error {
	d.mu.Lock()
	delay, report := d.startDelay, d.startReport
	d.startBegan = time.Now()
	d.mu.Unlock()
	if report != nil {
		report(ctx)
	}
	if delay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil
}

// booted ends the boot boot began.
func (d *poolTestDriver) booted() {
	d.mu.Lock()
	d.startEnded = time.Now()
	d.mu.Unlock()
}

func (d *poolTestDriver) Start(ctx context.Context, inst machine.Instance, opts machine.BootOptions) (machine.Machine, error) {
	d.mu.Lock()
	d.starts++
	d.mu.Unlock()
	if err := d.boot(ctx); err != nil {
		return nil, err
	}
	m, err := d.Driver.Start(ctx, inst, opts)
	d.booted()
	if err != nil {
		return nil, err
	}
	return &poolTestMachine{Machine: m, driver: d}, nil
}

// nextServiceErr is the next of serviceErrs, nil once none are left.
func (d *poolTestDriver) nextServiceErr() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var next error
	if len(d.serviceErrs) > 0 {
		next, d.serviceErrs = d.serviceErrs[0], d.serviceErrs[1:]
	}
	return next
}

func (d *poolTestDriver) State(ctx context.Context, inst machine.Instance) (machine.State, error) {
	if err := d.nextServiceErr(); err != nil {
		return "", err
	}
	return d.Driver.State(ctx, inst)
}

func (d *poolTestDriver) Attach(ctx context.Context, inst machine.Instance) (machine.Machine, error) {
	if err := d.nextServiceErr(); err != nil {
		return nil, err
	}
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
	if slices.Equal(req.Argv, provisioner.SystemdCheck) {
		// The host's systemd is not the machine's, either: the base has it
		// unless the test says otherwise.
		code := "0"
		if m.driver.noSystemd {
			code = "1"
		}
		return m.Machine.Exec(ctx, machine.ExecRequest{Argv: []string{"sh", "-c", "exit " + code}})
	}
	if len(req.Argv) == 3 && req.Argv[0] == "rm" && req.Argv[1] == "-f" {
		// A fake guest's command runs on the host: the path is its root's.
		return m.Machine.Exec(ctx, machine.ExecRequest{Argv: []string{"sh", "-c", `rm -f "$DISCO_VM_GUEST_ROOT$0"`, req.Argv[2]}})
	}
	if len(req.Argv) >= 3 && req.Argv[0] == "sh" && strings.Contains(req.Argv[2], bundle.UnitsStep) {
		// The bundle's step, which the fake guest has none of: what is
		// asked is counted, and answered.
		m.driver.mu.Lock()
		m.driver.unitsApplied++
		m.driver.handOver = slices.Contains(req.Env, bundle.HandOverEnv+"=1")
		m.driver.mu.Unlock()
		return m.Machine.Exec(ctx, machine.ExecRequest{Argv: []string{"true"}})
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
		if len(req.Argv) != 4 || req.Argv[3] != "--wait" {
			return m.Machine.Exec(ctx, machine.ExecRequest{Argv: []string{"sh", "-c", "echo apply was not asked to wait >&2; exit 2"}})
		}
		check += ` && test -f "$root` + bundle.BootstrapFile + `"`
		if m.driver.name == "" {
			// A pool that authenticates in-machine (a test driver named as
			// boxd) is given no key file; any other is.
			check += ` && test -f "$root` + bundle.ProvisioningKeyFile + `"`
		}
		check += ` && test "$(cat "$root` + agentlayer.PlacedBlob(testPoolBlob) + `")" = "` + testPoolBlobData + `"`
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
		if err != nil && file.path == bundle.ProvisioningKeyFile && p.machine.driver.name != "" {
			continue
		}
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
// digest, and which records what it enrolled, and what of the pool's host was
// saved on its row (the row's runtime state and host credential).
type testMaker struct {
	digest   string
	enrolled []string
	platform imagecache.Platform
	// row is the pool's row as RecordHost last saved it; rowAtEnroll is it as
	// the box made its identity, before its agent ran.
	row, rowAtEnroll model.Pool
}

func (m *testMaker) RecordHost(_ context.Context, runtimeState model.RawJSON, credential *model.PoolHostCredential) error {
	m.row.RuntimeState, m.row.HostCredential = slices.Clone(runtimeState), credential
	return nil
}

func (m *testMaker) Bootstrap(context.Context) (poolagent.Bootstrap, error) {
	return poolagent.Bootstrap{ProjectID: "project-1", ControlPlaneKey: "control-plane-key"}, nil
}

// testPoolBlob is the one blob of the test pool bundle, which the box is
// placed with: what of it the test reads is that it was placed, not what it
// is.
var testPoolBlob = "sha256:" + strings.Repeat("c", 64)

const testPoolBlobData = "pool bundle blob"

func (m *testMaker) Box(_ context.Context, platform imagecache.Platform) (sandbox.PoolBox, error) {
	m.platform = platform
	agentDigest := "sha256:" + strings.Repeat("b", 64)
	blobs := map[string][]byte{agentDigest: agentLayer(), testPoolBlob: []byte(testPoolBlobData)}
	return sandbox.PoolBox{
		Agent:      sandbox.BundlePin{Name: "discobox-agent", Digest: "sha256:" + strings.Repeat("a", 64)},
		AgentLayer: agentDigest,
		Bundles:    []sandbox.BundlePin{{Name: "discobox-pool", Digest: m.digest}},
		Blobs:      []string{testPoolBlob},
		OpenBlob: func(_ context.Context, digest string) (io.ReadCloser, int64, error) {
			data, ok := blobs[digest]
			if !ok {
				return nil, 0, fmt.Errorf("no blob %s", digest)
			}
			return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
		},
	}, nil
}

func (m *testMaker) Enroll(_ context.Context, publicKey string) error {
	m.enrolled = append(m.enrolled, publicKey)
	m.rowAtEnroll = m.row
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

// A pool's machine whose boot outlasts its deadline fails the pass with the
// pool's own error, not one the reconciler waits on: its create boots it, and
// a boot that does not finish is the guest's, as a start's is (REVIEW.md), so
// a machine that never boots is not made, waited on and removed at every
// pass. The create leaves no machine.
func TestFakePoolMachineWhoseBootOutlastsItsDeadlineIsNotWaitedOn(t *testing.T) {
	r, driver := newFakePool(t, "")
	driver.startDelay = time.Minute
	pool := &model.Pool{ID: "pool-never-boots", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("EnsurePool() with a boot past its deadline = %v, want the boot's deadline", err)
	}
	if errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Fatalf("EnsurePool() = %v, want the pool's error, not one the reconciler waits on", err)
	}
	if driver.startedCreates != 1 || driver.starts != 0 {
		t.Fatalf("%d started creates and %d starts, want the one create that boots it", driver.startedCreates, driver.starts)
	}
	if all, err := driver.Instances(context.Background()); err != nil || len(all) != 0 {
		t.Fatalf("instances = %v, %v; want none left", all, err)
	}
}

// A pool's machine on a driver that gives no ingress URL is refused when it
// is made, naming that URL: the server reaches the pool's agent there alone,
// and never at the pool URL its peers reach (disco-vm ADR 26-11-14-610).
func TestFakePoolMachineNeedsAnIngressURL(t *testing.T) {
	r, driver := newFakePool(t, "")
	driver.noIngress = true
	ctx := context.Background()
	pool := &model.Pool{ID: "pool_0123456789abcdef", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil)
	if err == nil || !strings.Contains(err.Error(), "ingress URL") || !errors.Is(err, machine.ErrUnsupported) {
		t.Fatalf("EnsurePool() on a driver with no ingress URL = %v, want it refused, naming the ingress URL", err)
	}
	if errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Fatalf("EnsurePool() = %v, want the pool's error, not one the reconciler waits on", err)
	}
	if len(driver.created) != 0 || driver.applies() != 0 {
		t.Fatalf("created %d machines and applied %d times, want none", len(driver.created), driver.applies())
	}
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
	const scope = "pool-0123456789abcdef"
	if len(driver.created) != 1 || driver.created[0].Name != scope || driver.created[0].Bare || !slices.Equal(driver.created[0].Networks, []string{scope}) || driver.created[0].URLs != (machine.URLRequests{Ingress: machine.URLRequest{Mode: machine.URLOn, Strict: true}}) {
		t.Fatalf("created %+v, want one machine named %s, not isolated, on network %s alone, with its ingress URL asked on, strictly", driver.created, scope, scope)
	}
	// Made running by its create, and not started again: no stop between
	// its create and its start (disco-vm ADR 26-11-23-458).
	if driver.created[0].Start == nil || driver.startedCreates != 1 || driver.starts != 0 {
		t.Fatalf("created %+v with %d started creates and %d starts, want one create asked to start and no Start", driver.created[0], driver.startedCreates, driver.starts)
	}
	if len(maker.enrolled) != 1 || maker.enrolled[0] != testPoolKey {
		t.Fatalf("enrolled %q, want the key the box made", maker.enrolled)
	}
	if driver.applies() != 1 {
		t.Fatalf("applied %d times, want once", driver.applies())
	}
	bootstrap := driver.applied[0]
	if bootstrap.Runtime != poolagent.RuntimeMachine || bootstrap.Driver != "fake" || bootstrap.MachinePool != scope || bootstrap.MachineNetwork != scope ||
		bootstrap.ControlPlaneURL != testControlPlaneURL || bootstrap.PoolID != pool.ID || bootstrap.ProjectID != "project-1" || bootstrap.ControlPlaneKey != "control-plane-key" ||
		!strings.HasPrefix(bootstrap.AgentListenURL, "http://") {
		t.Fatalf("bootstrap = %+v, want the machine runtime on the pool's scope and network, listening where the service does", bootstrap)
	}
	// Both of the machine's URLs, as its driver gives them: the fake
	// driver's machines share a loopback, so its pool URL is its ingress
	// one (ADR 26-10-29-481 §1).
	if !strings.HasPrefix(bootstrap.IngressURL, "http://127.0.0.1:") || bootstrap.PoolURL != bootstrap.IngressURL {
		t.Fatalf("bootstrap URLs = ingress %q pool %q, want the fake machine's loopback URL as both", bootstrap.IngressURL, bootstrap.PoolURL)
	}
	key := driver.lastKey(t)
	if !key.Issued || key.Driver != "fake" || key.Secret == "" || !strings.HasPrefix(key.Name, scope+"-") {
		t.Fatalf("provisioning key = %v issued %v, want one the driver issued for the pool", key, key.Issued)
	}
	record := recordOf(t, pool)
	if record.MachinePool != scope || record.Box.Agent == "" || record.Key == nil || record.Key.ID != key.ID || !record.Key.Issued {
		t.Fatalf("record = %+v, want the scope, the box and the key placed", record)
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

	// The pool agent is reached at the machine's ingress URL.
	lease, err := r.AcquirePoolAgentClient(ctx, pool)
	if err != nil {
		t.Fatalf("AcquirePoolAgentClient() error = %v", err)
	}
	inst, _ := r.host.(*poolMachine).instance(ctx, pool)
	if url, _ := r.engine.URL(ctx, inst, machine.URLIngress); lease.BaseURL != url.URL {
		t.Fatalf("lease base URL = %q, want the machine's %q", lease.BaseURL, url.URL)
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

// On a driver that fences at its provider's edge, the pool is carried (ADR
// 26-11-16-619 §1): its machine is made on no network, since such a driver
// has none, and its bootstrap names none for its sandboxes'; it carries no
// machine token, since the driver gives a machine nothing of the account. A
// machine under its name on a network is not the pool's.
func TestFakePoolMachineAtAnEdgeIsOnNoNetwork(t *testing.T) {
	r, driver := newFakePool(t, "")
	driver.edge = true
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-edge", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatalf("EnsurePool() on a driver fenced at its edge = %v", err)
	}
	if len(driver.created) != 1 || len(driver.created[0].Networks) != 0 || driver.created[0].Bare {
		t.Fatalf("created %+v, want one machine on no network, not stripped", driver.created)
	}
	if driver.applies() != 1 || driver.applied[0].MachineNetwork != "" || driver.applied[0].MachinePool != "pool-edge" {
		t.Fatalf("bootstrap = %+v, want the pool's scope and no network", driver.applied)
	}
	if credential := pool.HostCredential; credential == nil || credential.MachineToken {
		t.Fatalf("host credential = %+v, want no machine token on a driver that gives a machine none", credential)
	}

	host := r.host.(*poolMachine)
	inst, err := host.instance(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.engine.Remove(ctx, inst, true); err != nil {
		t.Fatal(err)
	}
	// Made by someone else on a driver with networks, then read as the edge
	// driver reads it.
	driver.edge = false
	if _, err := r.engine.Create(ctx, engine.CreateOptions{Name: "pool-edge", Networks: []string{"someone-else"}}); err != nil {
		t.Fatal(err)
	}
	driver.edge = true
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err == nil || !strings.Contains(err.Error(), "nothing is placed on it") {
		t.Fatalf("EnsurePool() over a machine on a network = %v, want it refused", err)
	}
}

// A pool machine its provider paused (Archil at a sandbox's session limit,
// here the fake driver's hook) is resumed at the pool's next reconcile, by
// the engine's Start, on the machine and disk it had (ADR 26-11-16-619 §5):
// the pool's own machine is never left paused.
func TestFakePoolMachinePausedByItsProviderIsResumed(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-paused", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	maker := newTestMaker()
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	inst, err := r.host.(*poolMachine).instance(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := fake.Pause(ctx, driver.Driver, inst); err != nil {
		t.Fatal(err)
	}
	if state := r.engine.State(ctx, inst); state != engine.Paused {
		t.Fatalf("pool machine after the provider's pause = %s, want paused", state)
	}
	// The pass's state read fails once, so the attach that asks again is
	// what finds it paused.
	driver.mu.Lock()
	driver.serviceErrs = []error{status.Error(codes.Unavailable, "the service dropped the call")}
	driver.mu.Unlock()
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	if state := r.engine.State(ctx, inst); state != engine.Running || len(driver.created) != 1 {
		t.Fatalf("pool machine after a reconcile = %s, %d made; want the one machine running", state, len(driver.created))
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
	// The fake driver's URLs are its shim's loopback port, which moves at
	// every start (disco-vm ADR 0005 §2), and the bootstrap names them, so
	// the started machine is placed again with its new ones. A driver whose
	// URLs stay put, as boxd's do, places nothing.
	if r.engine.State(ctx, inst) != engine.Running || begun != 1 || driver.applies() != 3 || len(driver.created) != 1 || pool.State != model.PoolStateRegistering {
		t.Fatalf("a stopped machine: state %s, begun %d, applies %d, created %d, pool %q; want it started in place, placed again with the URLs it moved to", r.engine.State(ctx, inst), begun, driver.applies(), len(driver.created), pool.State)
	}

	if err := r.engine.Remove(ctx, inst, true); err != nil {
		t.Fatal(err)
	}
	oldKey := recordOf(t, pool).Key.ID
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, begin); err != nil {
		t.Fatal(err)
	}
	ids := issuedKeys(t, r, pool)
	if len(driver.created) != 2 || driver.applies() != 4 || len(ids) != 1 || ids[0] == oldKey || recordOf(t, pool).Key.ID != ids[0] {
		t.Fatalf("a lost machine: created %d, applies %d, keys %v (old %s); want it made again with a new key alone", len(driver.created), driver.applies(), ids, oldKey)
	}
}

// Every pass that makes or starts the pool's machine, or may restart the
// agent in it, tells the reconcile when the agent started, and how: a made or
// started machine from just before, so its agent's first report counts even
// when the pass that settles is a later one (ADR 26-11-15-294 §1); an agent
// in a machine the pass did not create from just before the apply that may
// restart it, since the new agent's first report lands before the apply
// returns (ADR 26-11-15-602 §1). A pass that applies nothing tells nothing.
func TestFakePoolMachineReportsWhenItStartsTheAgent(t *testing.T) {
	r, driver := newFakePool(t, "")
	pool := &model.Pool{ID: "pool-7", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	type start struct {
		at   time.Time
		kind sandbox.AgentStartKind
		// applied is how many applies had run when the start was told.
		applied int
	}
	var starts []start
	ctx := sandbox.WithAgentStartReporter(context.Background(), func(at time.Time, kind sandbox.AgentStartKind) {
		starts = append(starts, start{at: at, kind: kind, applied: driver.applies()})
	})
	maker := newTestMaker()
	ensure := func(what string) (time.Time, time.Time, int) {
		t.Helper()
		starts = nil
		applies := driver.applies()
		before := time.Now()
		if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		return before, time.Now(), applies
	}
	during := func(s start, before, after time.Time) bool {
		return !s.at.Before(before) && !s.at.After(after)
	}

	before, after, _ := ensure("a new machine")
	if len(starts) != 1 || starts[0].kind != sandbox.AgentStartBox || !during(starts[0], before, after) || len(driver.created) != 1 {
		t.Fatalf("a new machine reported starts %+v, want one box start, during the pass", starts)
	}

	pool.SetState(model.PoolStateActive)
	ensure("an unmoved machine")
	if len(starts) != 0 {
		t.Fatalf("an unmoved machine reported starts %+v, want none", starts)
	}

	maker.digest = "sha256:" + strings.Repeat("2", 64)
	before, after, applies := ensure("moved bundles")
	if len(starts) != 1 || starts[0].kind != sandbox.AgentStartInPlace || !during(starts[0], before, after) ||
		starts[0].applied != applies || driver.applies() != applies+1 {
		t.Fatalf("moved bundles reported starts %+v with %d applies before the pass, want one in-place start before the apply", starts, applies)
	}

	inst, err := r.host.(*poolMachine).instance(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.engine.Stop(context.Background(), inst, time.Minute); err != nil {
		t.Fatal(err)
	}
	// The fake driver's URLs move at every start, so the started machine is
	// placed again and its agent restarted on the URLs it moved to: the
	// agent it booted with may be replaced, so the apply records an
	// in-place start after the box start.
	before, after, applies = ensure("a stopped machine")
	if len(starts) != 2 || starts[0].kind != sandbox.AgentStartBox || starts[1].kind != sandbox.AgentStartInPlace ||
		!during(starts[0], before, after) || !starts[1].at.After(starts[0].at) || !during(starts[1], before, after) ||
		starts[1].applied != applies || driver.applies() != applies+1 {
		t.Fatalf("a stopped machine reported starts %+v, want its box start and then an in-place start before its apply", starts)
	}

	// A running machine under the pool's name that the record does not name
	// is adopted, not made: it may already run an agent, so its start is an
	// in-place one, before it is placed again, never a box start.
	stale := slices.Clone(pool.RuntimeState)
	if err := r.engine.Remove(context.Background(), inst, true); err != nil {
		t.Fatal(err)
	}
	ensure("a lost machine")
	pool.RuntimeState = stale
	before, after, applies = ensure("an adopted machine")
	if len(starts) != 1 || starts[0].kind != sandbox.AgentStartInPlace || !during(starts[0], before, after) ||
		len(driver.created) != 2 || starts[0].applied != applies || driver.applies() != applies+1 {
		t.Fatalf("an adopted machine reported starts %+v with %d applies before the pass; want one in-place start, before it was placed again", starts, applies)
	}
}

// The apply records an in-place start only where it may restart an agent
// that was already running: never in a machine the pass created, and in one
// it booted only when no record says it was provisioned or something moved.
// A repair boots the machine and places it again with nothing moved, so it
// keeps its box start, and its booted agent's report counts.
func TestReplacesAgent(t *testing.T) {
	for _, c := range []struct {
		name                                 string
		created, started, provisioned, moved bool
		want                                 bool
	}{
		{name: "created", created: true, started: true, moved: true, want: false},
		{name: "running, moved", provisioned: true, moved: true, want: true},
		{name: "running, forced with nothing moved", provisioned: true, want: true},
		{name: "running, unprovisioned", want: true},
		{name: "booted, moved", started: true, provisioned: true, moved: true, want: true},
		{name: "booted, unprovisioned", started: true, want: true},
		{name: "booted, repaired with nothing moved", started: true, provisioned: true, want: false},
	} {
		if got := replacesAgent(c.created, c.started, c.provisioned, c.moved); got != c.want {
			t.Errorf("%s: replacesAgent = %v, want %v", c.name, got, c.want)
		}
	}
}

// A pass that makes the pool a new machine and stops before its record is
// saved — a server restarted mid-reconcile — leaves the row naming the machine
// that was lost. The next pass finds a machine under the same name, but not
// the one the record names, so it is placed and keyed as a new machine is,
// not taken for the one that was provisioned.
func TestFakePoolMachineReplacedUnderItsRecordIsPlacedAgain(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-6", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	maker := newTestMaker()
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	stale := slices.Clone(pool.RuntimeState)
	staleKey := recordOf(t, pool).Key.ID

	inst, err := r.host.(*poolMachine).instance(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.engine.Remove(ctx, inst, true); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	// The pass's record is lost: the row still says what the first machine
	// was given.
	pool.RuntimeState = stale
	applies := driver.applies()

	begun := 0
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, func(context.Context) error { begun++; return nil }); err != nil {
		t.Fatal(err)
	}
	replacement, err := r.host.(*poolMachine).instance(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	record := recordOf(t, pool)
	if len(driver.created) != 2 || begun != 1 || driver.applies() != applies+1 || record.Instance != replacement.ID {
		t.Fatalf("created %d, begun %d, applies %d (was %d), record instance %q (machine %q); want the machine there placed as a new one, nothing made",
			len(driver.created), begun, driver.applies(), applies, record.Instance, replacement.ID)
	}
	if ids := issuedKeys(t, r, pool); record.Key == nil || record.Key.ID == staleKey || !slices.Contains(ids, record.Key.ID) || slices.Contains(ids, staleKey) {
		t.Fatalf("keys %v, record key %+v (stale %s); want a new key placed and recorded, the lost machine's revoked", ids, record.Key, staleKey)
	}
	if pool.State != model.PoolStateRegistering {
		t.Fatalf("pool state = %q, want registering", pool.State)
	}
}

// A machine under the pool machine's name that is not the recorded one, and
// not on the pool's own network, is not the pool's: nothing is placed on it
// and no key is issued for it.
func TestFakePoolMachineReplacedOffThePoolsNetworkIsRefused(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-7", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	maker := newTestMaker()
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	host := r.host.(*poolMachine)
	inst, err := host.instance(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.engine.Remove(ctx, inst, true); err != nil {
		t.Fatal(err)
	}
	scope, err := machinePool(pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.engine.Create(ctx, engine.CreateOptions{Name: scope, Networks: []string{"someone-else"}}); err != nil {
		t.Fatal(err)
	}
	keys := issuedKeys(t, r, pool)
	applies := driver.applies()

	err = r.EnsurePool(ctx, nil, nil, pool, maker, nil)
	if err == nil || !strings.Contains(err.Error(), "not on the networks its pool's machine is made on") {
		t.Fatalf("ensure = %v, want the stranger refused", err)
	}
	if driver.applies() != applies || !slices.Equal(issuedKeys(t, r, pool), keys) {
		t.Fatalf("applies %d (was %d), keys %v (were %v): something was placed on a machine off the pool's network", driver.applies(), applies, issuedKeys(t, r, pool), keys)
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

// A driver that issues no keys at all, and does not authenticate a machine
// as itself, makes no pool machine: it would be billed and never keyed. The
// pool is blocked, saying so, before anything is made.
func TestFakePoolMachineIsNotMadeWhereNoKeyCouldReachIt(t *testing.T) {
	r, driver := newFakePool(t, "")
	driver.keysNone = true
	pool := &model.Pool{ID: "pool-keyless", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	err := r.EnsurePool(context.Background(), nil, nil, pool, newTestMaker(), nil)
	if err == nil || !strings.Contains(err.Error(), "blocked") || !errors.Is(err, machine.ErrNoKey) || len(driver.created) != 0 {
		t.Fatalf("EnsurePool() on a driver that issues no keys = %v, created %d; want it blocked before any machine", err, len(driver.created))
	}
}

// A driver that cannot issue a key, and does not authenticate a machine
// as itself, blocks the pool, saying so; the provider's own key is never
// placed (ADR 26-11-11-080 §9). An issue that fails otherwise is retried. A
// provider key an older server placed is taken off the machine.
func TestFakePoolMachineKeyWhenTheDriverCannotIssue(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-8", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })

	driver.issueErr = errors.New("service unavailable")
	err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil)
	if err == nil || !strings.Contains(err.Error(), "service unavailable") || driver.applies() != 0 {
		t.Fatalf("a failed issue = %v, applies %d; want it failed", err, driver.applies())
	}

	driver.issueErr = machine.ErrCannotIssue
	err = r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil)
	if err == nil || !strings.Contains(err.Error(), "blocked") || !strings.Contains(err.Error(), "never placed") || driver.applies() != 0 {
		t.Fatalf("no issue = %v, applies %d; want the pool blocked, saying the provider's key is never placed", err, driver.applies())
	}

	// A machine an older server put the provider's own key on.
	host := r.host.(*poolMachine)
	inst, err := host.instance(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := r.engine.Guest(ctx, inst)
	if err != nil {
		t.Fatal(err)
	}
	if err := poolruntime.PlaceBoxFiles(ctx, guest, poolruntime.BoxFile{Path: bundle.ProvisioningKeyFile, Data: []byte(`{"driver":"fake","secret":"the-providers-own"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := host.keys.ensure(ctx, pool, guest, &poolKeyRecord{IssuedAt: time.Now(), Fingerprint: "an-old-provider-key"}); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatalf("an older provider key's machine = %v, want blocked", err)
	}
	if _, err := readGuestFile(guest, bundle.ProvisioningKeyFile); err == nil {
		t.Fatal("the provider's key an older server placed stayed on the machine")
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

// A pool machine whose base carries no systemd is refused before anything of
// the pool is placed in it, naming what the base lacks (ADR 26-11-16-619 §4):
// the provider's base is its operator's to fix.
func TestFakePoolMachineWhoseBaseHasNoSystemd(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-11", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	driver.noSystemd = true
	err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil)
	if !errors.Is(err, provisioner.ErrNoSystemd) || !strings.Contains(err.Error(), provisioner.SystemdPath) {
		t.Fatalf("a base without systemd = %v, want it refused naming %s", err, provisioner.SystemdPath)
	}
	if len(driver.applied) != 0 {
		t.Fatal("a machine without systemd was applied")
	}
}

// A driver that cannot fence a pool's sandboxes, on networks or at its
// provider's edge, makes no pool machine.
func TestFakePoolMachineNeedsIsolation(t *testing.T) {
	r, _ := newFakePool(t, "")
	r.engine.Driver = r.engine.Driver.(*poolTestDriver).Driver
	r.host = fakePoolMachine(r.engine, "")
	err := r.EnsurePool(context.Background(), nil, nil, &model.Pool{ID: "pool-14"}, newTestMaker(), nil)
	if err == nil || !strings.Contains(err.Error(), "cannot carry a machine pool") || !strings.Contains(err.Error(), "its pools are refused") {
		t.Fatalf("EnsurePool() on a driver that cannot isolate = %v", err)
	}
}

// The driver checks its account before the pool's machine is made: an account
// it refuses makes no machine, and the pool's error says why; a service that
// does not answer the check is a host not reachable yet, which the reconciler
// waits on. A pool whose machine exists is checked again, for its driver's
// machine limit, once an interval has passed since its last check, and a
// check that fails there does not fail the pass (ADR 26-11-15-831 §1).
func TestFakePoolMachineChecksTheAccountBeforeItsMachine(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-15", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })

	driver.checkErr = status.Error(codes.PermissionDenied, "the account refuses this test")
	err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil)
	if err == nil || !strings.Contains(err.Error(), "refuses its account") || !strings.Contains(err.Error(), "the account refuses this test") || errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Fatalf("EnsurePool() on an account the driver refuses = %v, want the driver's reason, not a wait", err)
	}
	if len(driver.created) != 0 {
		t.Fatalf("created %+v on an account the driver refuses", driver.created)
	}

	driver.checkErr = status.Error(codes.Unavailable, "connection refused")
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); !errors.Is(err, sandbox.ErrPoolNotReachable) || !strings.Contains(err.Error(), "cannot be checked yet") {
		t.Fatalf("EnsurePool() while the check does not answer = %v, want ErrPoolNotReachable, not a refusal", err)
	}
	if len(driver.created) != 0 {
		t.Fatalf("created %+v while the check did not answer", driver.created)
	}

	driver.checkErr = nil
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatalf("EnsurePool() once the account passes = %v", err)
	}
	if len(driver.created) != 1 {
		t.Fatalf("created %d machines, want one", len(driver.created))
	}
	if driver.checks != 3 {
		t.Fatalf("checks = %d, want 3: the pass that made the machine checked once", driver.checks)
	}
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatalf("EnsurePool() again = %v", err)
	}
	if driver.checks != 3 {
		t.Fatalf("checks = %d, want 3: a pool checked within the interval is not checked again", driver.checks)
	}
	r.host.(*poolMachine).limitInterval = 0
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatalf("EnsurePool() once the interval has passed = %v", err)
	}
	if driver.checks != 4 {
		t.Fatalf("checks = %d, want 4: a pool whose interval has passed is checked again", driver.checks)
	}
	driver.checkErr = status.Error(codes.PermissionDenied, "the account refuses this test")
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatalf("EnsurePool() on a running pool whose check fails = %v, want the pool kept", err)
	}
	if len(driver.created) != 1 {
		t.Fatalf("created %d machines, want the one", len(driver.created))
	}
}

// The server reads the driver's machine limit at its Check, with the
// provider's key, and the pool's bootstrap carries it, so the agent reports
// it; a pass whose Check fails, or a server that has not checked since it
// started, places the limit recorded; a moved limit is placed again, and a
// driver that reports none places none (ADR 26-11-15-831 §§1–3). Check runs
// at most once an interval, and is not run again within it.
func TestFakePoolMachinePlacesItsDriversMachineLimit(t *testing.T) {
	r, driver := newFakePool(t, "")
	host := r.host.(*poolMachine)
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-15", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	maker := newTestMaker()
	lastPlaced := func() map[machine.OS]int {
		t.Helper()
		driver.mu.Lock()
		defer driver.mu.Unlock()
		if len(driver.applied) == 0 {
			t.Fatal("nothing was placed")
		}
		return driver.applied[len(driver.applied)-1].MaxMachines
	}
	recorded := func() map[machine.OS]int { return decodeRuntimeState(pool).MaxMachines }

	driver.maxMachines = map[machine.OS]int{machine.Linux: 4}
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	if want := map[machine.OS]int{machine.Linux: 4}; !maps.Equal(lastPlaced(), want) || !maps.Equal(recorded(), want) {
		t.Fatalf("placed %v, recorded %v; want the limit the create's check read, %v", lastPlaced(), recorded(), want)
	}
	if driver.checks != 1 {
		t.Fatalf("checks = %d, want the create's one", driver.checks)
	}

	// Within the interval the limit recorded is placed and the driver is
	// not asked again, even when its provider's limit moved.
	driver.maxMachines = map[machine.OS]int{machine.Linux: 9}
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	if driver.checks != 1 || driver.applies() != 1 || !maps.Equal(recorded(), map[machine.OS]int{machine.Linux: 4}) {
		t.Fatalf("checks %d, applies %d, recorded %v; want no check within the interval", driver.checks, driver.applies(), recorded())
	}
	host.limitInterval = 0

	// A check that fails keeps the recorded limit, and the pool as placed.
	driver.checkErr = status.Error(codes.Unavailable, "connection refused")
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatalf("EnsurePool() while the check does not answer = %v", err)
	}
	if driver.applies() != 1 || !maps.Equal(recorded(), map[machine.OS]int{machine.Linux: 4}) {
		t.Fatalf("applies %d, recorded %v; want the pool kept on its recorded limit", driver.applies(), recorded())
	}

	// A server that has not checked since it started reports none from its
	// driver; the recorded limit stands while its check fails.
	driver.mu.Lock()
	driver.read = nil
	driver.mu.Unlock()
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	if driver.applies() != 1 {
		t.Fatalf("applies %d; a restarted server whose check fails placed the pool again", driver.applies())
	}

	// A limit that moved is placed again, and the agent restarted to read it.
	driver.checkErr, driver.maxMachines = nil, map[machine.OS]int{machine.Linux: 6}
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	if want := map[machine.OS]int{machine.Linux: 6}; driver.applies() != 2 || !maps.Equal(lastPlaced(), want) || !maps.Equal(recorded(), want) {
		t.Fatalf("applies %d, placed %v, recorded %v; want the moved limit %v placed", driver.applies(), lastPlaced(), recorded(), want)
	}
	if len(driver.restarts) != 0 || driver.unitsApplied != 2 {
		t.Fatalf("restarts = %q, units applied %d; want the box's units applied after each apply, which restart what moved, and nothing restarted by the server", driver.restarts, driver.unitsApplied)
	}
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	if driver.applies() != 2 {
		t.Fatalf("applies %d; an unchanged limit placed the pool again", driver.applies())
	}

	// A check that answers with no limit places none.
	driver.maxMachines = nil
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	if driver.applies() != 3 || lastPlaced() != nil || recorded() != nil {
		t.Fatalf("applies %d, placed %v, recorded %v; want none placed or recorded", driver.applies(), lastPlaced(), recorded())
	}
	if !strings.Contains(string(pool.RuntimeState), `"instance"`) || strings.Contains(string(pool.RuntimeState), "maxMachines") {
		t.Fatalf("runtime state %s, want no maxMachines", pool.RuntimeState)
	}
}

// The runtime's check is the one a pool's machine is created after, and
// makes nothing: an account the driver refuses is a refusal, a service that
// does not answer is ErrPoolNotReachable, each saying what the pool's create
// would (ADR 26-11-15-089 §3).
func TestRuntimeCheckIsTheDriversCheckAndMakesNothing(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	if err := r.Check(ctx); err != nil || driver.checks != 1 {
		t.Fatalf("Check() = %v after %d driver checks, want ok after one", err, driver.checks)
	}

	driver.checkErr = status.Error(codes.PermissionDenied, "the account refuses this test")
	err := r.Check(ctx)
	if err == nil || !strings.Contains(err.Error(), "refuses its account") || !strings.Contains(err.Error(), "the account refuses this test") || errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Fatalf("Check() on an account the driver refuses = %v, want the driver's reason as a refusal", err)
	}

	driver.checkErr = status.Error(codes.Unavailable, "connection refused")
	if err := r.Check(ctx); !errors.Is(err, sandbox.ErrProviderNotReachable) || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("Check() while the service does not answer = %v, want ErrProviderNotReachable naming the error", err)
	}
	if len(driver.created) != 0 {
		t.Fatalf("a check created %+v", driver.created)
	}
}

// A provider check has no pool, so a driver's service that does not answer
// is named as the service, never as a pool agent; the check still reads as
// unreachable.
func TestRuntimeCheckUnreachableNamesTheServiceNotAPoolAgent(t *testing.T) {
	r, driver := newFakePool(t, "")
	driver.checkErr = status.Error(codes.Unavailable, "connection refused")
	err := r.Check(context.Background())
	if !errors.Is(err, sandbox.ErrProviderNotReachable) || errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Fatalf("Check() while the service does not answer = %v, want ErrProviderNotReachable and not ErrPoolNotReachable", err)
	}
	if strings.Contains(err.Error(), "pool agent") || !strings.Contains(err.Error(), "the "+driver.Name()+" driver's") {
		t.Fatalf("Check() while the service does not answer = %q, want the driver named and no pool agent", err)
	}
}

// A running pool is provisioned again when what the provider's configuration
// says of it moves — its control plane URL, its policy — not only its
// bundles, and its units applied after each apply, which hand its agent and
// proxy over to ones that read them at start (poolunits).
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
	if len(driver.restarts) != 0 || driver.unitsApplied != 2 || !driver.handOver {
		t.Fatalf("restarts = %q, units applied %d, hands over %v; want the box's units applied after each apply, asked to hand over", driver.restarts, driver.unitsApplied, driver.handOver)
	}
	host.policy = map[string]string{"DISCOBOX_SANDBOX_IDLE_TIMEOUT": "1h0m0s"}
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	if driver.applies() != 3 || driver.unitsApplied != 3 {
		t.Fatalf("a moved policy: applies %d, units applied %d; want it placed again, once", driver.applies(), driver.unitsApplied)
	}
	// Moved bundles restart what they move through apply, and nothing more.
	maker.digest = "sha256:" + strings.Repeat("4", 64)
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	if driver.applies() != 4 || driver.unitsApplied != 4 || len(driver.restarts) != 0 {
		t.Fatalf("moved bundles: applies %d, units applied %d, restarts %q; want applied once more", driver.applies(), driver.unitsApplied, driver.restarts)
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

// A driver that authenticates a machine as itself (boxd, vm#109) has its
// pool's machine given nothing: no key, issued or the provider's, and no key
// file; its bootstrap turns the driver's in-machine field on and names no
// key (ADR 26-11-11-080 §9).
func TestFakePoolMachineAuthenticatesInMachine(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-17", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	host := r.host.(*poolMachine)
	driver.name = "boxd"
	host.keys.inMachineField = "inMachine"
	host.driverConfig = json.RawMessage(`{"region":"fra"}`)

	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatal(err)
	}
	if issued := issuedKeys(t, r, pool); len(issued) != 0 {
		t.Fatalf("issued %v for an in-machine pool, want none", issued)
	}
	if _, err := readGuestFile(poolGuest(t, r, pool), bundle.ProvisioningKeyFile); err == nil {
		t.Fatal("an in-machine pool's machine holds a key file")
	}
	if record := recordOf(t, pool).Key; record == nil || !record.InMachine || record.Issued {
		t.Fatalf("record %+v, want in-machine", record)
	}
	if credential := pool.HostCredential; credential == nil || credential.Key != model.PoolHostKeyInMachine {
		t.Fatalf("host credential = %+v, want in-machine", credential)
	}
	bootstrap := driver.applied[len(driver.applied)-1]
	var config map[string]any
	_ = json.Unmarshal(bootstrap.DriverConfig, &config)
	if config["inMachine"] != true || config["region"] != "fra" || len(config) != 2 {
		t.Fatalf("bootstrap's driver configuration = %s; want the driver's in-machine field on beside the provider's", bootstrap.DriverConfig)
	}
}

// A pool an older server keyed keeps its key until the placement that turns
// in-machine auth on has applied: its running agent reads the key file on
// every call. A placement that does not apply leaves the key, its file and
// its record as they were. Once it applies the file is taken off and every
// key issued for the pool revoked, so one read from the machine's disk stops
// working now (ADR 26-11-11-080 §9).
func TestFakePoolMachineMovesToInMachineAfterItsPlacementApplies(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-18", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	host := r.host.(*poolMachine)
	driver.name = "boxd"
	host.driverConfig = json.RawMessage(`{"region":"fra"}`)
	maker := newTestMaker()
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	keyed := recordOf(t, pool).Key
	if keyed == nil || !keyed.Issued || len(issuedKeys(t, r, pool)) != 1 {
		t.Fatalf("the older server's pool = %+v, want an issued key", keyed)
	}

	// The server now authenticates the driver's pools in-machine.
	host.keys.inMachineField = "inMachine"
	driver.applyFails = true
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatalf("a running pool whose in-machine placement does not apply = %v, want it kept", err)
	}
	if record := recordOf(t, pool).Key; record == nil || record.ID != keyed.ID || record.InMachine {
		t.Fatalf("record after a failed apply = %+v, want the issued key's", record)
	}
	if guestKey(t, r, pool).ID != keyed.ID || len(issuedKeys(t, r, pool)) != 1 {
		t.Fatal("the key the running agent reads was taken before its in-machine placement applied")
	}
	// While the switch has not applied, the key it reads is still renewed.
	host.keys.now = func() time.Time { return time.Now().Add(poolKeyTTL*3/4 - time.Hour) }
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	renewed := recordOf(t, pool).Key
	if renewed == nil || !renewed.Issued || renewed.ID == keyed.ID || guestKey(t, r, pool).ID != renewed.ID {
		t.Fatalf("record past half-life while the switch has not applied = %+v, want a renewed key placed", renewed)
	}
	host.keys.now = time.Now

	driver.applyFails = false
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	if record := recordOf(t, pool).Key; record == nil || !record.InMachine {
		t.Fatalf("record = %+v, want in-machine", record)
	}
	if _, err := readGuestFile(poolGuest(t, r, pool), bundle.ProvisioningKeyFile); err == nil {
		t.Fatal("the key an older server placed stayed on an in-machine pool's machine")
	}
	if issued := issuedKeys(t, r, pool); len(issued) != 0 {
		t.Fatalf("keys = %v after the move to in-machine, want every one revoked", issued)
	}
	var config map[string]any
	_ = json.Unmarshal(driver.applied[len(driver.applied)-1].DriverConfig, &config)
	if config["inMachine"] != true {
		t.Fatalf("the applied bootstrap's driver configuration = %v, want in-machine on", config)
	}

	// A record lost says nothing of what the machine holds: whatever an older
	// server left there is taken off once the placement applies.
	if err := poolruntime.PlaceBoxFiles(ctx, poolGuest(t, r, pool), poolruntime.BoxFile{Path: bundle.ProvisioningKeyFile, Data: []byte(`{"driver":"boxd","secret":"bxd_old"}`)}); err != nil {
		t.Fatal(err)
	}
	state := recordOf(t, pool)
	state.Key = nil
	if err := host.writeState(pool, state); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsurePool(ctx, nil, nil, pool, maker, nil); err != nil {
		t.Fatal(err)
	}
	if record := recordOf(t, pool).Key; record == nil || !record.InMachine {
		t.Fatalf("record after a lost one = %+v, want in-machine", record)
	}
	if _, err := readGuestFile(poolGuest(t, r, pool), bundle.ProvisioningKeyFile); err == nil {
		t.Fatal("a key an older server placed stayed on the machine after its record was lost")
	}

	// The provider's own key, which an older server placed, is taken off too.
	if err := poolruntime.PlaceBoxFiles(ctx, poolGuest(t, r, pool), poolruntime.BoxFile{Path: bundle.ProvisioningKeyFile, Data: []byte(`{"driver":"boxd","secret":"bxd_old"}`)}); err != nil {
		t.Fatal(err)
	}
	record, err := host.keys.retire(ctx, pool, poolGuest(t, r, pool), &poolKeyRecord{IssuedAt: time.Now(), Fingerprint: "an-old-provider-key"})
	if err != nil || record == nil || !record.InMachine {
		t.Fatalf("retiring the provider's key = %+v, %v; want in-machine", record, err)
	}
	if _, err := readGuestFile(poolGuest(t, r, pool), bundle.ProvisioningKeyFile); err == nil {
		t.Fatal("the provider's key an older server placed stayed on the machine")
	}
}

// poolGuest is the pool's machine's guest.
func poolGuest(t *testing.T, r *Runtime, pool *model.Pool) machine.Guest {
	t.Helper()
	inst, err := r.host.(*poolMachine).instance(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	guest, err := r.engine.Guest(context.Background(), inst)
	if err != nil {
		t.Fatal(err)
	}
	return guest
}

// A record from before the box's record held its placement kept it beside
// the box; it is read so that pool is not placed again over nothing.
func TestDecodeRuntimeStateReadsAPlacementKeptBesideTheBox(t *testing.T) {
	pool := &model.Pool{RuntimeState: []byte(`{"instance":"i-1","box":{"agent":"discobox-agent@sha256:a"},"placement":"fingerprint-1"}`)}
	state := decodeRuntimeState(pool)
	if state.Box.Placement != "fingerprint-1" || state.Box.Agent != "discobox-agent@sha256:a" || state.Instance != "i-1" {
		t.Fatalf("state = %+v, want the placement kept beside the box read into it", state)
	}
	pool.RuntimeState = []byte(`{"instance":"i-1","box":{"placement":"fingerprint-2"},"placement":"fingerprint-1"}`)
	if state := decodeRuntimeState(pool); state.Box.Placement != "fingerprint-2" {
		t.Fatalf("placement = %q, want the box's own over the one beside it", state.Box.Placement)
	}
}

// A remote driver's service that does not answer when the pool's machine is
// looked up is a host not reachable yet, as it is when the machine's URL or
// state is asked for: the pool reconciler waits on it rather than recording a
// failure and repairing a pool for a network fault, and a create waiting for
// the pool's agent keeps waiting.
func TestFakePoolMachineLookupWhileTheServiceDoesNotAnswer(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1"}
	t.Cleanup(func() {
		driver.mu.Lock()
		driver.lookupErr = nil
		driver.mu.Unlock()
		_ = r.RemovePool(context.Background(), nil, nil, pool)
	})
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatalf("EnsurePool() error = %v", err)
	}

	driver.mu.Lock()
	driver.lookupErr = status.Error(codes.Unavailable, "connection error: desc = \"error reading from server: unexpected EOF\"")
	driver.mu.Unlock()

	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); !errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Errorf("EnsurePool() while the service does not answer = %v, want ErrPoolNotReachable", err)
	}
	if err := r.RepairPool(ctx, nil, nil, pool, newTestMaker(), "test", nil); !errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Errorf("RepairPool() while the service does not answer = %v, want ErrPoolNotReachable", err)
	}
	if _, err := r.AcquirePoolAgentClient(ctx, pool); !errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Errorf("AcquirePoolAgentClient() while the service does not answer = %v, want ErrPoolNotReachable", err)
	}
}

// A running pool whose driver's service answers the state check and drops on
// the next call, for its guest, is a host not reachable for a moment, as one
// that drops at the lookup is: one call to the service left unmapped would
// fail the reconcile and repair a pool with sandboxes.
func TestFakePoolMachineGuestWhileTheServiceDrops(t *testing.T) {
	r, driver := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1"}
	t.Cleanup(func() {
		driver.mu.Lock()
		driver.serviceErrs = nil
		driver.mu.Unlock()
		_ = r.RemovePool(context.Background(), nil, nil, pool)
	})
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatalf("EnsurePool() error = %v", err)
	}

	driver.mu.Lock()
	driver.serviceErrs = []error{nil, status.Error(codes.Unavailable, "connection error: desc = \"error reading from server: unexpected EOF\"")}
	driver.mu.Unlock()

	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); !errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Errorf("EnsurePool() while the service drops at the guest = %v, want ErrPoolNotReachable", err)
	}
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Errorf("EnsurePool() once the service is back = %v, want nil", err)
	}
}

// Removing a pool removes what its agent made in the pool's own disco-vm
// scope (machinePool): a committed snapshot, and a snapshot build still
// running, which no agent is left to remove once the pool's machine is gone.
func TestFakePoolMachineRemovalEmptiesThePoolsScope(t *testing.T) {
	r, _ := newFakePool(t, "")
	ctx := context.Background()
	pool := &model.Pool{ID: "pool_scope1", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(context.Background(), nil, nil, pool) })
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), func(context.Context) error { return nil }); err != nil {
		t.Fatalf("EnsurePool() error = %v", err)
	}

	// What the pool agent makes, in the pool's scope, on the same driver.
	scope, err := machinePool(pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := newDriver(fakeConfig(t), scope, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := engine.Open(driver, scope)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := agent.Create(ctx, engine.CreateOptions{Name: "snap-committed"})
	if err != nil {
		t.Fatalf("create the snapshot's machine: %v", err)
	}
	if _, err := agent.Commit(ctx, committed); err != nil {
		t.Fatalf("commit the snapshot: %v", err)
	}
	building, err := agent.Create(ctx, engine.CreateOptions{Name: "snap-building"})
	if err != nil {
		t.Fatalf("create the build's machine: %v", err)
	}
	if err := agent.Start(ctx, building, engine.StartOptions{}); err != nil {
		t.Fatalf("start the build's machine: %v", err)
	}
	t.Cleanup(func() {
		left, _ := agent.List(context.Background())
		for _, inst := range left {
			_ = agent.Remove(context.Background(), inst, true)
		}
	})

	if err := r.RemovePool(ctx, nil, nil, pool); err != nil {
		t.Fatalf("RemovePool() error = %v", err)
	}
	left, err := agent.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, inst := range left {
		names = append(names, fmt.Sprintf("%s (%s)", inst.Name, agent.State(ctx, inst)))
	}
	if len(names) > 0 {
		t.Fatalf("after RemovePool the pool's scope %s still holds %v, want nothing", scope, names)
	}
}

// progressPoolManager is the pool manager a pool's maker and progress
// reporter are made from, recording every provisioning phase reported.
type progressPoolManager struct {
	sandbox.PoolManager
	maker   *testMaker
	mu      sync.Mutex
	reports []progressReport
}

type progressReport struct {
	phase sandbox.PoolProvisionPhase
	at    time.Time
}

func (m *progressPoolManager) ReportPoolProvisionProgress(_ context.Context, _ string, progress sandbox.PoolProvisionProgress) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reports = append(m.reports, progressReport{phase: progress.Phase, at: time.Now()})
	return nil
}

// take answers the reports made since the last take, and forgets them.
func (m *progressPoolManager) take() []progressReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	reports := m.reports
	m.reports = nil
	return reports
}

func phasesOf(reports []progressReport) []sandbox.PoolProvisionPhase {
	phases := make([]sandbox.PoolProvisionPhase, 0, len(reports))
	for _, report := range reports {
		phases = append(phases, report.phase)
	}
	return slices.Compact(phases)
}

func (m *progressPoolManager) PoolBox(ctx context.Context, platform imagecache.Platform) (sandbox.PoolBox, error) {
	return m.maker.Box(ctx, platform)
}

func (m *progressPoolManager) EnsureAgentTrustKey(context.Context) (string, error) {
	return "control-plane-key", nil
}

func (m *progressPoolManager) EnrollPool(ctx context.Context, _, publicKey string) error {
	return m.maker.Enroll(ctx, publicKey)
}

func (m *progressPoolManager) RecordPoolHost(ctx context.Context, _ string, runtimeState model.RawJSON, credential *model.PoolHostCredential) error {
	return m.maker.RecordHost(ctx, runtimeState, credential)
}

func (m *progressPoolManager) ListPools(context.Context, string) ([]model.Pool, error) {
	return nil, errors.New("pool sync is outside this test")
}

// newProgressPool is a fake pool machine whose provider reports its progress
// to the manager it returns, as FactoryWithPoolManager's does.
func newProgressPool(t *testing.T) (*Runtime, *poolTestDriver, *progressPoolManager) {
	t.Helper()
	r, driver := newFakePool(t, "")
	manager := &progressPoolManager{maker: newTestMaker()}
	r.host.(*poolMachine).progress = sandbox.PoolProgressReporterFor(manager)
	return r, driver, manager
}

// A pool machine reports the phases the engine's do where its steps mean
// the same: making and starting the machine, then provisioning its box, whose
// apply returns once the agent it starts has reported. A pass on a running
// pool, its bundles moved or not, brings nothing up and says nothing.
func TestFakePoolMachineReportsProvisionProgress(t *testing.T) {
	r, _, manager := newProgressPool(t)
	ctx := context.Background()
	project := &model.Project{ID: "project-1"}
	pool := &model.Pool{ID: "pool-progress", ProjectID: project.ID}
	t.Cleanup(func() { _ = r.RemovePool(ctx, nil, nil, pool) })
	provider := poolruntime.New(r, Definition("fake"), manager, nil)
	instance := fakeInstance(t)
	reconcile := func() error { return provider.ReconcilePool(ctx, manager, project, instance, pool, nil) }
	bringUp := []sandbox.PoolProvisionPhase{sandbox.PoolPhaseStartingVM, sandbox.PoolPhaseStartingPoolAgent}
	for _, pass := range []struct {
		name string
		run  func() error
		want []sandbox.PoolProvisionPhase
	}{
		{"bring-up", reconcile, bringUp},
		{"repair", func() error {
			pool.SetState(model.PoolStateActive)
			return provider.RepairPool(ctx, manager, project, instance, pool, "test", nil)
		}, bringUp},
		{"current", reconcile, nil},
		{"moved bundles", func() error {
			manager.maker.digest = "sha256:" + strings.Repeat("2", 64)
			return reconcile()
		}, nil},
		// The fake driver's URLs move at every start, and the bootstrap
		// names them, so its started machine is placed again: a bring-up.
		// A driver whose URLs stay put (boxd's) finds the box current, and
		// reports waiting_for_pool_agent for the agent its boot started.
		{"stopped", func() error {
			inst, err := r.host.(*poolMachine).instance(ctx, pool)
			if err != nil {
				return err
			}
			if err := r.engine.Stop(ctx, inst, time.Minute); err != nil {
				return err
			}
			return reconcile()
		}, bringUp},
	} {
		if err := pass.run(); err != nil {
			t.Fatalf("%s: %v", pass.name, err)
		}
		if phases := phasesOf(manager.take()); !slices.Equal(phases, pass.want) {
			t.Errorf("%s: reported provisioning phases %v, want %v", pass.name, phases, pass.want)
		}
	}
}

// A machine still booting when the pass that made it reaches for its guest
// fails that pass as not reachable; the next finds it running with no box,
// and placing the pool's first agent there is still bringing it up.
func TestFakePoolMachineReportsTheFirstProvisionAfterABoot(t *testing.T) {
	r, driver, manager := newProgressPool(t)
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-booting", ProjectID: "project-1"}
	t.Cleanup(func() {
		driver.mu.Lock()
		driver.serviceErrs = nil
		driver.mu.Unlock()
		_ = r.RemovePool(ctx, nil, nil, pool)
	})
	driver.mu.Lock()
	// The pass's state check (State) and its start's attach pass; its reach
	// for the guest is refused.
	driver.serviceErrs = []error{nil, nil, status.Error(codes.Unavailable, "the machine is still booting")}
	driver.mu.Unlock()
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); !errors.Is(err, sandbox.ErrPoolNotReachable) {
		t.Fatalf("EnsurePool() while the machine boots = %v, want ErrPoolNotReachable", err)
	}
	if phases := phasesOf(manager.take()); !slices.Equal(phases, []sandbox.PoolProvisionPhase{sandbox.PoolPhaseStartingVM}) {
		t.Fatalf("the pass that made the machine reported %v, want [starting_vm]", phases)
	}
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatal(err)
	}
	if phases := phasesOf(manager.take()); !slices.Equal(phases, []sandbox.PoolProvisionPhase{sandbox.PoolPhaseStartingPoolAgent}) {
		t.Errorf("the pass that placed its first agent reported %v, want [starting_pool_agent]", phases)
	}
}

// A machine whose start outlasts a heartbeat keeps its line fresh while it
// boots, rather than leaving a stamp that reads as a pool that stopped, and
// the start runs under the context starting_vm's hold returns: what the
// driver reports from inside it, as a guest image fetch is, is what the
// heartbeat restates.
func TestFakePoolMachineHoldsStartingVMThroughALongBoot(t *testing.T) {
	if testing.Short() {
		t.Skip("boots past a progress heartbeat")
	}
	r, driver, manager := newProgressPool(t)
	ctx := context.Background()
	pool := &model.Pool{ID: "pool-long-boot", ProjectID: "project-1"}
	t.Cleanup(func() { _ = r.RemovePool(ctx, nil, nil, pool) })
	// Past the reporter's five-second heartbeat.
	driver.mu.Lock()
	driver.startDelay = 6 * time.Second
	driver.startReport = func(ctx context.Context) {
		r.host.(*poolMachine).progress.Report(ctx, pool.ID, sandbox.PoolPhaseFetchingVMImage)
	}
	driver.mu.Unlock()
	if err := r.EnsurePool(ctx, nil, nil, pool, newTestMaker(), nil); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	began, ended := driver.startBegan, driver.startEnded
	driver.mu.Unlock()
	var during []progressReport
	for _, report := range manager.take() {
		if report.at.After(began) && !report.at.After(ended) {
			during = append(during, report)
		}
	}
	// The driver's own report, then at least one restatement of it.
	if len(during) < 2 {
		t.Fatalf("reported %v during a %s boot, want the driver's report restated", during, ended.Sub(began).Round(time.Second))
	}
	for _, report := range during {
		if report.phase != sandbox.PoolPhaseFetchingVMImage {
			t.Fatalf("reported %q while the machine booted, want the driver's %q restated: the start did not run under the hold's context", report.phase, sandbox.PoolPhaseFetchingVMImage)
		}
	}
}
