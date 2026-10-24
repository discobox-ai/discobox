package sandboxruntime

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/client"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/assetcache"
)

// fakeAgentLayers answers one layer for every sandbox it is asked about.
type fakeAgentLayers struct {
	digest string
	layer  []byte
	opened int
}

func (f *fakeAgentLayers) AgentLayer(context.Context, string) (string, bool, error) {
	return f.digest, true, nil
}

func (f *fakeAgentLayers) OpenAgentLayer(_ context.Context, _, digest string) (io.ReadCloser, error) {
	if digest != f.digest {
		return nil, errors.New("not the agent layer")
	}
	f.opened++
	return io.NopCloser(bytes.NewReader(f.layer)), nil
}

func gzippedAgentLayer(t *testing.T, agent string, extra ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	write := func(header *tar.Header, body string) {
		if err := tw.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	write(&tar.Header{Typeflag: tar.TypeDir, Name: "features/", Mode: 0o755}, "")
	write(&tar.Header{Typeflag: tar.TypeDir, Name: "features/core/", Mode: 0o755}, "")
	write(&tar.Header{Typeflag: tar.TypeReg, Name: agentlayer.AgentFile, Mode: 0o755, Size: int64(len(agent))}, agent)
	for _, name := range extra {
		write(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: 1}, "x")
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// placementDaemon answers a container's archive route, recording what was
// copied into it, and inspects of the container and its image, which is the
// image the container names in images. It answers no stat: nothing in the
// box is asked.
type placementDaemon struct {
	mu     sync.Mutex
	copied [][]byte
	// images is, by container, the image it runs; unnamed is "img_base".
	images map[string]string
	// labels is, by image, its labels, and tags its tags.
	labels map[string]map[string]string
	tags   map[string][]string
}

func (d *placementDaemon) image(container string) string {
	if image, ok := d.images[container]; ok {
		return image
	}
	return "img_base"
}

func (d *placementDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Api-Version", "1.45")
	switch {
	case strings.HasSuffix(r.URL.Path, "/_ping"):
		w.WriteHeader(http.StatusOK)
	case strings.HasSuffix(r.URL.Path, "/archive") && r.Method == http.MethodPut:
		if r.URL.Query().Get("path") != "/" {
			http.Error(w, "placed somewhere other than the root", http.StatusBadRequest)
			return
		}
		body, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.copied = append(d.copied, body)
		d.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
		id := strings.TrimSuffix(r.URL.Path[strings.Index(r.URL.Path, "/containers/")+len("/containers/"):], "/json")
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"Id": id, "Image": d.image(id)}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	case strings.Contains(r.URL.Path, "/images/") && strings.HasSuffix(r.URL.Path, "/json"):
		id := strings.TrimSuffix(r.URL.Path[strings.Index(r.URL.Path, "/images/")+len("/images/"):], "/json")
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"Id": id, "RepoTags": d.tags[id], "Config": map[string]any{"Labels": d.labels[id]}}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	default:
		http.NotFound(w, r)
	}
}

func placementFixture(t *testing.T, layers AgentLayers) (*DockerSandboxRuntime, *placementDaemon) {
	t.Helper()
	daemon := &placementDaemon{}
	server := httptest.NewServer(daemon)
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	r := &DockerSandboxRuntime{root: withTestRoot(t), client: cli, projectID: "proj_1", poolID: "pool_1", agentLayers: layers}
	return r, daemon
}

func archiveNames(t *testing.T, data []byte) map[string]string {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(data))
	names := map[string]string{}
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(tr)
		names[header.Name] = string(body)
	}
}

