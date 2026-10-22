package assets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/imagecache"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/auth"
	"github.com/discobox-ai/discobox/server/internal/database"
	"github.com/discobox-ai/discobox/server/internal/model"
	services "github.com/discobox-ai/discobox/server/internal/services"
	"github.com/discobox-ai/discobox/server/internal/store"
)

// registry is a source holding content by digest and references by name, as
// a registry does, counting what it is asked for.
type registry struct {
	mu      sync.Mutex
	content map[string][]byte
	tags    map[string]string
	// corrupt names digests served with other bytes than their own.
	corrupt map[string]bool
	// gate, when set, holds every fetch until it is closed.
	gate chan struct{}
	// down fails every resolve, as an unreachable registry does.
	down     bool
	fetches  atomic.Int32
	releases map[string]bool
}

func newRegistry() *registry {
	return &registry{content: map[string][]byte{}, tags: map[string]string{}, corrupt: map[string]bool{}, releases: map[string]bool{}}
}

func (r *registry) add(data []byte) string {
	digest := digestOf(data)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.content[digest] = data
	return digest
}

func (r *registry) fetch(_ context.Context, p pin, digest string, _ bool, blobs *imagecache.Layout) error {
	if r.gate != nil {
		<-r.gate
	}
	r.fetches.Add(1)
	r.mu.Lock()
	data, ok := r.content[digest]
	corrupt := r.corrupt[digest]
	r.releases[p.ref] = p.release
	r.mu.Unlock()
	if !ok {
		return errNotHere
	}
	if corrupt {
		data = append([]byte("not "), data...)
	}
	return blobs.PutBlob(digest, bytes.NewReader(data))
}

func (r *registry) resolve(_ context.Context, ref string, release bool, blobs *imagecache.Layout) (string, error) {
	r.mu.Lock()
	digest, ok := r.tags[ref]
	data := r.content[digest]
	r.releases[ref] = release
	down := r.down
	r.mu.Unlock()
	if down {
		return "", errors.New("registry unreachable")
	}
	if !ok {
		return "", errNotHere
	}
	return digest, blobs.PutBlob(digest, bytes.NewReader(data))
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// image is one bundle's content in the registry: an image manifest naming a
// config and one layer.
type image struct {
	manifest, config, layer string
}

func (r *registry) image(t *testing.T, label string) image {
	t.Helper()
	config := []byte(`{"architecture":"amd64","os":"linux","config":{"Labels":{"name":"` + label + `"}}}`)
	layer := []byte("layer of " + label)
	img := image{config: r.add(config), layer: r.add(layer)}
	data, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     imagecache.MediaTypeOCIManifest,
		"config":        map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": img.config, "size": len(config)},
		"layers":        []any{map[string]any{"mediaType": "application/vnd.oci.image.layer.v1.tar", "digest": img.layer, "size": len(layer)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	img.manifest = r.add(data)
	return img
}

// index lists children as an index does, without its own media type, as an
// OCI index may.
func (r *registry) index(t *testing.T, children ...image) string {
	t.Helper()
	var manifests []any
	for _, child := range children {
		r.mu.Lock()
		size := len(r.content[child.manifest])
		r.mu.Unlock()
		manifests = append(manifests, map[string]any{"mediaType": imagecache.MediaTypeOCIManifest, "digest": child.manifest, "size": size})
	}
	data, err := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": manifests})
	if err != nil {
		t.Fatal(err)
	}
	return r.add(data)
}

const (
	platformRef = "ghcr.io/discobox-ai/discobox-platform:v1"
	acmeRef     = "example.com/acme/discobox-harness-acme:v1"
	otherRef    = "example.com/other/discobox-harness-other:v1"
)

type fixture struct {
	db       *database.DB
	service  *Service
	registry *registry
	dir      string
	platform image
	index    string
	acme     image
	other    image
}

// newFixture is a server whose release pins a platform bundle, with two
// projects: project-1's pool-1 and harness config on acme's bundle, and
// project-2's pool-2 and harness config on other's.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	db, err := database.New(database.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(ctx); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	f := &fixture{db: db, registry: newRegistry(), dir: t.TempDir()}
	f.platform = f.registry.image(t, "platform")
	f.index = f.registry.index(t, f.platform)
	f.registry.tags[platformRef] = f.index
	f.acme = f.registry.image(t, "acme")
	f.other = f.registry.image(t, "other")
	for _, row := range []any{
		&model.Project{ID: "project-1", OwnerUserID: "user-1", Name: "One"},
		&model.Project{ID: "project-2", OwnerUserID: "user-1", Name: "Two"},
		&model.SandboxProviderInstance{ID: "provider-1", ProjectID: "project-1", Type: "docker", Name: "one"},
		&model.SandboxProviderInstance{ID: "provider-2", ProjectID: "project-2", Type: "docker", Name: "two"},
		&model.Pool{ID: "pool-1", ProjectID: "project-1", PoolManifest: model.PoolManifest{Name: "one", ProviderInstanceID: "provider-1"}},
		&model.Pool{ID: "pool-2", ProjectID: "project-2", PoolManifest: model.PoolManifest{Name: "two", ProviderInstanceID: "provider-2"}},
		&model.HarnessConfig{ID: "hc-1", ProjectID: "project-1", Slug: "acme", Name: "Acme", Bundle: acmeRef, BundleDigest: f.acme.manifest},
		&model.HarnessConfig{ID: "hc-2", ProjectID: "project-2", Slug: "other", Name: "Other", Bundle: otherRef, BundleDigest: f.other.manifest},
	} {
		if err := db.Write.WithContext(ctx).Create(row).Error; err != nil {
			t.Fatalf("create %T: %v", row, err)
		}
	}
	f.registry.tags[acmeRef] = f.acme.manifest
	f.registry.tags[otherRef] = f.other.manifest
	f.service = newService(store.New(db.Write, db.Read), Options{Dir: f.dir, PlatformBundle: platformRef}, []source{f.registry}, []source{f.registry})
	return f
}

func asPool(poolID string) context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypePool, PoolID: poolID})
}

