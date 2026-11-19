package assets

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/imagecache"
)

var (
	linuxAMD64  = imagecache.Platform{OS: "linux", Architecture: "amd64"}
	linuxARM64  = imagecache.Platform{OS: "linux", Architecture: "arm64"}
	darwinARM64 = imagecache.Platform{OS: "darwin", Architecture: "arm64"}
)

// platformIndex is an index listing each image under its platform, as a
// multi-platform build lists them.
func (r *registry) platformIndex(t *testing.T, images map[imagecache.Platform]image) string {
	t.Helper()
	var manifests []any
	for platform, img := range images {
		r.mu.Lock()
		size := len(r.content[img.manifest])
		r.mu.Unlock()
		manifests = append(manifests, map[string]any{"mediaType": imagecache.MediaTypeOCIManifest, "digest": img.manifest, "size": size, "platform": platform})
	}
	data, err := json.Marshal(map[string]any{"schemaVersion": 2, "mediaType": imagecache.MediaTypeOCIIndex, "manifests": manifests})
	if err != nil {
		t.Fatal(err)
	}
	return r.add(data)
}

// imageOf reads the image manifest name's index lists for platform, as a
// pool does, through the asset routes.
func imageOf(t *testing.T, f *fixture, name, index string, platform imagecache.Platform) (imagecache.Manifest, string) {
	t.Helper()
	ctx := asPool("pool-1")
	manifest, digest, err := imagecache.ImageManifest(index, platform, func(digest string) (imagecache.Manifest, error) {
		content, err := f.service.AssetManifest(ctx, "pool-1", name, digest)
		if err != nil {
			return imagecache.Manifest{}, err
		}
		return imagecache.ParseManifest(read(t, content))
	})
	if err != nil {
		t.Fatalf("%s for %s: %v", name, platform, err)
	}
	return manifest, digest
}

// The release's agent and platform bundles each list darwin's entry beside
// Linux's (ADR 26-11-11-963 §9). A pool reads darwin's manifests and blobs
// through the same routes as Linux's, under the same names and pins, and a
// Linux pool's box is still placed with Linux's agent.
func TestTheReleasesDarwinEntriesAreServed(t *testing.T) {
	f := newFixture(t)
	const agentRef, poolRef = "ghcr.io/discobox-ai/discobox-agent:v1", "ghcr.io/discobox-ai/discobox-pool:v1"
	agents := map[imagecache.Platform]image{}
	platforms := map[imagecache.Platform]image{}
	for _, platform := range []imagecache.Platform{linuxAMD64, linuxARM64, darwinARM64} {
		agents[platform] = f.registry.imageFor(t, "agent "+platform.OS+"/"+platform.Architecture, platform)
		platforms[platform] = f.registry.imageFor(t, "platform "+platform.OS+"/"+platform.Architecture, platform)
	}
	f.registry.tags[agentRef] = f.registry.platformIndex(t, agents)
	f.registry.tags[platformRef] = f.registry.platformIndex(t, platforms)
	f.registry.tags[poolRef] = f.registry.platformIndex(t, map[imagecache.Platform]image{linuxAMD64: f.registry.image(t, "pool")})
	f.service = newService(f.service.store, Options{Dir: f.dir, PlatformBundle: platformRef, AgentBundle: agentRef, PoolBundle: poolRef}, []source{f.registry}, []source{f.registry})
	ctx := asPool("pool-1")

	for _, bundle := range []struct {
		name, ref string
		images    map[imagecache.Platform]image
	}{
		{"discobox-agent", agentRef, agents},
		{"discobox-platform", platformRef, platforms},
	} {
		content, err := f.service.ResolveAssetName(ctx, "pool-1", bundle.name)
		if err != nil {
			t.Fatalf("resolve %s: %v", bundle.name, err)
		}
		if want := f.registry.tags[bundle.ref]; content.Digest != want {
			t.Fatalf("%s resolved to %s, want the release's index %s", bundle.name, content.Digest, want)
		}
		read(t, content)
		manifest, digest := imageOf(t, f, bundle.name, content.Digest, darwinARM64)
		darwin := bundle.images[darwinARM64]
		if digest != darwin.manifest {
			t.Fatalf("%s for darwin/arm64 is %s, want darwin's %s", bundle.name, digest, darwin.manifest)
		}
		for _, digest := range []string{manifest.Config.Digest, manifest.Layers[0].Digest} {
			read(t, mustBlob(t, f, digest))
		}
	}

	box, err := f.service.PoolBox(context.Background(), linuxAMD64)
	if err != nil {
		t.Fatal(err)
	}
	if box.AgentLayer != agents[linuxAMD64].layer {
		t.Fatalf("a linux/amd64 pool's box is placed with agent layer %s, want Linux's %s", box.AgentLayer, agents[linuxAMD64].layer)
	}
}