// Before a delivered sandbox starts, its platform's agent layer, its units and
// its pins are copied into its container; the layer only when the pool's own
// record does not say it placed it there, and the rest every time (ADR
// 26-10-16-423 §7). A new container gets the layer again.
func TestPlaceAgentCopiesTheLayerOnlyWhenTheBoxLacksIt(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)
	layers := &fakeAgentLayers{digest: digest, layer: gzippedAgentLayer(t, "agent-v1")}
	r, daemon := placementFixture(t, layers)
	pins := []assetcache.Pin{
		{Name: "discobox-platform", Digest: "sha256:" + strings.Repeat("a", 64)},
		{Name: "discobox-harness-shell", Digest: "sha256:" + strings.Repeat("b", 64)},
	}
	if err := assetcache.WriteRecord(r.sandboxRoot("sbx_1"), assetcache.Delivery{Agent: testRecordAgent, Bundles: pins}); err != nil {
		t.Fatal(err)
	}

	if err := r.placeAgent(context.Background(), "sbx_1", "ctr_1"); err != nil {
		t.Fatal(err)
	}
	names := archiveNames(t, daemon.copied[0])
	if got := names[strings.TrimPrefix(agentlayer.LayerDir(digest), "/")+"/"+agentlayer.AgentFile]; got != "agent-v1" {
		t.Fatalf("placed agent = %q, want the layer's", got)
	}
	pinned, err := agentlayer.ParsePins([]byte(names["opt/discobox/bundles/pins.json"]))
	if err != nil || len(pinned) != 2 || pinned[0].Digest != pins[0].Digest || pinned[1].Digest != pins[1].Digest {
		t.Fatalf("placed pins = %+v, %v", pinned, err)
	}

	if err := r.placeAgent(context.Background(), "sbx_1", "ctr_1"); err != nil {
		t.Fatal(err)
	}
	names = archiveNames(t, daemon.copied[1])
	if _, ok := names[strings.TrimPrefix(agentlayer.LayerDir(digest), "/")+"/"+agentlayer.AgentFile]; ok {
		t.Fatal("placed the layer again into a box that holds it")
	}
	if _, ok := names["etc/systemd/system/discobox-wire.service"]; !ok {
		t.Fatal("the units were not placed again")
	}
	if err := r.placeAgent(context.Background(), "sbx_1", "ctr_2"); err != nil {
		t.Fatal(err)
	}
	if _, ok := archiveNames(t, daemon.copied[2])[strings.TrimPrefix(agentlayer.LayerDir(digest), "/")+"/"+agentlayer.AgentFile]; !ok {
		t.Fatal("a new container was not given the layer")
	}
	// Extracted once on this side, however many starts.
	if layers.opened != 1 {
		t.Fatalf("layer opened %d times, want once for its one extraction", layers.opened)
	}
	if _, err := os.Stat(filepath.Join(r.agentsDir(), agentlayer.Hex(digest), agentBinaryName)); err != nil {
		t.Fatalf("the extracted agent: %v", err)
	}
}

// A container started from a snapshot holding the agent layer is not given
// it again, and one from a snapshot of another layer is (ADR 26-10-16-423
// §8).
func TestPlaceAgentLeavesTheLayerASnapshotHolds(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)
	r, daemon := placementFixture(t, &fakeAgentLayers{digest: digest, layer: gzippedAgentLayer(t, "agent-v1")})
	daemon.images = map[string]string{"ctr_snap": "img_snap", "ctr_old": "img_old"}
	daemon.tags = map[string][]string{"img_snap": {snapshotRepository + ":k1"}, "img_old": {snapshotRepository + ":k2"}}
	daemon.labels = map[string]map[string]string{
		"img_snap": {snapshotAgentLabel: digest},
		"img_old":  {snapshotAgentLabel: "sha256:" + strings.Repeat("d", 64)},
	}
	if err := assetcache.WriteRecord(r.sandboxRoot("sbx_1"), assetcache.Delivery{Agent: testRecordAgent, Bundles: []assetcache.Pin{{Name: "discobox-platform", Digest: "sha256:" + strings.Repeat("a", 64)}}}); err != nil {
		t.Fatal(err)
	}
	layerFile := strings.TrimPrefix(agentlayer.LayerDir(digest), "/") + "/" + agentlayer.AgentFile
	if err := r.placeAgent(context.Background(), "sbx_1", "ctr_snap"); err != nil {
		t.Fatal(err)
	}
	if _, ok := archiveNames(t, daemon.copied[0])[layerFile]; ok {
		t.Fatal("placed the layer into a container whose snapshot holds it")
	}
	if err := r.placeAgent(context.Background(), "sbx_1", "ctr_old"); err != nil {
		t.Fatal(err)
	}
	if _, ok := archiveNames(t, daemon.copied[1])[layerFile]; !ok {
		t.Fatal("a snapshot of another agent layer was not given this one")
	}
}

// A sandbox delivered no platform is started as it was built.
func TestPlaceAgentLeavesAnUndeliveredSandbox(t *testing.T) {
	r, daemon := placementFixture(t, &fakeAgentLayers{})
	if err := r.placeAgent(context.Background(), "sbx_1", "ctr_1"); err != nil {
		t.Fatal(err)
	}
	if len(daemon.copied) != 0 {
		t.Fatal("placed an agent in a sandbox delivered no platform")
	}
}

// A platform whose last layer holds anything beside the agent is refused,
// rather than booting a box from whatever it holds.
func TestPlaceAgentRefusesALayerThatIsNotTheAgentsAlone(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)
	r, daemon := placementFixture(t, &fakeAgentLayers{digest: digest, layer: gzippedAgentLayer(t, "agent", "features/core/discobox-access")})
	if err := assetcache.WriteRecord(r.sandboxRoot("sbx_1"), assetcache.Delivery{Agent: testRecordAgent, Bundles: []assetcache.Pin{{Name: "discobox-platform", Digest: "sha256:" + strings.Repeat("a", 64)}}}); err != nil {
		t.Fatal(err)
	}
	if err := r.placeAgent(context.Background(), "sbx_1", "ctr_1"); !errors.Is(err, agentlayer.ErrNotAgentLayer) {
		t.Fatalf("err = %v, want ErrNotAgentLayer", err)
	}
	if len(daemon.copied) != 0 {
		t.Fatal("placed a layer that is not the agent's")
	}
}

