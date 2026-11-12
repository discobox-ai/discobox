package harnessconfigs

import (
	"context"
	"errors"
	"io"
	golog "log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/store"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// layerReads counts the GETs a registry answers for the blobs it was told are
// layers, by digest.
type layerReads struct {
	mu     sync.Mutex
	layers map[string]bool
	reads  int
}

func (l *layerReads) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reads
}

// countingRegistry is testRegistry, counting the layer blobs it serves.
func countingRegistry(t *testing.T, reads *layerReads) string {
	t.Helper()
	handler := registry.New(registry.Logger(golog.New(io.Discard, "", 0)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "discobox-sandbox-agent (port probe)" {
			return
		}
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") {
			reads.mu.Lock()
			if reads.layers[r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]] {
				reads.reads++
			}
			reads.mu.Unlock()
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

// pushCountedBundle pushes a one-layer bundle to repository and records its
// layer as one reads counts.
func pushCountedBundle(t *testing.T, host, repository string, labels map[string]string, reads *layerReads) string {
	t.Helper()
	image, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{OS: "linux", Architecture: "amd64", Config: v1.Config{Labels: labels}})
	if err != nil {
		t.Fatal(err)
	}
	layer := layerOf(t, "features/"+repository+"/install.sh")
	if image, err = mutate.AppendLayers(image, layer); err != nil {
		t.Fatal(err)
	}
	digest, err := layer.Digest()
	if err != nil {
		t.Fatal(err)
	}
	reads.mu.Lock()
	reads.layers[digest.String()] = true
	reads.mu.Unlock()
	tag, err := name.NewTag(host + "/" + repository + ":v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(tag, image); err != nil {
		t.Fatal(err)
	}
	return tag.String()
}

// A bundle's whiteout verdict is its content's, which its digest names, so a
// server that restarts onto bundles it already verified reads their manifests
// and no layer: the platform bundle's layers are the agent's binaries, and
// reading them again puts the registry's throughput on every start.
func TestARestartedServerReadsNoLayerOfABundleItVerified(t *testing.T) {
	reads := &layerReads{layers: map[string]bool{}}
	host := countingRegistry(t, reads)
	platformRef := pushCountedBundle(t, host, "platform", map[string]string{
		harness.ImageLayerLabelPrefix + harness.PlatformLayer: testPlatformLayer,
	}, reads)
	harnessRef := pushCountedBundle(t, host, "claude-code",
		bundleLabels(`{"harness":{"id":"claude-code","name":"Claude Code"}}`), reads)
	references := bundleReferences(map[string]string{"claude-code": harnessRef})
	references.PlatformBundle = platformRef

	st := newTestStore(t)
	ctx := context.Background()
	if err := NewService(st, references, true).SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("first start: SeedBuiltIns() error = %v", err)
	}
	if reads.count() == 0 {
		t.Fatal("first start read no layer; want each bundle's layers verified once")
	}

	before := reads.count()
	// A restart: a new service, as server.NewApp builds, on the same database.
	if err := NewService(st, references, true).SeedBuiltIns(ctx, "project-1"); err != nil {
		t.Fatalf("restart: SeedBuiltIns() error = %v", err)
	}
	if again := reads.count() - before; again != 0 {
		t.Fatalf("restart read %d layer blob(s) of bundles verified on the first start; want 0", again)
	}
}

// Reopen physical connections too: the verdict is on disk, not in a service,
// store, or connection pool.
func TestBundleVerificationSurvivesDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "server.db")
	open := func() *database.DB {
		db, err := database.New(database.Config{DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
		return db
	}
	db := open()
	st := store.New(db.Write, db.Read)
	ref := pushBundle(t, map[string]string{}, []string{"features/good/install.sh"})
	first, err := (defaultImageInspector{verified: &verifiedBundles{store: st}}).InspectBundle(ctx, ref, admitAny)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = open()
	defer db.Close()
	st = store.New(db.Write, db.Read)
	if found, err := st.HasVerifiedBundle(ctx, first.Digest); err != nil || !found {
		t.Fatalf("reopened verdict = %v, %v", found, err)
	}
	// Even on a cache hit, labels must still satisfy this caller's admission.
	refusal := errors.New("wrong bundle kind")
	_, err = (defaultImageInspector{verified: &verifiedBundles{store: st}}).InspectBundle(ctx, ref, func(map[string]string) error { return refusal })
	if !errors.Is(err, refusal) {
		t.Fatalf("cached bundle admission error = %v", err)
	}
}

func TestFailedBundleVerificationIsNeverPersisted(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	ref := pushBundle(t, map[string]string{}, []string{"features/bad/.wh.install.sh"})
	for range 2 {
		_, err := (defaultImageInspector{verified: &verifiedBundles{store: st}}).InspectBundle(ctx, ref, admitAny)
		if err == nil || !strings.Contains(err.Error(), "whiteout") {
			t.Fatalf("inspection error = %v", err)
		}
	}
	parsed, err := name.ParseReference(ref)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := remote.Get(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if found, err := st.HasVerifiedBundle(ctx, descriptor.Digest.String()); err != nil || found {
		t.Fatalf("failed check cached = %v, %v", found, err)
	}
}

// A stored index verdict must not admit the as-yet unchecked selected image.
func TestIndexDigestDoesNotStandInForImageVerification(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	image, err := mutate.AppendLayers(empty.Image, layerOf(t, "features/bad/.wh.install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	image, err = mutate.ConfigFile(image, &v1.ConfigFile{OS: "linux", Architecture: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: image, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}})
	tag, err := name.NewTag(testRegistry(t) + "/platform:index")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(tag, index); err != nil {
		t.Fatal(err)
	}
	digest, err := index.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RecordVerifiedBundle(ctx, digest.String()); err != nil {
		t.Fatal(err)
	}
	_, err = (defaultImageInspector{verified: &verifiedBundles{store: st}}).InspectBundle(ctx, tag.String(), admitAny)
	if err == nil || !strings.Contains(err.Error(), "whiteout") {
		t.Fatalf("indexed image inspection = %v", err)
	}
}