// read reads content whole, checking that what was served is the digest it
// was served as.
func read(t *testing.T, content services.AssetContent) []byte {
	t.Helper()
	defer content.Body.Close()
	data, err := io.ReadAll(content.Body)
	if err != nil {
		t.Fatalf("read %s: %v", content.Digest, err)
	}
	if got := digestOf(data); got != content.Digest {
		t.Fatalf("served %s as %s", got, content.Digest)
	}
	if int64(len(data)) != content.Size {
		t.Fatalf("served %d bytes of %s, declared %d", len(data), content.Digest, content.Size)
	}
	return data
}

func wantStatus(t *testing.T, err error, status int) {
	t.Helper()
	if err == nil {
		t.Fatalf("answered, want status %d", status)
	}
	var statusErr apperrors.StatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode() != status {
		t.Fatalf("err = %v, want status %d", err, status)
	}
}

func TestAPoolIsAnsweredWhatItsScopePins(t *testing.T) {
	f := newFixture(t)
	ctx := asPool("pool-1")

	// Its own harness's bundle, the release's platform index, and the
	// platform manifest the index lists.
	for _, tc := range []struct{ name, digest, mediaType string }{
		{"discobox-harness-acme", f.acme.manifest, imagecache.MediaTypeOCIManifest},
		{"discobox-platform", f.index, imagecache.MediaTypeOCIIndex},
		{"discobox-platform", f.platform.manifest, imagecache.MediaTypeOCIManifest},
	} {
		content, err := f.service.AssetManifest(ctx, "pool-1", tc.name, tc.digest)
		if err != nil {
			t.Fatalf("manifest %s of %s: %v", tc.digest, tc.name, err)
		}
		if content.MediaType != tc.mediaType {
			t.Fatalf("manifest %s served as %q, want %q", tc.digest, content.MediaType, tc.mediaType)
		}
		read(t, content)
	}
	// And every blob beneath them.
	for _, digest := range []string{f.acme.config, f.acme.layer, f.platform.config, f.platform.layer} {
		content, err := f.service.AssetBlob(ctx, "pool-1", digest)
		if err != nil {
			t.Fatalf("blob %s: %v", digest, err)
		}
		read(t, content)
	}
}

func TestAPoolIsRefusedAnotherProjectsManifest(t *testing.T) {
	f := newFixture(t)
	ctx := asPool("pool-1")

	// Under its own name, and under the name of a bundle pool-1 does pin.
	_, err := f.service.AssetManifest(ctx, "pool-1", "discobox-harness-other", f.other.manifest)
	wantStatus(t, err, http.StatusNotFound)
	_, err = f.service.AssetManifest(ctx, "pool-1", "discobox-harness-acme", f.other.manifest)
	wantStatus(t, err, http.StatusNotFound)
	_, err = f.service.ResolveAssetName(ctx, "pool-1", "discobox-harness-other")
	wantStatus(t, err, http.StatusNotFound)
	// The pool that serves project-2 is answered it.
	content, err := f.service.AssetManifest(asPool("pool-2"), "pool-2", "discobox-harness-other", f.other.manifest)
	if err != nil {
		t.Fatalf("pool-2 reading its own manifest: %v", err)
	}
	read(t, content)
}