// The environment variables naming a built agent bundle's and platform
// bundle's OCI image layout (a tar), which `go tool task test:darwin-bundles`
// builds for darwin and Linux and runs this package's tests with.
const (
	agentLayoutEnv    = "DISCOBOX_TEST_AGENT_BUNDLE_LAYOUT"
	platformLayoutEnv = "DISCOBOX_TEST_PLATFORM_BUNDLE_LAYOUT"
)

// The agent and platform bundles as sandbox-agent/Dockerfile builds them for
// the release, pushed to a registry and served from it by the asset service:
// darwin's entries are darwin's agent and darwin's platform, with its own
// manifest and without the desktop and nested Docker, beside Linux's (ADR
// 26-11-11-963 §9).
func TestTheBuiltDarwinBundlesAreServed(t *testing.T) {
	agentLayout, platformLayout := os.Getenv(agentLayoutEnv), os.Getenv(platformLayoutEnv)
	if agentLayout == "" || platformLayout == "" {
		t.Skipf("set %s and %s to built bundles' layouts: go tool task test:darwin-bundles", agentLayoutEnv, platformLayoutEnv)
	}
	f := newFixture(t)
	const agentRef = "ghcr.io/discobox-ai/discobox-agent:v1"
	f.registry.push(t, agentRef, agentLayout)
	f.registry.push(t, platformRef, platformLayout)
	f.service = newService(f.service.store, Options{Dir: f.dir, PlatformBundle: platformRef, AgentBundle: agentRef},
		[]source{f.registry}, []source{f.registry})
	ctx := asPool("pool-1")
	resolve := func(name string) string {
		t.Helper()
		content, err := f.service.ResolveAssetName(ctx, "pool-1", name)
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		read(t, content)
		return content.Digest
	}
	agentIndex, platformIndex := resolve("discobox-agent"), resolve("discobox-platform")

	// The agent: one layer, the agent alone, built for the entry's platform.
	for platform, magic := range map[imagecache.Platform]func([]byte) bool{darwinARM64: isMachOARM64, linuxAMD64: isELF} {
		agent, _ := imageOf(t, f, "discobox-agent", agentIndex, platform)
		if len(agent.Layers) != 1 {
			t.Fatalf("the agent bundle for %s is %d layers, not the agent's one", platform, len(agent.Layers))
		}
		files := layerFiles(t, f, agent.Layers[0].Digest)
		if len(files) != 1 || !magic(files["features/core/discobox-sandbox-agent"]) {
			t.Fatalf("the agent bundle for %s holds %d files, want %s's agent alone", platform, len(files), platform)
		}
	}

	// The platform: darwin's own manifest, its binaries built for darwin, and
	// every feature but the desktop and nested Docker, which Linux's has.
	features := func(platform imagecache.Platform) (map[string][]byte, harness.ImageMetadata) {
		manifest, _ := imageOf(t, f, "discobox-platform", platformIndex, platform)
		var config struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			Config       struct {
				Labels map[string]string `json:"Labels"`
			} `json:"config"`
		}
		if err := json.Unmarshal(read(t, mustBlob(t, f, manifest.Config.Digest)), &config); err != nil {
			t.Fatal(err)
		}
		if config.OS != platform.OS || config.Architecture != platform.Architecture {
			t.Fatalf("the platform bundle's %s entry is configured %s/%s", platform, config.OS, config.Architecture)
		}
		var metadata harness.ImageMetadata
		raw, ok := config.Config.Labels[harness.ImageLayerLabelPrefix+harness.PlatformLayer]
		if !ok {
			t.Fatalf("the platform bundle's %s entry carries no platform layer", platform)
		}
		if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
			t.Fatalf("the platform bundle's %s platform layer: %v", platform, err)
		}
		files := map[string][]byte{}
		for _, layer := range manifest.Layers {
			for name, data := range layerFiles(t, f, layer.Digest) {
				files[name] = data
			}
		}
		return files, metadata
	}
	darwin, darwinManifest := features(darwinARM64)
	linux, linuxManifest := features(linuxAMD64)
	if linuxManifest.Features != (harness.Features{Desktop: true, Docker: true}) || len(linuxManifest.Volumes) == 0 {
		t.Fatalf("Linux's platform layer = %+v, want sandbox-agent/image.json's", linuxManifest)
	}
	if darwinManifest.Features != (harness.Features{}) || len(darwinManifest.Volumes) != 0 || len(darwinManifest.AdditionalGroups) != 0 {
		t.Fatalf("darwin's platform layer = %+v, want sandbox-agent/image.darwin.json's", darwinManifest)
	}
	for _, name := range []string{
		"features/core/provision.yaml",
		"features/core/discobox-access",
		"features/discobox-cli/discobox",
		"features/review/discobox-review",
		"features/nix/provision.yaml",
		"features/brew/provision.yaml",
		"features/docker/provision.yaml",
		"features/desktop/provision.yaml",
	} {
		inDarwin, inLinux := darwin[name] != nil, linux[name] != nil
		if !inLinux {
			t.Errorf("Linux's platform entry lacks %s", name)
		}
		absent := strings.HasPrefix(name, "features/docker/") || strings.HasPrefix(name, "features/desktop/")
		if inDarwin == absent {
			t.Errorf("darwin's platform entry has %s: %v, want %v", name, inDarwin, !absent)
		}
	}
	for _, name := range []string{"features/core/discobox-access", "features/discobox-cli/discobox", "features/review/discobox-review"} {
		if !isMachOARM64(darwin[name]) {
			t.Errorf("darwin's %s is not a darwin/arm64 executable", name)
		}
		if !isELF(linux[name]) {
			t.Errorf("Linux's %s is not a Linux executable", name)
		}
	}
	for name := range darwin {
		if strings.HasPrefix(name, "features/docker/") || strings.HasPrefix(name, "features/desktop/") {
			t.Fatalf("darwin's platform entry carries %s", name)
		}
	}
}

