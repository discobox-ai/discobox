package guestimage

import (
	"context"
	"io"
	golog "log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// pushGuestImage serves an in-process registry holding one image with the given
// contents and returns its digest-pinned reference plus a counter of requests,
// so a test can prove a second resolution never transferred the image again.
func pushGuestImage(t *testing.T, contents map[string][]byte) (string, *atomic.Int64) {
	t.Helper()
	var requests atomic.Int64
	handler := registry.New(registry.Logger(golog.New(io.Discard, "", 0)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The sandbox-agent asks every port that starts listening on the
		// machine for its identity once, and this repository is worked on
		// inside a sandbox; that probe is not a registry request. See the
		// repository REVIEW.md.
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			return
		}
		requests.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	host, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse registry URL: %v", err)
	}
	image, err := crane.Image(contents)
	if err != nil {
		t.Fatalf("build guest image: %v", err)
	}
	tag, err := name.NewTag(host.Host + "/discobox-guest:test")
	if err != nil {
		t.Fatalf("build tag: %v", err)
	}
	if err := remote.Write(tag, image); err != nil {
		t.Fatalf("push guest image: %v", err)
	}
	digest, err := image.Digest()
	if err != nil {
		t.Fatalf("image digest: %v", err)
	}
	requests.Store(0)
	return tag.Repository.Name() + "@" + digest.String(), &requests
}

func linuxAMD64() *v1.Platform {
	return &v1.Platform{OS: "linux", Architecture: "amd64"}
}

// The core behavior: a driver names the files it needs and gets local paths,
// with no Docker daemon anywhere in the call.
func TestResolveExtractsRequestedArtifacts(t *testing.T) {
	reference, _ := pushGuestImage(t, map[string][]byte{
		"vmlinux":    []byte("kernel bytes"),
		"initrd.img": []byte("initrd bytes"),
		"root.ext4":  []byte("root bytes"),
		"unrelated":  []byte("ignored"),
	})
	resolver, err := New(Config{
		Reference: reference,
		CacheDir:  t.TempDir(),
		Platform:  linuxAMD64(),
		Artifacts: []Artifact{{Name: "vmlinux"}, {Name: "initrd.img"}, {Name: "root.ext4"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	bundle, err := resolver.Resolve(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for artifact, want := range map[string]string{
		"vmlinux":    "kernel bytes",
		"initrd.img": "initrd bytes",
		"root.ext4":  "root bytes",
	} {
		got, err := os.ReadFile(bundle.Path(artifact))
		if err != nil {
			t.Fatalf("read %s: %v", artifact, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", artifact, got, want)
		}
	}
	// Files the driver did not ask for stay out of the cache: the guest image is
	// a release artifact, not a directory to rummage through.
	if _, err := os.Stat(filepath.Join(bundle.Dir, "unrelated")); !os.IsNotExist(err) {
		t.Errorf("unrelated file was extracted (stat err = %v)", err)
	}
	if !strings.HasPrefix(bundle.Source, "sha256:") {
		t.Errorf("Source = %q, want a digest", bundle.Source)
	}
}

// The cache is why a second pool start is not a second pull, and why the next
// server process starts a pool without transferring the guest again.
func TestResolveCachesByDigestAcrossResolvers(t *testing.T) {
	reference, requests := pushGuestImage(t, map[string][]byte{"vmlinux": []byte("kernel")})
	config := Config{
		Reference: reference,
		CacheDir:  t.TempDir(),
		Platform:  linuxAMD64(),
		Artifacts: []Artifact{{Name: "vmlinux"}},
	}

	first, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := first.Resolve(context.Background(), nil); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}
	pulled := requests.Load()
	if pulled == 0 {
		t.Fatal("first Resolve made no registry requests")
	}

	// A second resolver stands in for the next server process: the cache on
	// disk, not process state, is what makes this cheap.
	second, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	bundle, err := second.Resolve(context.Background(), nil)
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if bundle.Path("vmlinux") == "" {
		t.Fatal("second Resolve returned no kernel path")
	}
	// The manifest is still fetched to learn the digest; no blob is transferred.
	if blobs := requests.Load() - pulled; blobs > 2 {
		t.Errorf("second Resolve made %d registry requests, want a manifest lookup only", blobs)
	}
}

// A guest release that does not carry what the driver boots is reported here,
// where the message can name the image, rather than as a VM that never starts.
func TestResolveFailsOnMissingRequiredArtifact(t *testing.T) {
	reference, _ := pushGuestImage(t, map[string][]byte{"vmlinux": []byte("kernel")})
	resolver, err := New(Config{
		Reference: reference,
		CacheDir:  t.TempDir(),
		Platform:  linuxAMD64(),
		Artifacts: []Artifact{{Name: "vmlinux"}, {Name: "root.ext4"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = resolver.Resolve(context.Background(), nil)
	if err == nil {
		t.Fatal("Resolve succeeded without the root filesystem")
	}
	if !strings.Contains(err.Error(), "root.ext4") {
		t.Errorf("error %q does not name the missing artifact", err)
	}
}

// An optional artifact is how a driver that can boot without an initrd says so
// once, instead of branching on a missing-file error at boot.
func TestResolveToleratesAbsentOptionalArtifact(t *testing.T) {
	reference, _ := pushGuestImage(t, map[string][]byte{"vmlinux": []byte("kernel")})
	resolver, err := New(Config{
		Reference: reference,
		CacheDir:  t.TempDir(),
		Platform:  linuxAMD64(),
		Artifacts: []Artifact{{Name: "vmlinux"}, {Name: "initrd.img", Optional: true}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	bundle, err := resolver.Resolve(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if bundle.Path("initrd.img") != "" {
		t.Errorf("absent optional artifact resolved to %q", bundle.Path("initrd.img"))
	}
}

// The override is how a guest image built from local sources is booted, so it
// must need no registry at all.
func TestResolveUsesOverrideDirectory(t *testing.T) {
	override := t.TempDir()
	if err := os.WriteFile(filepath.Join(override, "vmlinux"), []byte("local kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolver, err := New(Config{
		OverrideDir: override,
		Reference:   "example.invalid/never-pulled:latest",
		Artifacts:   []Artifact{{Name: "vmlinux"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := resolver.Reference(); got != "" {
		t.Errorf("Reference() = %q, want empty while overridden", got)
	}
	bundle, err := resolver.Resolve(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if bundle.Source != "override" {
		t.Errorf("Source = %q, want override", bundle.Source)
	}
	contents, err := os.ReadFile(bundle.Path("vmlinux"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "local kernel" {
		t.Errorf("kernel = %q", contents)
	}
}

// An incomplete override fails when the pool starts rather than booting a VM
// with no root filesystem.
func TestResolveFailsOnIncompleteOverrideDirectory(t *testing.T) {
	resolver, err := New(Config{
		OverrideDir: t.TempDir(),
		Artifacts:   []Artifact{{Name: "vmlinux"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := resolver.Resolve(context.Background(), nil); err == nil {
		t.Fatal("Resolve succeeded with an empty override directory")
	}
}

func TestNewRejectsInvalidConfigurations(t *testing.T) {
	for scenario, cfg := range map[string]Config{
		"no artifacts":        {Reference: "example.com/guest:v1", CacheDir: "/tmp/cache"},
		"duplicate artifacts": {Reference: "example.com/guest:v1", CacheDir: "/tmp/cache", Artifacts: []Artifact{{Name: "vmlinux"}, {Name: "/vmlinux"}}},
		"blank artifact":      {Reference: "example.com/guest:v1", CacheDir: "/tmp/cache", Artifacts: []Artifact{{Name: "  "}}},
		"no reference":        {CacheDir: "/tmp/cache", Artifacts: []Artifact{{Name: "vmlinux"}}},
		"no cache dir":        {Reference: "example.com/guest:v1", Artifacts: []Artifact{{Name: "vmlinux"}}},
		"relative cache dir":  {Reference: "example.com/guest:v1", CacheDir: "cache", Artifacts: []Artifact{{Name: "vmlinux"}}},
		"relative override":   {OverrideDir: "guest", Artifacts: []Artifact{{Name: "vmlinux"}}},
		"bad reference":       {Reference: "NOT A REF", CacheDir: "/tmp/cache", Artifacts: []Artifact{{Name: "vmlinux"}}},
	} {
		t.Run(scenario, func(t *testing.T) {
			if _, err := New(cfg); err == nil {
				t.Fatal("New succeeded")
			}
		})
	}
}

// A locally built guest image is adopted by existing, with no configuration
// change. This is the local bootstrap loop: build the guest, boot the guest.
func TestResolvePrefersACompleteLocalDirectory(t *testing.T) {
	reference, requests := pushGuestImage(t, map[string][]byte{"vmlinux": []byte("published kernel")})
	local := t.TempDir()
	if err := os.WriteFile(filepath.Join(local, "vmlinux"), []byte("locally built kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	resolver, err := New(Config{
		Reference: reference,
		LocalDir:  local,
		CacheDir:  t.TempDir(),
		Platform:  linuxAMD64(),
		Artifacts: []Artifact{{Name: "vmlinux"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	bundle, err := resolver.Resolve(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if bundle.Source != "local" {
		t.Fatalf("Source = %q, want local", bundle.Source)
	}
	contents, err := os.ReadFile(bundle.Path("vmlinux"))
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "locally built kernel" {
		t.Errorf("kernel = %q, want the locally built one", contents)
	}
	if requests.Load() != 0 {
		t.Errorf("a complete local build still made %d registry requests", requests.Load())
	}
}

// An absent or half-built local directory is not an error: it is simply not
// there, and the published image is what boots. A developer mid-build must not
// be able to break every pool on the machine.
func TestResolveFallsBackWhenTheLocalDirectoryIsIncomplete(t *testing.T) {
	reference, _ := pushGuestImage(t, map[string][]byte{
		"vmlinux":   []byte("published kernel"),
		"root.ext4": []byte("published root"),
	})
	halfBuilt := t.TempDir()
	if err := os.WriteFile(filepath.Join(halfBuilt, "vmlinux"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, local := range map[string]string{
		"missing":    filepath.Join(t.TempDir(), "never-built"),
		"half-built": halfBuilt,
	} {
		t.Run(name, func(t *testing.T) {
			resolver, err := New(Config{
				Reference: reference,
				LocalDir:  local,
				CacheDir:  t.TempDir(),
				Platform:  linuxAMD64(),
				Artifacts: []Artifact{{Name: "vmlinux"}, {Name: "root.ext4"}},
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			bundle, err := resolver.Resolve(context.Background(), nil)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if bundle.Source == "local" {
				t.Fatal("an incomplete local build was adopted")
			}
			contents, err := os.ReadFile(bundle.Path("vmlinux"))
			if err != nil {
				t.Fatal(err)
			}
			if string(contents) != "published kernel" {
				t.Errorf("kernel = %q, want the published one", contents)
			}
		})
	}
}

// An explicitly configured directory is an assertion, not a preference: it must
// not silently fall back to the registry when it is broken.
func TestResolveDoesNotFallBackFromAConfiguredOverride(t *testing.T) {
	resolver, err := New(Config{
		OverrideDir: t.TempDir(),
		Reference:   "example.invalid/never-pulled:latest",
		Artifacts:   []Artifact{{Name: "vmlinux"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := resolver.Resolve(context.Background(), nil); err == nil {
		t.Fatal("a broken configured override fell back instead of failing")
	}
}

// A guest image is hundreds of megabytes and is fetched before there is a VM to
// have a console, so this report is the only account of the longest wait in a
// cold pool start. It has to carry bytes, not just the fact that a fetch is
// happening.
func TestResolveReportsFetchProgress(t *testing.T) {
	reference, _ := pushGuestImage(t, map[string][]byte{
		"vmlinux":   []byte(strings.Repeat("kernel", 4096)),
		"root.ext4": []byte(strings.Repeat("root", 4096)),
	})
	resolver, err := New(Config{
		Reference: reference,
		CacheDir:  t.TempDir(),
		Platform:  linuxAMD64(),
		Artifacts: []Artifact{{Name: "vmlinux"}, {Name: "root.ext4"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var mu sync.Mutex
	var reports []Progress
	if _, err := resolver.Resolve(context.Background(), func(progress Progress) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, progress)
	}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reports) < 2 {
		t.Fatalf("reports = %d, want an opening one and a closing one at least", len(reports))
	}
	first := reports[0]
	if first.Reference != reference {
		t.Errorf("first report reference = %q, want %q", first.Reference, reference)
	}
	// The denominator comes from the manifest, so it is known before a byte
	// moves — unlike a Docker pull, whose total grows as layers are walked.
	if first.Total <= 0 {
		t.Errorf("first report total = %d, want the manifest's layer sizes", first.Total)
	}
	if first.Layers <= 0 {
		t.Errorf("first report layers = %d, want the image's layer count", first.Layers)
	}
	last := reports[len(reports)-1]
	if !last.Done {
		t.Error("the last report is not marked done")
	}
	if last.Current != last.Total {
		t.Errorf("closing report = %d of %d bytes, want every compressed byte counted", last.Current, last.Total)
	}
	if last.LayersComplete != last.Layers {
		t.Errorf("closing report = %d of %d layers complete", last.LayersComplete, last.Layers)
	}
}

// A resolve that never reaches the registry has nothing to narrate, and a
// report that fired anyway would put a byte counter on a status line for a
// fetch that is not happening.
func TestResolveReportsNothingForACacheHit(t *testing.T) {
	reference, _ := pushGuestImage(t, map[string][]byte{"vmlinux": []byte("kernel bytes")})
	cache := t.TempDir()
	config := Config{
		Reference: reference,
		CacheDir:  cache,
		Platform:  linuxAMD64(),
		Artifacts: []Artifact{{Name: "vmlinux"}},
	}
	first, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := first.Resolve(context.Background(), nil); err != nil {
		t.Fatalf("first Resolve: %v", err)
	}

	second, err := New(config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	reported := false
	if _, err := second.Resolve(context.Background(), func(Progress) { reported = true }); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if reported {
		t.Error("a cache hit reported fetch progress")
	}
}