func TestAPoolIsRefusedABlobNoManifestInItsScopeReferences(t *testing.T) {
	f := newFixture(t)
	ctx := asPool("pool-1")

	// Another project's layer, which pool-2 reads, so the store holds it.
	content, err := f.service.AssetBlob(asPool("pool-2"), "pool-2", f.other.layer)
	if err != nil {
		t.Fatalf("pool-2 reading its own layer: %v", err)
	}
	read(t, content)
	_, err = f.service.AssetBlob(ctx, "pool-1", f.other.layer)
	wantStatus(t, err, http.StatusNotFound)
	// A blob the store holds that no manifest references at all.
	stray := []byte("referenced by nothing")
	if err := imagecache.Open(f.dir).PutBlob(digestOf(stray), bytes.NewReader(stray)); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.AssetBlob(ctx, "pool-1", digestOf(stray))
	wantStatus(t, err, http.StatusNotFound)
	// A manifest is no blob of the asset API's blob read.
	_, err = f.service.AssetBlob(ctx, "pool-1", f.acme.manifest)
	wantStatus(t, err, http.StatusNotFound)
}

func TestAPoolReadsOnlyAsItself(t *testing.T) {
	f := newFixture(t)
	_, err := f.service.AssetManifest(asPool("pool-2"), "pool-1", "discobox-harness-acme", f.acme.manifest)
	wantStatus(t, err, http.StatusForbidden)
	_, err = f.service.AssetBlob(auth.WithPrincipal(context.Background(), auth.Principal{Type: auth.PrincipalTypeSandbox, PoolID: "pool-1"}), "pool-1", f.acme.layer)
	wantStatus(t, err, http.StatusForbidden)
}

func TestABlobThatDoesNotMatchItsDigestIsNeverServed(t *testing.T) {
	f := newFixture(t)
	ctx := asPool("pool-1")

	// Bytes a source hands in that are not the digest's are refused, and
	// nothing is stored under the digest.
	f.registry.corrupt[f.acme.layer] = true
	_, err := f.service.AssetBlob(ctx, "pool-1", f.acme.layer)
	wantStatus(t, err, http.StatusBadGateway)
	if f.service.has(f.acme.layer) {
		t.Fatal("a blob that did not match its digest was stored")
	}
	f.registry.corrupt[f.acme.layer] = false
	read(t, mustBlob(t, f, f.acme.layer))

	// A blob damaged on disk after it was stored fails the read that would
	// end it, and is given up, so the next read fetches it again.
	path := filepath.Join(f.dir, "blobs", "sha256", strings.TrimPrefix(f.acme.layer, "sha256:"))
	damaged := []byte("layer of acmf")
	if err := os.WriteFile(path, damaged, 0o600); err != nil {
		t.Fatal(err)
	}
	content := mustBlob(t, f, f.acme.layer)
	if got, err := io.ReadAll(content.Body); err == nil || int64(len(got)) >= content.Size {
		t.Fatalf("read %d of %d bytes of a damaged blob, %v; want fewer and an error", len(got), content.Size, err)
	}
	_ = content.Body.Close()
	if f.service.has(f.acme.layer) {
		t.Fatal("a damaged blob was kept")
	}
	read(t, mustBlob(t, f, f.acme.layer))
}

func mustBlob(t *testing.T, f *fixture, digest string) services.AssetContent {
	t.Helper()
	content, err := f.service.AssetBlob(asPool("pool-1"), "pool-1", digest)
	if err != nil {
		t.Fatalf("blob %s: %v", digest, err)
	}
	return content
}

func TestConcurrentReadsOfABlobShareOneFetch(t *testing.T) {
	f := newFixture(t)
	// The manifest first, read for the config, so that what is counted is the
	// layer alone.
	read(t, mustBlob(t, f, f.acme.config))
	before := f.registry.fetches.Load()
	f.registry.gate = make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			content, err := f.service.AssetBlob(asPool("pool-1"), "pool-1", f.acme.layer)
			if err != nil {
				errs <- err
				return
			}
			_, err = io.Copy(io.Discard, content.Body)
			_ = content.Body.Close()
			errs <- err
		})
	}
	// Let every read reach the fetch before it is answered.
	time.Sleep(100 * time.Millisecond)
	close(f.registry.gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := f.registry.fetches.Load() - before; got != 1 {
		t.Fatalf("8 reads fetched the layer %d times, want 1", got)
	}
}