// push puts every blob of the OCI image layout tar at path in the registry,
// and tags ref with the one manifest or index its index.json lists, as
// pushing what `docker buildx build --output type=oci` built would.
func (r *registry) push(t *testing.T, ref, path string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	entries := tar.NewReader(file)
	for {
		header, err := entries.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		name := strings.TrimPrefix(header.Name, "./")
		data, err := io.ReadAll(entries)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case strings.HasPrefix(name, "blobs/sha256/") && header.Typeflag == tar.TypeReg:
			if digest := r.add(data); digest != "sha256:"+strings.TrimPrefix(name, "blobs/sha256/") {
				t.Fatalf("%s: %s holds %s", path, name, digest)
			}
		case name == "index.json":
			var index imagecache.Manifest
			if err := json.Unmarshal(data, &index); err != nil || len(index.Manifests) != 1 {
				t.Fatalf("%s: index.json lists no one bundle (%v)", path, err)
			}
			r.mu.Lock()
			r.tags[ref] = index.Manifests[0].Digest
			r.mu.Unlock()
		}
	}
	if r.tags[ref] == "" {
		t.Fatalf("%s has no index.json", path)
	}
}

// layerFiles reads the regular files of the layer digest, served as a blob,
// by path.
func layerFiles(t *testing.T, f *fixture, digest string) map[string][]byte {
	t.Helper()
	content := mustBlob(t, f, digest)
	defer content.Body.Close()
	body := bufio.NewReader(content.Body)
	var stream io.Reader = body
	if head, err := body.Peek(2); err == nil && bytes.Equal(head, []byte{0x1f, 0x8b}) {
		gz, err := gzip.NewReader(body)
		if err != nil {
			t.Fatal(err)
		}
		defer gz.Close()
		stream = gz
	}
	files := map[string][]byte{}
	entries := tar.NewReader(stream)
	for {
		header, err := entries.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatalf("layer %s: %v", digest, err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(entries)
		if err != nil {
			t.Fatal(err)
		}
		files[strings.TrimPrefix(header.Name, "/")] = data
	}
}

// isMachOARM64 says whether data is a 64-bit Mach-O executable for arm64.
func isMachOARM64(data []byte) bool {
	const magic64, cpuARM64 = 0xfeedfacf, 0x0100000c
	return len(data) >= 8 && binary.LittleEndian.Uint32(data) == magic64 && binary.LittleEndian.Uint32(data[4:]) == cpuARM64
}

// isELF says whether data is an ELF executable.
func isELF(data []byte) bool { return bytes.HasPrefix(data, []byte("\x7fELF")) }
