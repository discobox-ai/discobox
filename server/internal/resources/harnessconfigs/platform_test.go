package harnessconfigs

import (
	"io"
	golog "log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/discobox-ai/discobox/pkg/platform"
)

// testRegistry serves an in-process registry and returns its host.
func testRegistry(t *testing.T) string {
	t.Helper()
	handler := registry.New(registry.Logger(golog.New(io.Discard, "", 0)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The sandbox-agent probes every port that starts listening once; it
		// is not a registry request (repository REVIEW.md).
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	host, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return host.Host
}

// archImage is an image for one platform whose label names its architecture,
// so a test can tell which platform's labels were read.
func archImage(t *testing.T, p v1.Platform) v1.Image {
	t.Helper()
	image, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{
		OS: p.OS, Architecture: p.Architecture,
		Config: v1.Config{Labels: map[string]string{"arch": p.Architecture}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return image
}

// pushIndex pushes an index of one image per platform, and resolves it the way
// the inspector does. It also returns the tag it was pushed to.
func pushIndex(t *testing.T, platforms ...v1.Platform) (*remote.Descriptor, name.Tag) {
	t.Helper()
	tag, err := name.NewTag(testRegistry(t) + "/harness:test")
	if err != nil {
		t.Fatal(err)
	}
	var index v1.ImageIndex = empty.Index
	for _, p := range platforms {
		index = mutate.AppendManifests(index, mutate.IndexAddendum{
			Add:        archImage(t, p),
			Descriptor: v1.Descriptor{Platform: &p},
		})
	}
	if err := remote.WriteIndex(tag, index); err != nil {
		t.Fatal(err)
	}
	pool := platform.Pool()
	descriptor, err := remote.Get(tag, remote.WithPlatform(v1.Platform{OS: pool.OS, Architecture: pool.Arch}))
	if err != nil {
		t.Fatal(err)
	}
	return descriptor, tag
}

func labelArch(t *testing.T, image v1.Image) string {
	t.Helper()
	config, err := image.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	return config.Config.Labels["arch"]
}

// The platform bundle is published for several platforms, and its index says
// which; the attestation manifests a build adds are not platforms. Its labels
// are read from the platform a pool on this machine hosts.
func TestPublishedImageReadsAnIndexsPlatforms(t *testing.T) {
	pool := platform.Pool()
	other := "riscv64"
	descriptor, _ := pushIndex(t,
		v1.Platform{OS: "linux", Architecture: pool.Arch},
		v1.Platform{OS: "linux", Architecture: other},
		v1.Platform{OS: "unknown", Architecture: "unknown"},
	)
	image, err := publishedImage(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if got := labelArch(t, image); got != pool.Arch {
		t.Fatalf("labels read from %q, want this machine's %q", got, pool.Arch)
	}
}

// A bundle not published for this machine's platform still declares the same
// manifest: its labels are read from a platform it is published for.
func TestPublishedImageReadsAnotherPlatformsLabels(t *testing.T) {
	descriptor, _ := pushIndex(t, v1.Platform{OS: "linux", Architecture: "riscv64"})
	image, err := publishedImage(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if got := labelArch(t, image); got != "riscv64" {
		t.Fatalf("labels read from %q, want riscv64", got)
	}
}