func TestResolvingANameAnswersItsPin(t *testing.T) {
	f := newFixture(t)
	ctx := asPool("pool-1")

	// A harness's tag that moved is not followed: the name answers the digest
	// the harness config is pinned to until it is re-pinned.
	f.registry.tags[acmeRef] = f.registry.image(t, "acme v2").manifest
	content, err := f.service.ResolveAssetName(ctx, "pool-1", "discobox-harness-acme")
	if err != nil {
		t.Fatal(err)
	}
	if content.Digest != f.acme.manifest {
		t.Fatalf("resolved %s, want the pinned %s", content.Digest, f.acme.manifest)
	}
	read(t, content)
	// The platform is what the release's reference names now, asked afresh
	// even while its last resolution is recent.
	if content, err := f.service.ResolveAssetName(ctx, "pool-1", "discobox-platform"); err != nil {
		t.Fatal(err)
	} else {
		read(t, content)
	}
	f.registry.tags[platformRef] = f.registry.index(t, f.registry.image(t, "platform v2"))
	content, err = f.service.ResolveAssetName(ctx, "pool-1", "discobox-platform")
	if err != nil {
		t.Fatal(err)
	}
	if want := f.registry.tags[platformRef]; content.Digest != want {
		t.Fatalf("resolved %s, want the moved tag's %s", content.Digest, want)
	}
	read(t, content)
	_, err = f.service.ResolveAssetName(ctx, "pool-1", "discobox-nothing")
	wantStatus(t, err, http.StatusNotFound)
}

// A platform digest once answered stays answered after the tag moves: a pool
// part way through fetching the platform it was handed is not cut off.
func TestAPlatformDigestOnceAnsweredStaysInScope(t *testing.T) {
	f := newFixture(t)
	ctx := asPool("pool-1")
	read(t, mustBlob(t, f, f.platform.layer))
	moved := f.registry.image(t, "platform v2")
	f.registry.tags[platformRef] = f.registry.index(t, moved)
	if content, err := f.service.ResolveAssetName(ctx, "pool-1", "discobox-platform"); err != nil {
		t.Fatal(err)
	} else {
		read(t, content)
	}
	for _, digest := range []string{f.platform.layer, moved.layer} {
		read(t, mustBlob(t, f, digest))
	}
	content, err := f.service.AssetManifest(ctx, "pool-1", "discobox-platform", f.index)
	if err != nil {
		t.Fatalf("the old platform index after the tag moved: %v", err)
	}
	read(t, content)
}

// A platform that cannot be resolved is the server's fault, not a read out of
// scope, and it is not asked about again on every read.
func TestAnUnresolvablePlatformIsABadGatewayNotANotFound(t *testing.T) {
	f := newFixture(t)
	f.registry.down = true
	_, err := f.service.AssetManifest(asPool("pool-1"), "pool-1", "discobox-platform", f.index)
	wantStatus(t, err, http.StatusBadGateway)
	_, err = f.service.AssetBlob(asPool("pool-1"), "pool-1", f.platform.layer)
	wantStatus(t, err, http.StatusBadGateway)
	// What else is in scope is still answered.
	read(t, mustBlob(t, f, f.acme.layer))
	// And once it resolves, after the wait a failure is held for, it is too.
	f.registry.down = false
	f.service.now = func() time.Time { return time.Now().Add(platformRetryFailed) }
	read(t, mustBlob(t, f, f.platform.layer))
}

func TestANameTwoPinsShareIsAmbiguous(t *testing.T) {
	f := newFixture(t)
	fork := f.registry.image(t, "fork")
	if err := f.service.store.CreateHarnessConfig(context.Background(), &model.HarnessConfig{
		ID: "hc-3", ProjectID: "project-1", Slug: "fork", Name: "Fork",
		Bundle: "example.com/fork/discobox-harness-acme:v1", BundleDigest: fork.manifest,
	}); err != nil {
		t.Fatal(err)
	}
	_, err := f.service.ResolveAssetName(asPool("pool-1"), "pool-1", "discobox-harness-acme")
	wantStatus(t, err, http.StatusConflict)
	// Each pin is still answered by its digest.
	content, err := f.service.AssetManifest(asPool("pool-1"), "pool-1", "discobox-harness-acme", fork.manifest)
	if err != nil {
		t.Fatal(err)
	}
	read(t, content)
}

