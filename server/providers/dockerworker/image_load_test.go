package dockerworker

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/imagecache"
	"github.com/discobox-ai/discobox/imagecache/imagecachetest"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// fakeLoadDaemon loads archives the way either of Docker's image stores does:
// the containerd store keeps the registry digest an archive's index.json gives
// an image, and the classic store keeps none until a pull by that digest
// records it.
type fakeLoadDaemon struct {
	containerd bool
	// digestPullFails fails a pull by digest, as an unreachable registry does.
	digestPullFails bool

	mu      sync.Mutex
	images  map[string]fakeImage
	loads   []string
	pulls   []string
	removed []string
}

type fakeImage struct {
	id          string
	repoDigests []string
}

func newFakeLoadDaemon(containerd bool) *fakeLoadDaemon {
	return &fakeLoadDaemon{containerd: containerd, images: map[string]fakeImage{}}
}

func repositoryOf(reference string) string {
	if colon := strings.LastIndex(reference, ":"); colon > strings.LastIndex(reference, "/") {
		return reference[:colon]
	}
	return reference
}

func (d *fakeLoadDaemon) serveHTTP(w http.ResponseWriter, request *http.Request) {
	path := stripDockerAPIVersion(request.URL.Path)
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case request.Method == http.MethodGet && path == "/_ping":
		w.Header().Set("Api-Version", "1.51")
		w.WriteHeader(http.StatusOK)
	case request.Method == http.MethodGet && path == "/info":
		status := [][2]string{{"Backing Filesystem", "extfs"}}
		if d.containerd {
			status = [][2]string{{"driver-type", containerdSnapshotter}}
		}
		architecture := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[runtime.GOARCH]
		writeDockerJSON(w, map[string]any{"OSType": "linux", "Architecture": architecture, "DriverStatus": status})
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
		reference, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json"))
		image, ok := d.images[reference]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			writeDockerJSON(w, map[string]string{"message": "No such image: " + reference})
			return
		}
		writeDockerJSON(w, map[string]any{"Id": image.id, "RepoDigests": image.repoDigests})
	case request.Method == http.MethodPost && path == "/images/load":
		d.load(w, request)
	case request.Method == http.MethodPost && path == "/images/create":
		d.pull(w, request)
	case request.Method == http.MethodDelete && strings.HasPrefix(path, "/images/"):
		reference, _ := url.PathUnescape(strings.TrimPrefix(path, "/images/"))
		delete(d.images, reference)
		d.removed = append(d.removed, reference)
		writeDockerJSON(w, []map[string]string{{"Untagged": reference}})
	default:
		http.Error(w, request.Method+" "+path, http.StatusNotFound)
	}
}

