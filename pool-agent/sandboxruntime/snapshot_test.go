package sandboxruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pool-agent/proxyagent"

	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
)

func snapshotCreate(sandboxID string, platform string) *workerapimodel.PoolSandboxCreateRequest {
	return &workerapimodel.PoolSandboxCreateRequest{
		SandboxId: sandboxID,
		Bundles: workerclient.NewOptNilBundlePinArray([]workerapimodel.BundlePin{
			{Kind: workerclient.BundlePinKindPlatform, Name: "discobox-platform", Digest: "sha256:" + strings.Repeat(platform, 64)},
			{Kind: workerclient.BundlePinKindHarness, Name: "discobox-harness-shell", Digest: "sha256:" + strings.Repeat("b", 64)},
		}),
		ProvisionEgress: workerclient.NewOptEgressPolicy(workerclient.EgressPolicy{}),
	}
}

// A snapshot is keyed by what it holds depends on — the base, the pins, the
// features turned off, and the egress it is provisioned under — and by
// nothing of the sandbox whose create asked for it, so every sandbox of a
// plan shares one (ADR 26-10-16-423 §8).
func TestASnapshotIsKeyedByItsPlanAndNothingOfTheSandbox(t *testing.T) {
	r := &DockerSandboxRuntime{projectID: "proj_1", poolID: "pool_1"}
	key := func(base string, req *workerapimodel.PoolSandboxCreateRequest) string {
		t.Helper()
		plan, err := r.newSnapshotPlan(base, req)
		if err != nil {
			t.Fatal(err)
		}
		return plan.Key()
	}
	plain := key("sha256:base", snapshotCreate("sbx_1", "a"))

	other := snapshotCreate("sbx_2", "a")
	other.Egress = workerclient.NewOptEgressPolicy(workerclient.EgressPolicy{DenyByDefault: workerclient.NewOptBool(true)})
	other.SecretEnv = workerclient.NewOptNilPoolSandboxCreateRequestSecretEnv(workerclient.PoolSandboxCreateRequestSecretEnv{"TOKEN": "sentinel"})
	other.Config.Name = workerclient.NewOptString("another")
	if got := key("sha256:base", other); got != plain {
		t.Fatal("the sandbox's own ID, egress level, secrets or name changed its snapshot's key")
	}

	off := snapshotCreate("sbx_1", "a")
	off.Config.Features = workerclient.NewOptSandboxConfigFeatures(workerclient.SandboxConfigFeatures{"brew": {Enabled: false}})
	restricted := snapshotCreate("sbx_1", "a")
	restricted.ProvisionEgress = workerclient.NewOptEgressPolicy(workerclient.EgressPolicy{DenyByDefault: workerclient.NewOptBool(true)})
	for name, got := range map[string]string{
		"another base":             key("sha256:other", snapshotCreate("sbx_1", "a")),
		"another platform":         key("sha256:base", snapshotCreate("sbx_1", "c")),
		"a feature turned off":     key("sha256:base", off),
		"another provision egress": key("sha256:base", restricted),
	} {
		if got == plain {
			t.Errorf("%s: same key as the plain plan", name)
		}
	}
	on := snapshotCreate("sbx_1", "a")
	on.Config.Features = workerclient.NewOptSandboxConfigFeatures(workerclient.SandboxConfigFeatures{"brew": {Enabled: true}})
	if key("sha256:base", on) != plain {
		t.Error("a feature turned on, as every feature is unless turned off, changed the key")
	}
}