func TestADamagedManifestIsFetchedAgain(t *testing.T) {
	f := newFixture(t)
	read(t, mustBlob(t, f, f.acme.layer))
	path := filepath.Join(f.dir, "blobs", "sha256", strings.TrimPrefix(f.acme.manifest, "sha256:"))
	if err := os.WriteFile(path, []byte(`{"schemaVersion":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A fresh service, as after a restart, has no copy of it in memory.
	f.service = newService(f.service.store, Options{Dir: f.dir, PlatformBundle: platformRef}, []source{f.registry}, []source{f.registry})
	content, err := f.service.AssetManifest(asPool("pool-1"), "pool-1", "discobox-harness-acme", f.acme.manifest)
	if err != nil {
		t.Fatalf("a damaged manifest: %v", err)
	}
	read(t, content)
	read(t, mustBlob(t, f, f.acme.config))
}

// A blob leaves the store when no pin of any project reaches it any more, and
// not before it is old enough that nothing can be reading it for a new pin.
func TestPruneRemovesWhatNoPinReaches(t *testing.T) {
	f := newFixture(t)
	read(t, mustBlob(t, f, f.acme.layer))
	content, err := f.service.AssetBlob(asPool("pool-2"), "pool-2", f.other.layer)
	if err != nil {
		t.Fatal(err)
	}
	read(t, content)
	read(t, mustBlob(t, f, f.platform.layer))
	// project-2's harness is repinned to something else.
	moved := f.registry.image(t, "other v2")
	if err := f.service.store.UpdateHarnessConfig(context.Background(), &model.HarnessConfig{ID: "hc-2", ProjectID: "project-2", Slug: "other", Name: "Other", Bundle: otherRef, BundleDigest: moved.manifest}); err != nil {
		t.Fatal(err)
	}
	if removed, err := f.service.Prune(context.Background()); err != nil || removed != 0 {
		t.Fatalf("a sweep inside the grace removed %d, %v", removed, err)
	}
	f.service.now = func() time.Time { return time.Now().Add(pruneGrace + platformRetry) }
	removed, err := f.service.Prune(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// What was fetched for project-2's old pin: its manifest and its layer.
	if removed != 2 {
		t.Fatalf("removed %d blobs, want other's manifest and layer", removed)
	}
	for _, digest := range []string{f.other.manifest, f.other.layer} {
		if f.service.has(digest) {
			t.Fatalf("%s no pin reaches was kept", digest)
		}
	}
	for _, digest := range []string{f.acme.manifest, f.acme.layer, f.index, f.platform.manifest, f.platform.layer} {
		if !f.service.has(digest) {
			t.Fatalf("%s a pin reaches was removed", digest)
		}
	}
	// Nothing goes while the platform cannot be resolved.
	f.registry.down = true
	f.service.now = func() time.Time { return time.Now().Add(2 * (pruneGrace + platformRetry)) }
	f.service.platform = &platformPins{ref: platformRef, resolve: f.service.resolve, now: f.service.now}
	if _, err := f.service.Prune(context.Background()); err == nil {
		t.Fatal("pruned with the platform unresolved")
	}
}

// The daemon is never asked for a bundle a project supplied: what it holds is
// this host's, not the project's.
func TestTheDaemonHoldsNothingOfAProjects(t *testing.T) {
	d := newDaemonSource()
	if err := d.fetch(context.Background(), pin{ref: acmeRef, digest: "sha256:" + strings.Repeat("a", 64)}, "sha256:"+strings.Repeat("b", 64), false, nil); !errors.Is(err, errNotHere) {
		t.Fatalf("fetch for a project's pin = %v, want errNotHere", err)
	}
	if _, err := d.resolve(context.Background(), acmeRef, false, nil); !errors.Is(err, errNotHere) {
		t.Fatalf("resolve of a project's reference = %v, want errNotHere", err)
	}
}

// Only what the release names is pulled with the server's own registry
// credentials; a bundle a project supplied is pulled without them.
func TestOnlyTheReleasesBundlesArePulledWithTheServersCredentials(t *testing.T) {
	f := newFixture(t)
	read(t, mustBlob(t, f, f.platform.layer))
	read(t, mustBlob(t, f, f.acme.layer))
	if !f.registry.releases[platformRef] {
		t.Fatalf("the platform bundle was pulled without the server's credentials")
	}
	if f.registry.releases[acmeRef] {
		t.Fatalf("a project's bundle was pulled with the server's credentials")
	}
}