func (d *fakeLoadDaemon) load(w http.ResponseWriter, request *http.Request) {
	files := map[string][]byte{}
	reader := tar.NewReader(request.Body)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		files[header.Name], _ = io.ReadAll(reader)
	}
	var index struct {
		Manifests []struct {
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	var docker []struct {
		Config string   `json:"Config"`
		Layers []string `json:"Layers"`
	}
	if json.Unmarshal(files["index.json"], &index) != nil || len(index.Manifests) != 1 ||
		json.Unmarshal(files["manifest.json"], &docker) != nil || len(docker) != 1 {
		http.Error(w, "not an image archive", http.StatusBadRequest)
		return
	}
	for _, blob := range append([]string{docker[0].Config}, docker[0].Layers...) {
		if _, ok := files[blob]; !ok {
			http.Error(w, "archive lacks "+blob, http.StatusBadRequest)
			return
		}
	}
	reference := index.Manifests[0].Annotations[imagecache.AnnotationImageName]
	image := fakeImage{id: "sha256:" + strings.TrimPrefix(docker[0].Config, "blobs/sha256/")}
	if d.containerd {
		top := index.Manifests[0].Digest
		image = fakeImage{id: top, repoDigests: []string{repositoryOf(reference) + "@" + top}}
	}
	d.images[reference] = image
	d.loads = append(d.loads, reference)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"stream":"Loaded image: ` + reference + `\n"}` + "\n"))
}

func (d *fakeLoadDaemon) pull(w http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	from, tag := query.Get("fromImage"), query.Get("tag")
	w.Header().Set("Content-Type", "application/json")
	if strings.HasPrefix(tag, "sha256:") {
		d.pulls = append(d.pulls, from+"@"+tag)
		if d.digestPullFails {
			_, _ = w.Write([]byte(`{"errorDetail":{"message":"registry unreachable"},"error":"registry unreachable"}` + "\n"))
			return
		}
		for reference, image := range d.images {
			if repositoryOf(reference) == from {
				image.repoDigests = append(image.repoDigests, from+"@"+tag)
				d.images[reference] = image
			}
		}
		_, _ = w.Write([]byte(`{"status":"Digest: ` + tag + `"}` + "\n"))
		return
	}
	d.pulls = append(d.pulls, from+":"+tag)
	d.images[from+":"+tag] = fakeImage{id: "sha256:pulled", repoDigests: []string{from + "@sha256:pulled"}}
	_, _ = w.Write([]byte(`{"id":"layer1","status":"Pull complete"}` + "\n"))
}

func (d *fakeLoadDaemon) snapshot() (loads, pulls, removed []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.loads), slices.Clone(d.pulls), slices.Clone(d.removed)
}

// stagedCache is an image cache holding one image, staged from a registry the
// way the CLI stages one.
func stagedCache(t *testing.T) (*imagecache.Layout, imagecachetest.Image) {
	t.Helper()
	registry := imagecachetest.NewRegistry(t)
	image := registry.Publish("x/agent", "v1", []byte("base"))
	cache := imagecache.Open(t.TempDir())
	if _, err := cache.Stage(context.Background(), []string{image.Reference}, imagecache.Options{Client: registry.Client()}); err != nil {
		t.Fatal(err)
	}
	return cache, image
}

type progressLog struct {
	mu      sync.Mutex
	reports []sandbox.PoolProvisionProgress
}

func (l *progressLog) phases() []sandbox.PoolProvisionPhase {
	l.mu.Lock()
	defer l.mu.Unlock()
	var phases []sandbox.PoolProvisionPhase
	for _, report := range l.reports {
		phases = append(phases, report.Phase)
	}
	return phases
}

func loadEngine(t *testing.T, daemon *fakeLoadDaemon, cache *imagecache.Layout, image string) (*Engine, *progressLog, string) {
	t.Helper()
	return localityEngine(t, daemon, cache, image, DaemonOnThisMachine)
}

func localityEngine(t *testing.T, daemon *fakeLoadDaemon, cache *imagecache.Layout, image string, locality DaemonLocality) (*Engine, *progressLog, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(daemon.serveHTTP))
	t.Cleanup(server.Close)
	log := &progressLog{}
	engine := &Engine{
		driver: &preloadDriver{url: server.URL, locality: locality},
		cfg: Config{
			Image:              image,
			DockerReadyTimeout: 2 * time.Second,
			ImageCache:         cache,
			ProgressReporter: func(_ context.Context, _ string, progress sandbox.PoolProvisionProgress) {
				log.mu.Lock()
				defer log.mu.Unlock()
				log.reports = append(log.reports, progress)
			},
		},
	}
	return engine, log, server.URL
}

// A containerd daemon keeps the digest the archive gives the image, so a
// staged image is loaded, narrated as a load, and never pulled.
func TestStageImagesLoadsFromTheImageCache(t *testing.T) {
	cache, image := stagedCache(t)
	daemon := newFakeLoadDaemon(true)
	engine, _, _ := loadEngine(t, daemon, cache, testPoolImage)

	var loading, sawBytes bool
	err := engine.StageImages(context.Background(), &model.Pool{ID: "pool_1"}, []string{image.Reference},
		func(progress sandbox.PreloadProgress) {
			if progress.Loading {
				loading = true
				sawBytes = sawBytes || (progress.Pull != nil && progress.Pull.Current > 0)
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	loads, pulls, _ := daemon.snapshot()
	if !slices.Equal(loads, []string{image.Reference}) || len(pulls) != 0 {
		t.Fatalf("loads = %v, pulls = %v; want one load and no pull", loads, pulls)
	}
	if !loading || !sawBytes {
		t.Fatal("the load was not reported as a load with its byte counts")
	}
}

// A classic daemon records no registry digest for a load, and a sandbox is
// pinned to that digest, so the load is followed by a pull by digest — which
// the daemon answers without a layer, and which records it.
func TestALoadIntoAClassicDaemonRecordsTheRegistryDigest(t *testing.T) {
	cache, image := stagedCache(t)
	daemon := newFakeLoadDaemon(false)
	engine, _, _ := loadEngine(t, daemon, cache, testPoolImage)

	if err := engine.StageImages(context.Background(), &model.Pool{ID: "pool_1"}, []string{image.Reference}, nil); err != nil {
		t.Fatal(err)
	}
	loads, pulls, removed := daemon.snapshot()
	digestReference := repositoryOf(image.Reference) + "@" + image.Index
	if !slices.Equal(loads, []string{image.Reference}) || !slices.Equal(pulls, []string{digestReference}) || len(removed) != 0 {
		t.Fatalf("loads = %v, pulls = %v, removed = %v; want a load and a pull by %s", loads, pulls, removed, digestReference)
	}
	if !slices.Contains(daemon.images[image.Reference].repoDigests, digestReference) {
		t.Fatalf("repo digests = %v, want %s", daemon.images[image.Reference].repoDigests, digestReference)
	}
}

// A loaded image whose digest cannot be recorded would sit on the daemon
// forever with a pin it can never match, so it is taken back and the image is
// pulled the ordinary way.
func TestAClassicLoadWhoseDigestCannotBeRecordedIsTakenBack(t *testing.T) {
	cache, image := stagedCache(t)
	daemon := newFakeLoadDaemon(false)
	daemon.digestPullFails = true
	engine, _, _ := loadEngine(t, daemon, cache, testPoolImage)

	if err := engine.StageImages(context.Background(), &model.Pool{ID: "pool_1"}, []string{image.Reference}, nil); err != nil {
		t.Fatal(err)
	}
	_, pulls, removed := daemon.snapshot()
	if !slices.Equal(removed, []string{image.Reference}) {
		t.Fatalf("removed = %v, want the loaded image taken back", removed)
	}
	if len(pulls) != 2 || pulls[1] != image.Reference {
		t.Fatalf("pulls = %v, want the digest pull and then the ordinary one", pulls)
	}
}

// An image the cache does not hold is pulled, exactly as before there was one.
func TestAnImageNotInTheCacheIsPulled(t *testing.T) {
	daemon := newFakeLoadDaemon(true)
	engine, _, _ := loadEngine(t, daemon, imagecache.Open(t.TempDir()), testPoolImage)

	if err := engine.StageImages(context.Background(), &model.Pool{ID: "pool_1"}, []string{"ghcr.io/x/harness-shell:v1"}, nil); err != nil {
		t.Fatal(err)
	}
	loads, pulls, _ := daemon.snapshot()
	if len(loads) != 0 || !slices.Equal(pulls, []string{"ghcr.io/x/harness-shell:v1"}) {
		t.Fatalf("loads = %v, pulls = %v; want the image pulled", loads, pulls)
	}
}

// The pool-agent image takes the same route, reported as loading rather than
// as pulling.
func TestThePoolImageLoadsFromTheImageCache(t *testing.T) {
	cache, image := stagedCache(t)
	daemon := newFakeLoadDaemon(true)
	engine, log, daemonURL := loadEngine(t, daemon, cache, image.Reference)
	cli, err := testDockerClient(daemonURL)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	launched, err := engine.ensureImage(context.Background(), NewDockerClientLease(cli, DaemonOnThisMachine, nil), "pool_1")
	if err != nil {
		t.Fatal(err)
	}
	if launched != image.Reference {
		t.Fatalf("launched %q, want %q", launched, image.Reference)
	}
	phases := log.phases()
	if !slices.Contains(phases, sandbox.PoolPhaseLoadingPoolImage) || slices.Contains(phases, sandbox.PoolPhasePullingPoolImage) {
		t.Fatalf("phases = %v, want a load and no pull", phases)
	}
}

// A daemon on another machine is closer to its registry than to this machine's
// disk, so an image staged here is pulled by it rather than sent over the
// network twice — and the driver that reached it is what says so.
func TestAnImageIsPulledIntoADaemonElsewhere(t *testing.T) {
	cache, image := stagedCache(t)
	daemon := newFakeLoadDaemon(true)
	engine, _, _ := localityEngine(t, daemon, cache, testPoolImage, DaemonElsewhere)

	if err := engine.StageImages(context.Background(), &model.Pool{ID: "pool_1"}, []string{image.Reference}, nil); err != nil {
		t.Fatal(err)
	}
	loads, pulls, _ := daemon.snapshot()
	if len(loads) != 0 || !slices.Equal(pulls, []string{image.Reference}) {
		t.Fatalf("loads = %v, pulls = %v; want the image pulled by the daemon itself", loads, pulls)
	}
}

// The answer for a daemon named by a host URL, which is how the docker and exec
// providers answer for theirs.
func TestDaemonLocalityForHost(t *testing.T) {
	for host, want := range map[string]DaemonLocality{
		"unix:///var/run/docker.sock":    DaemonOnThisMachine,
		"npipe:////./pipe/docker_engine": DaemonOnThisMachine,
		"ssh://root@10.0.0.2":            DaemonElsewhere,
		"tcp://10.0.0.2:2375":            DaemonElsewhere,
		// Forwarded from anywhere, so it is not taken at its word.
		"tcp://localhost:2375": DaemonElsewhere,
		"":                     DaemonElsewhere,
	} {
		if got := DaemonLocalityForHost(host); got != want {
			t.Errorf("DaemonLocalityForHost(%q) = %v, want %v", host, got, want)
		}
	}
}