// snapshotDaemon answers inspects of one snapshot and of containers running
// it, and nothing else: no build it is asked for can succeed.
func snapshotDaemon(t *testing.T, snapshot, base string) *client.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.45")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.WriteHeader(http.StatusOK)
		case snapshot != "" && strings.HasSuffix(r.URL.Path, "/images/"+snapshot+"/json"),
			strings.HasSuffix(r.URL.Path, "/images/img_snap/json"):
			_, _ = w.Write([]byte(`{"Id":"img_snap","RepoTags":["` + snapshotRepository + `:k"],"Config":{"Labels":{"` + snapshotBaseLabel + `":"` + base + `"}}}`))
		case strings.HasSuffix(r.URL.Path, "/containers/ctr_snap/json"):
			_, _ = w.Write([]byte(`{"Id":"ctr_snap","Image":"img_snap","State":{"Status":"running"},"Config":{"Labels":{}}}`))
		case strings.HasSuffix(r.URL.Path, "/images/discobox-sandbox-base:test/json"):
			_, _ = w.Write([]byte(`{"Id":"sha256:base-v2"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	return cli
}

// A delivered create starts from its plan's snapshot when the pool has one
// built on the base it would run, and otherwise from the base, with the
// snapshot built behind it — once, and not again soon after it failed.
func TestADeliveredCreateStartsFromItsPlansSnapshot(t *testing.T) {
	req := snapshotCreate("sbx_1", "a")
	r := &DockerSandboxRuntime{root: withTestRoot(t), projectID: "proj_1", poolID: "pool_1"}
	plan, err := r.newSnapshotPlan("sha256:base", req)
	if err != nil {
		t.Fatal(err)
	}

	r.client = snapshotDaemon(t, plan.reference(), "sha256:base")
	if got := r.deliveredImage(context.Background(), "sha256:base", req); got != "img_snap" {
		t.Fatalf("started from %q, want the plan's snapshot", got)
	}
	// A create with no build policy, from a server before it, gets none.
	unpoliced := snapshotCreate("sbx_1", "a")
	unpoliced.ProvisionEgress = workerclient.OptEgressPolicy{}
	if got := r.deliveredImage(context.Background(), "sha256:base", unpoliced); got != "sha256:base" {
		t.Fatalf("a create with no provisionEgress started from %q, want the base image", got)
	}
	if len(r.snapshots.active) != 0 {
		t.Fatal("a create with no provisionEgress asked for a build")
	}

	// A snapshot of an older base is no snapshot of this plan's.
	r.client = snapshotDaemon(t, plan.reference(), "sha256:old")
	if got := r.deliveredImage(context.Background(), "sha256:base", req); got != "sha256:base" {
		t.Fatalf("started from %q, want the base image", got)
	}
	waitForSnapshotBuilds(t, r)
	r.snapshots.mu.Lock()
	_, failed := r.snapshots.failed[plan.Key()]
	r.snapshots.mu.Unlock()
	if !failed {
		t.Fatal("no build of the plan's snapshot was made, or it did not fail against a daemon that cannot build")
	}
	// Failed within snapshotRetryAfter: the next create starts from the base
	// and asks for no build.
	if got := r.deliveredImage(context.Background(), "sha256:base", req); got != "sha256:base" {
		t.Fatalf("started from %q, want the base image", got)
	}
	r.snapshots.mu.Lock()
	active := len(r.snapshots.active)
	r.snapshots.mu.Unlock()
	if active != 0 {
		t.Fatal("asked again for a build that failed moments ago")
	}
}

func waitForSnapshotBuilds(t *testing.T, r *DockerSandboxRuntime) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r.snapshots.mu.Lock()
		active := len(r.snapshots.active)
		r.snapshots.mu.Unlock()
		if active == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the snapshot build did not finish")
}

// A container started from a snapshot runs the base the snapshot was built
// from: its pins moving restarts it, and only a base moved past that
// rebuilds it.
func TestDeliveredMovedReadsTheBaseOfASnapshot(t *testing.T) {
	for name, tc := range map[string]struct {
		snapshotBase string
		want         deliveredMove
	}{
		"built on the current base": {"sha256:base-v2", movedPins},
		"built on an older base":    {"sha256:base-v1", movedBase},
	} {
		r := &DockerSandboxRuntime{root: withTestRoot(t), client: snapshotDaemon(t, "", tc.snapshotBase), projectID: "proj_1", poolID: "pool_1", sandboxBaseImage: "discobox-sandbox-base:test"}
		existing := &Sandbox{ID: "ctr_snap", SandboxID: "sbx_1", Status: StatusRunning}
		got, err := r.deliveredMoved(context.Background(), existing, snapshotCreate("sbx_1", "a"))
		if err != nil || got != tc.want {
			t.Errorf("%s: %v, %v; want %v", name, got, err, tc.want)
		}
	}
}

// buildDaemon plays a daemon a build runs against: the container it creates
// "provisions" when started, writing the result the agent would into the
// directory bound at /etc/discobox, and exits; what it is asked to commit is
// recorded.
type buildDaemon struct {
	t         *testing.T
	result    string
	mu        sync.Mutex
	created   container.CreateRequest
	placed    bool
	commitURL url.Values
	committed container.Config
	started   chan struct{}
}

func (d *buildDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Api-Version", "1.45")
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/_ping"):
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/containers/"):
		http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
	case strings.HasSuffix(r.URL.Path, "/containers/create"):
		_ = json.NewDecoder(r.Body).Decode(&d.created)
		_, _ = w.Write([]byte(`{"Id":"ctr_build"}`))
	case strings.HasSuffix(r.URL.Path, "/containers/ctr_build/archive"):
		_, _ = io.Copy(io.Discard, r.Body)
		d.placed = true
		w.WriteHeader(http.StatusOK)
	case strings.HasSuffix(r.URL.Path, "/containers/ctr_build/wait"):
		// The daemon answers at once and writes the exit when it comes,
		// which is what tells the client the wait is in place.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		d.mu.Unlock()
		<-d.started
		d.mu.Lock()
		_, _ = w.Write([]byte(`{"StatusCode":0}`))
	case strings.HasSuffix(r.URL.Path, "/containers/ctr_build/start"):
		etc := d.created.HostConfig.Mounts[0].Source
		if err := os.WriteFile(filepath.Join(etc, "snapshot-result.json"), []byte(d.result), 0o644); err != nil {
			d.t.Error(err)
		}
		close(d.started)
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(r.URL.Path, "/commit"):
		d.commitURL = r.URL.Query()
		_ = json.NewDecoder(r.Body).Decode(&d.committed)
		_, _ = w.Write([]byte(`{"Id":"img_new"}`))
	default:
		http.NotFound(w, r)
	}
}

func buildFixture(t *testing.T, result string) (*DockerSandboxRuntime, *buildDaemon, snapshotPlan) {
	t.Helper()
	daemon := &buildDaemon{t: t, result: result, started: make(chan struct{})}
	server := httptest.NewServer(daemon)
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("c", 64)
	r := &DockerSandboxRuntime{root: withTestRoot(t), client: cli, projectID: "proj_1", poolID: "pool_1",
		agentLayers: &fakeAgentLayers{digest: digest, layer: gzippedAgentLayer(t, "agent")}}
	req := snapshotCreate("sbx_1", "a")
	req.Config.Features = workerclient.NewOptSandboxConfigFeatures(workerclient.SandboxConfigFeatures{"brew": {Enabled: false}})
	plan, err := r.newSnapshotPlan("sha256:base", req)
	if err != nil {
		t.Fatal(err)
	}
	return r, daemon, plan
}

// A build boots the base with the agent placed and an anonymous plan bound
// at /etc/discobox, waits for it to power off, and commits it tagged by its
// key with the base, its agent layer and the reclaim label; then nothing of
// it is left (ADR 26-10-16-423 §8).
func TestASnapshotBuildCommitsTheBoxItProvisioned(t *testing.T) {
	r, daemon, plan := buildFixture(t, `{}`)
	var bound []byte
	daemonPlan := func() {
		etc := daemon.created.HostConfig.Mounts[0].Source
		bound, _ = os.ReadFile(filepath.Join(etc, "snapshot.json"))
	}
	go func() { <-daemon.started; daemon.mu.Lock(); daemonPlan(); daemon.mu.Unlock() }()
	if err := r.buildSnapshot(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	created := daemon.created
	if created.Image != "sha256:base" || created.Entrypoint[0] != "/sbin/init" || !created.HostConfig.Privileged {
		t.Errorf("build container = image %q, entrypoint %v, privileged %v", created.Image, created.Entrypoint, created.HostConfig.Privileged)
	}
	if len(created.Labels) != 2 || created.Labels[sandboxLabelProject] != "proj_1" || created.Labels[sandboxLabelPool] != "pool_1" {
		t.Errorf("build container labels = %v, want its project's and pool's alone", created.Labels)
	}
	if mounts := created.HostConfig.Mounts; len(mounts) != 1 || mounts[0].Target != agentlayer.SnapshotDir {
		t.Errorf("build container mounts = %+v, want the plan's directory alone", mounts)
	}
	if !daemon.placed {
		t.Error("the agent was not placed in the build container")
	}
	var doc agentlayer.SnapshotPlan
	if err := json.Unmarshal(bound, &doc); err != nil {
		t.Fatalf("the bound plan: %v (%s)", err, bound)
	}
	if doc.ID != plan.buildID() || doc.Pool.Credentials != "" || doc.Features["brew"].Enabled || doc.RuntimeConfig.Proxy == nil {
		t.Errorf("bound plan = %+v", doc)
	}
	// The client sends the familiar name normalized; the daemon stores it as
	// the familiar name, which is what a create inspects.
	if got := daemon.commitURL.Get("repo") + ":" + daemon.commitURL.Get("tag"); got != "docker.io/library/"+plan.reference() {
		t.Errorf("committed as %q, want %q", got, plan.reference())
	}
	labels := daemon.committed.Labels
	if labels[snapshotBaseLabel] != "sha256:base" || labels[snapshotAgentLabel] != "sha256:"+strings.Repeat("c", 64) || labels[harness.ReclaimLabel] != harness.ReclaimLabelValue {
		t.Errorf("snapshot labels = %v", labels)
	}
	if _, err := os.Stat(filepath.Join(r.snapshotsDir(), plan.buildID())); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the build's tree is left: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proxyagent.PoolSandboxMaterialRoot(r.root, "proj_1", "pool_1"), plan.buildID())); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the build's proxy material is left: %v", err)
	}
	// Nothing of it was ever among the sandboxes' trees, which the control
	// plane is told of.
	if ids, err := r.StoredSandboxIDs(context.Background()); err != nil || len(ids) != 0 {
		t.Errorf("stored sandboxes = %v, %v; want none", ids, err)
	}
}

// A build whose provisioning failed commits nothing.
func TestAFailedSnapshotBuildCommitsNothing(t *testing.T) {
	r, daemon, plan := buildFixture(t, `{"error":"feature \"docker\": step \"install\": exit status 1"}`)
	err := r.buildSnapshot(context.Background(), plan)
	if err == nil || !strings.Contains(err.Error(), "docker") {
		t.Fatalf("buildSnapshot() = %v, want the provisioning's failure", err)
	}
	if daemon.commitURL != nil {
		t.Fatal("committed a build whose provisioning failed")
	}
}

// What a build left when its pool agent stopped — its container, which may
// still run, its tree, its proxy material — is removed by the sweep, the
// build under way is left alone, and a build whose container could not be
// removed keeps its policy, material and tree for the next pass, since an
// identity with no egress entry is unrestricted.
func TestTheSweepRemovesWhatABuildLeft(t *testing.T) {
	key := strings.Repeat("e", 64)
	left, live, stuck := "snapshot_left", snapshotBuildID(key), snapshotBuildID(strings.Repeat("a", 64))
	var mu sync.Mutex
	var listed url.Values
	removed := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.45")
		mu.Lock()
		defer mu.Unlock()
		target := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			listed = r.URL.Query()
			_, _ = w.Write([]byte(`[{"Id":"ctr_treeless","Names":["/discobox-snapshot-pool_1-` + strings.Repeat("f", 24) + `"]},{"Id":"ctr_live","Names":["/discobox-snapshot-pool_1-` + key[:24] + `"]}]`))
		case r.Method == http.MethodDelete && target == "discobox-snapshot-pool_1-"+strings.Repeat("a", 24):
			http.Error(w, `{"message":"removal in progress"}`, http.StatusInternalServerError)
		case r.Method == http.MethodDelete && target == "discobox-snapshot-pool_1-left":
			removed = append(removed, target)
			http.Error(w, `{"message":"No such container"}`, http.StatusNotFound)
		case r.Method == http.MethodDelete:
			removed = append(removed, target)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	r := &DockerSandboxRuntime{root: withTestRoot(t), client: cli, projectID: "proj_1", poolID: "pool_1"}
	for _, id := range []string{left, live, stuck} {
		if err := os.MkdirAll(filepath.Join(r.snapshotsDir(), id, "etc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := proxyagent.EnsureSandboxMaterial(r.root, "proj_1", "pool_1", id); err != nil {
			t.Fatal(err)
		}
	}
	r.snapshots.active = map[string]context.CancelFunc{key: func() {}}

	if err := r.sweepSnapshotBuilds(context.Background()); err == nil {
		t.Fatal("a removal that failed was not reported")
	}
	slices.Sort(removed)
	if want := []string{"ctr_treeless", "discobox-snapshot-pool_1-left"}; !slices.Equal(removed, want) {
		t.Errorf("removed %v, want %v", removed, want)
	}
	if filters := listed.Get("filters"); !strings.Contains(filters, snapshotContainerPrefix) || !strings.Contains(filters, "pool_1") {
		t.Errorf("listed with filters %s, want this pool's builds", filters)
	}
	material := proxyagent.PoolSandboxMaterialRoot(r.root, "proj_1", "pool_1")
	for _, path := range []string{filepath.Join(r.snapshotsDir(), left), filepath.Join(material, left)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is left: %v", path, err)
		}
	}
	for _, id := range []string{live, stuck} {
		for _, path := range []string{filepath.Join(r.snapshotsDir(), id), filepath.Join(material, id)} {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("%s was removed: %v", path, err)
			}
		}
	}
}

// A cache clear removes this pool's snapshots no container runs and forgets
// the plans that failed to build, so a box that came up wrong from one starts
// from scratch next; one a container runs is kept.
func TestAClearRemovesThePoolsSnapshots(t *testing.T) {
	var listed url.Values
	removed := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.45")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/images/json"):
			listed = r.URL.Query()
			_, _ = w.Write([]byte(`[{"Id":"img_unused"},{"Id":"img_running"}]`))
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/images/img_running"):
			http.Error(w, `{"message":"image is being used by running container"}`, http.StatusConflict)
		case r.Method == http.MethodDelete:
			removed = append(removed, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	r := &DockerSandboxRuntime{client: cli, projectID: "proj_1", poolID: "pool_1"}
	r.snapshots.failed = map[string]time.Time{"k": time.Now()}
	if err := r.clearSnapshots(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{"img_unused"}) {
		t.Errorf("removed %v, want the unused snapshot alone", removed)
	}
	if filters := listed.Get("filters"); !strings.Contains(filters, snapshotRepository) || !strings.Contains(filters, "pool_1") {
		t.Errorf("listed with filters %s, want this pool's snapshots", filters)
	}
	if len(r.snapshots.failed) != 0 {
		t.Error("a clear kept the plans that failed to build")
	}
}