// What moved under a delivered container is judged against what the pool
// recorded placing in it, not against the request's previous record: a
// restart cut short is made again by the next create, and a moved sandbox
// base image asks for a new container (ADR 26-10-16-423 §7, as amended).
func TestDeliveredMovedComparesWhatTheContainerBootedOn(t *testing.T) {
	base := "sha256:base-v1"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.45")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/containers/ctr_1/json"):
			_, _ = w.Write([]byte(`{"Id":"ctr_1","Image":"sha256:base-v1","State":{"Status":"running"},"Config":{"Labels":{}}}`))
		case strings.HasSuffix(r.URL.Path, "/images/discobox-sandbox-base:test/json"):
			_, _ = w.Write([]byte(`{"Id":"` + base + `"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	cli, err := client.New(client.WithHost("tcp://"+strings.TrimPrefix(server.URL, "http://")), client.WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	r := &DockerSandboxRuntime{root: withTestRoot(t), client: cli, projectID: "proj_1", poolID: "pool_1", sandboxBaseImage: "discobox-sandbox-base:test"}
	pin := func(hex string) workerapimodel.BundlePin {
		return workerapimodel.BundlePin{Name: "discobox-platform", Digest: "sha256:" + strings.Repeat(hex, 64)}
	}
	create := func(p workerapimodel.BundlePin) *workerapimodel.PoolSandboxCreateRequest {
		return &workerapimodel.PoolSandboxCreateRequest{SandboxId: "sbx_1", Agent: workerclient.NewOptBundlePin(testAgentPin), Bundles: workerclient.NewOptNilBundlePinArray([]workerapimodel.BundlePin{p})}
	}
	existing := &Sandbox{ID: "ctr_1", SandboxID: "sbx_1", Status: StatusRunning}
	booted, _, _ := applied(assetcache.Delivery{Agent: testRecordAgent, Bundles: []assetcache.Pin{{Name: "discobox-platform", Digest: "sha256:" + strings.Repeat("a", 64)}}})
	if err := os.MkdirAll(r.sandboxRoot("sbx_1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.writePlaced("sbx_1", placedRecord{Container: "ctr_1", Layer: "sha256:layer", Pins: booted}); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		req  *workerapimodel.PoolSandboxCreateRequest
		base string
		want deliveredMove
	}{
		"the pins it booted on":       {create(pin("a")), "sha256:base-v1", movedNothing},
		"moved pins":                  {create(pin("b")), "sha256:base-v1", movedPins},
		"moved pins and a moved base": {create(pin("b")), "sha256:base-v2", movedBase},
		"no platform":                 {&workerapimodel.PoolSandboxCreateRequest{SandboxId: "sbx_1"}, "sha256:base-v2", movedNothing},
	} {
		base = tc.base
		got, err := r.deliveredMoved(context.Background(), existing, tc.req)
		if err != nil || got != tc.want {
			t.Errorf("%s: %v, %v; want %v", name, got, err, tc.want)
		}
	}
	// A container the pool has no record of placing has moved.
	other := &Sandbox{ID: "ctr_1", SandboxID: "sbx_2", Status: StatusRunning}
	base = "sha256:base-v1"
	if got, err := r.deliveredMoved(context.Background(), other, create(pin("a"))); err != nil || got != movedPins {
		t.Errorf("unrecorded container: %v, %v; want movedPins", got, err)
	}
}

// testRecordAgent is the agent the records these tests write name.
var testRecordAgent = assetcache.Pin{Name: testAgentPin.Name, Digest: testAgentPin.Digest}

// A pool that upgrades finds the agent-placed.json its earlier agent wrote,
// whose pins are the object they were before they were a list. It reads as
// what that pool placed, so a delivered sandbox's next create compares
// against it rather than failing on it until someone deletes the record.
func TestAPlacedRecordFromBeforePinsWereAListReads(t *testing.T) {
	r := &DockerSandboxRuntime{root: withTestRoot(t)}
	if err := os.MkdirAll(r.sandboxRoot("sbx_1"), 0o755); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	legacy := `{"container":"ctr_1","layer":"sha256:layer","pins":{"platform":{"name":"discobox-platform","digest":"` + digest + `"}}}`
	if err := os.WriteFile(r.placedRecordPath("sbx_1"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	record, ok, err := r.readPlaced("sbx_1")
	if err != nil || !ok {
		t.Fatalf("readPlaced = %v, %v; want the legacy record read", ok, err)
	}
	if len(record.Pins) != 1 || record.Pins[0].Name != "discobox-platform" || record.Pins[0].Digest != digest || record.Container != "ctr_1" {
		t.Fatalf("record = %+v, want the platform pin it recorded", record)
	}
}
