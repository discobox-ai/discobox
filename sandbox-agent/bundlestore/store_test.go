package bundlestore

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/imagecache"
	"github.com/discobox-ai/discobox/sandbox-agent/provision"
)

// assets is an asset host in memory, counting what it is asked for.
type assets struct {
	mu      sync.Mutex
	content map[string][]byte
	asked   map[string]int
}

func newAssets() *assets {
	return &assets{content: map[string][]byte{}, asked: map[string]int{}}
}

func (a *assets) Fetch(_ context.Context, path string) (io.ReadCloser, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.asked[path]++
	data, ok := a.content[path]
	if !ok {
		return nil, errors.New("GET " + path + ": 404 Not Found")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (a *assets) blob(t *testing.T, data []byte) imagecache.Descriptor {
	t.Helper()
	digest := digestOf(data)
	a.content["/blobs/"+digest] = data
	return imagecache.Descriptor{MediaType: "application/vnd.oci.image.layer.v1.tar+gzip", Digest: digest, Size: int64(len(data))}
}

// layer is a gzipped tar of files, path to content; a path ending in / is a
// directory, and content "link:<target>" a symbolic link.
func layer(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if strings.HasSuffix(name, "/") {
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name, Mode: 0o755}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if target, ok := strings.CutPrefix(files[name], "link:"); ok {
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: target, Mode: 0o777}); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(files[name]))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(files[name])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// bundle publishes an image manifest of layers under name and returns its
// digest; with index set, an index listing it for linux/amd64 beside an
// attestation, and the index's digest.
func (a *assets) bundle(t *testing.T, name string, index bool, layers ...[]byte) (pinned, manifestDigest string) {
	t.Helper()
	config := a.blob(t, []byte(`{"architecture":"amd64","os":"linux"}`))
	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     imagecache.MediaTypeOCIManifest,
		"config":        config,
	}
	var descriptors []imagecache.Descriptor
	for _, l := range layers {
		descriptors = append(descriptors, a.blob(t, l))
	}
	manifest["layers"] = descriptors
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestDigest = digestOf(data)
	a.content["/"+name+"/manifests/"+manifestDigest] = data
	if !index {
		return manifestDigest, manifestDigest
	}
	idx, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     imagecache.MediaTypeOCIIndex,
		"manifests": []imagecache.Descriptor{
			{MediaType: imagecache.MediaTypeOCIManifest, Digest: digestOf([]byte("attestation")), Platform: &imagecache.Platform{OS: "unknown", Architecture: "unknown"}},
			{MediaType: imagecache.MediaTypeOCIManifest, Digest: manifestDigest, Size: int64(len(data)), Platform: &imagecache.Platform{OS: "linux", Architecture: "amd64"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	pinned = digestOf(idx)
	a.content["/"+name+"/manifests/"+pinned] = idx
	return pinned, manifestDigest
}

func newStore(t *testing.T, a *assets) *Store {
	t.Helper()
	return &Store{Root: t.TempDir(), Fetcher: a, Platform: imagecache.Platform{OS: "linux", Architecture: "amd64"}}
}

func read(t *testing.T, s *Store, path string) string {
	t.Helper()
	data, err := os.ReadFile(s.path(path))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

var (
	coreLayer  = map[string]string{"features/": "", "features/core/": "", "features/core/provision.yaml": "core v1"}
	agentLayer = map[string]string{"features/": "", "features/core/": "", "features/core/discobox-sandbox-agent": "agent v1"}
	nodeLayer  = map[string]string{"features/": "", "features/node/": "", "features/node/provision.yaml": "node"}
)

func TestApplyComposesEachPinsTreeAndPointsProvisioningAtIt(t *testing.T) {
	a := newAssets()
	platform, _ := a.bundle(t, "discobox-platform", true, layer(t, coreLayer), layer(t, nodeLayer), layer(t, agentLayer))
	harness, _ := a.bundle(t, "discobox-harness-claude-code", false, layer(t, map[string]string{"features/claude-code/provision.yaml": "claude"}))
	s := newStore(t, a)
	err := s.Apply(context.Background(), agentlayer.Pins{
		Platform: agentlayer.Pin{Name: "discobox-platform", Digest: platform},
		Harness:  &agentlayer.Pin{Name: "discobox-harness-claude-code", Digest: harness},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, s, provision.PlatformTree+"/features/core/discobox-sandbox-agent"); got != "agent v1" {
		t.Fatalf("agent = %q", got)
	}
	if got := read(t, s, provision.PlatformTree+"/features/core/provision.yaml"); got != "core v1" {
		t.Fatalf("core = %q", got)
	}
	if got := read(t, s, provision.HarnessTree+"/features/claude-code/provision.yaml"); got != "claude" {
		t.Fatalf("harness = %q", got)
	}
	if link, err := os.Readlink(s.path(provision.PlatformTree)); err != nil || !strings.HasPrefix(link, "trees/") {
		t.Fatalf("platform tree is %q, %v; want a link into trees/", link, err)
	}
}

func TestApplyFetchesOnlyTheLayersTheStoreLacks(t *testing.T) {
	a := newAssets()
	core, agent := layer(t, coreLayer), layer(t, agentLayer)
	first, _ := a.bundle(t, "discobox-platform", true, core, agent)
	s := newStore(t, a)
	if err := s.Apply(context.Background(), agentlayer.Pins{Platform: agentlayer.Pin{Name: "discobox-platform", Digest: first}}); err != nil {
		t.Fatal(err)
	}
	agent2 := layer(t, map[string]string{"features/": "", "features/core/": "", "features/core/discobox-sandbox-agent": "agent v2"})
	second, _ := a.bundle(t, "discobox-platform", true, core, agent2)
	if err := s.Apply(context.Background(), agentlayer.Pins{Platform: agentlayer.Pin{Name: "discobox-platform", Digest: second}}); err != nil {
		t.Fatal(err)
	}
	if n := a.asked["/blobs/"+digestOf(core)]; n != 1 {
		t.Fatalf("the unchanged layer was fetched %d times, want once", n)
	}
	if n := a.asked["/blobs/"+digestOf(agent2)]; n != 1 {
		t.Fatalf("the changed layer was fetched %d times, want once", n)
	}
	if got := read(t, s, provision.PlatformTree+"/features/core/discobox-sandbox-agent"); got != "agent v2" {
		t.Fatalf("agent = %q", got)
	}
}

func TestApplyKeepsThePreviousTreeAndCollectsTheRest(t *testing.T) {
	a := newAssets()
	s := newStore(t, a)
	var trees []string
	for _, version := range []string{"v1", "v2", "v3"} {
		pinned, tree := a.bundle(t, "discobox-platform", false, layer(t, map[string]string{"features/core/discobox-sandbox-agent": version}))
		if err := s.Apply(context.Background(), agentlayer.Pins{Platform: agentlayer.Pin{Name: "discobox-platform", Digest: pinned}}); err != nil {
			t.Fatal(err)
		}
		trees = append(trees, tree)
	}
	if _, err := os.Stat(s.path(agentlayer.TreeDir(trees[0]))); !os.IsNotExist(err) {
		t.Fatalf("the oldest tree is still there: %v", err)
	}
	for _, kept := range trees[1:] {
		if _, err := os.Stat(s.path(agentlayer.TreeDir(kept))); err != nil {
			t.Fatalf("tree %s was removed: %v", kept, err)
		}
	}
	layers, err := os.ReadDir(s.path(agentlayer.LayersDir))
	if err != nil {
		t.Fatal(err)
	}
	if len(layers) != 4 { // two layers, each with its marker
		var names []string
		for _, l := range layers {
			names = append(names, l.Name())
		}
		t.Fatalf("layers left: %v, want the two kept trees' with their markers", names)
	}
}

func TestApplyUsesTheAgentLayerThePoolPlaced(t *testing.T) {
	a := newAssets()
	agent := layer(t, agentLayer)
	pinned, _ := a.bundle(t, "discobox-platform", false, layer(t, coreLayer), agent)
	s := newStore(t, a)
	digest := digestOf(agent)
	dir := s.path(agentlayer.LayerDir(digest) + "/features/core")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "discobox-sandbox-agent"), []byte("placed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path(agentlayer.LayerComplete(digest)), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path(agentlayer.CurrentLink)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("bundles/layers/"+agentlayer.Hex(digest), s.path(agentlayer.CurrentLink)); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(context.Background(), agentlayer.Pins{Platform: agentlayer.Pin{Name: "discobox-platform", Digest: pinned}}); err != nil {
		t.Fatal(err)
	}
	if n := a.asked["/blobs/"+digest]; n != 0 {
		t.Fatalf("the placed agent layer was fetched %d times", n)
	}
	if got := read(t, s, provision.PlatformTree+"/features/core/discobox-sandbox-agent"); got != "placed" {
		t.Fatalf("agent = %q", got)
	}
}

func TestApplyFetchesAgainALayerWhosePlacementWasCutShort(t *testing.T) {
	a := newAssets()
	agent := layer(t, agentLayer)
	pinned, _ := a.bundle(t, "discobox-platform", false, agent)
	s := newStore(t, a)
	partial := s.path(agentlayer.LayerDir(digestOf(agent)) + "/features")
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(context.Background(), agentlayer.Pins{Platform: agentlayer.Pin{Name: "discobox-platform", Digest: pinned}}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, s, provision.PlatformTree+"/features/core/discobox-sandbox-agent"); got != "agent v1" {
		t.Fatalf("agent = %q", got)
	}
}

func TestApplyReplacesTheTreeARunnableImageBuiltInPlace(t *testing.T) {
	a := newAssets()
	pinned, _ := a.bundle(t, "discobox-platform", false, layer(t, agentLayer))
	s := newStore(t, a)
	baked := s.path(provision.PlatformTree + "/features/core")
	if err := os.MkdirAll(baked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(baked, "discobox-sandbox-agent"), []byte("baked"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(context.Background(), agentlayer.Pins{Platform: agentlayer.Pin{Name: "discobox-platform", Digest: pinned}}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, s, provision.PlatformTree+"/features/core/discobox-sandbox-agent"); got != "agent v1" {
		t.Fatalf("agent = %q", got)
	}
}

func TestApplyRemovesTheHarnessTreeWhenNoHarnessIsPinned(t *testing.T) {
	a := newAssets()
	platform, _ := a.bundle(t, "discobox-platform", false, layer(t, agentLayer))
	harness, _ := a.bundle(t, "discobox-harness-x", false, layer(t, map[string]string{"features/x/provision.yaml": "x"}))
	s := newStore(t, a)
	pins := agentlayer.Pins{Platform: agentlayer.Pin{Name: "discobox-platform", Digest: platform}, Harness: &agentlayer.Pin{Name: "discobox-harness-x", Digest: harness}}
	if err := s.Apply(context.Background(), pins); err != nil {
		t.Fatal(err)
	}
	pins.Harness = nil
	if err := s.Apply(context.Background(), pins); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(s.path(provision.HarnessTree)); !os.IsNotExist(err) {
		t.Fatalf("the harness tree is still there: %v", err)
	}
}

func TestApplyRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		publish func(t *testing.T, a *assets) string
		want    string
	}{
		"a manifest that is not its digest": {
			publish: func(t *testing.T, a *assets) string {
				pinned, _ := a.bundle(t, "discobox-platform", false, layer(t, agentLayer))
				a.content["/discobox-platform/manifests/"+pinned] = []byte(`{"schemaVersion":2}`)
				return pinned
			},
			want: "arrived as",
		},
		"a layer that is not its digest": {
			publish: func(t *testing.T, a *assets) string {
				l := layer(t, agentLayer)
				pinned, _ := a.bundle(t, "discobox-platform", false, l)
				a.content["/blobs/"+digestOf(l)] = layer(t, coreLayer)
				return pinned
			},
			want: "arrived as",
		},
		"a whiteout": {
			publish: func(t *testing.T, a *assets) string {
				pinned, _ := a.bundle(t, "discobox-platform", false, layer(t, map[string]string{"features/core/.wh.old": ""}))
				return pinned
			},
			want: "whiteout",
		},
		"a link out of the layer": {
			publish: func(t *testing.T, a *assets) string {
				pinned, _ := a.bundle(t, "discobox-platform", false, layer(t, map[string]string{"features/core/etc": "link:/etc"}))
				return pinned
			},
			want: "outside",
		},
		"two layers holding one file": {
			publish: func(t *testing.T, a *assets) string {
				pinned, _ := a.bundle(t, "discobox-platform", false, layer(t, agentLayer), layer(t, map[string]string{"features/core/discobox-sandbox-agent": "other"}))
				return pinned
			},
			want: "in an earlier layer too",
		},
		"an index with no manifest for the platform": {
			publish: func(t *testing.T, a *assets) string {
				pinned, _ := a.bundle(t, "discobox-platform", true, layer(t, agentLayer))
				return pinned
			},
			want: "no linux/arm64 image manifest",
		},
	} {
		t.Run(name, func(t *testing.T) {
			a := newAssets()
			pinned := tc.publish(t, a)
			s := newStore(t, a)
			if name == "an index with no manifest for the platform" {
				s.Platform = imagecache.Platform{OS: "linux", Architecture: "arm64"}
			}
			err := s.Apply(context.Background(), agentlayer.Pins{Platform: agentlayer.Pin{Name: "discobox-platform", Digest: pinned}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Apply = %v, want an error saying %q", err, tc.want)
			}
			if _, err := os.Lstat(s.path(provision.PlatformTree)); !os.IsNotExist(err) {
				t.Fatalf("a refused bundle was pointed at: %v", err)
			}
		})
	}
}

// A box whose store holds its pins' trees applies them with nothing fetched,
// manifests included: a box booted with its plan in its store, or from a
// snapshot of one, needs no asset route to start provisioning (ADR
// 26-10-16-423 §7). Going back to the previous pins fetches nothing either.
func TestApplyFetchesNothingForPinsTheStoreHolds(t *testing.T) {
	a := newAssets()
	first, _ := a.bundle(t, "discobox-platform", true, layer(t, coreLayer), layer(t, agentLayer))
	second, _ := a.bundle(t, "discobox-platform", true, layer(t, coreLayer), layer(t, map[string]string{"features/": "", "features/core/": "", "features/core/discobox-sandbox-agent": "agent v2"}))
	s := newStore(t, a)
	for _, digest := range []string{first, second} {
		if err := s.Apply(context.Background(), agentlayer.Pins{Platform: agentlayer.Pin{Name: "discobox-platform", Digest: digest}}); err != nil {
			t.Fatal(err)
		}
	}
	s.Fetcher = failingFetcher{t}
	for _, digest := range []string{second, first} {
		if err := s.Apply(context.Background(), agentlayer.Pins{Platform: agentlayer.Pin{Name: "discobox-platform", Digest: digest}}); err != nil {
			t.Fatalf("apply %s from the store: %v", digest, err)
		}
	}
	if got := read(t, s, provision.PlatformTree+"/features/core/discobox-sandbox-agent"); got != "agent v1" {
		t.Fatalf("agent = %q after going back to the first pins, want agent v1", got)
	}
}

type failingFetcher struct{ t *testing.T }

func (f failingFetcher) Fetch(_ context.Context, path string) (io.ReadCloser, error) {
	f.t.Errorf("fetched %s for pins the store holds", path)
	return nil, errors.New("no asset route")
}
