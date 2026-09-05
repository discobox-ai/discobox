package dockerworker

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/discobox-ai/discobox/devimage"
	"github.com/moby/moby/client"
)

type fakeImageDaemon struct {
	mu          sync.Mutex
	id          string
	images      map[string]string
	loadImages  map[string]string
	archive     []byte
	loadCount   int
	loadPayload []byte
}

func (d *fakeImageDaemon) serveHTTP(w http.ResponseWriter, request *http.Request) {
	path := stripDockerAPIVersion(request.URL.Path)
	switch {
	case request.Method == http.MethodGet && path == "/info":
		writeDockerJSON(w, map[string]string{"ID": d.id})
	case request.Method == http.MethodGet && path == "/images/get":
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(d.archive)
	case request.Method == http.MethodPost && path == "/images/load":
		gzipReader, err := gzip.NewReader(request.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		payload, err := io.ReadAll(gzipReader)
		if closeErr := gzipReader.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d.mu.Lock()
		d.loadCount++
		d.loadPayload = payload
		for reference, id := range d.loadImages {
			d.images[reference] = id
			d.images[id] = id
		}
		d.mu.Unlock()
		writeDockerJSON(w, map[string]string{"stream": "loaded"})
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
		reference := strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json")
		reference, _ = url.PathUnescape(reference)
		d.mu.Lock()
		id, ok := d.images[reference]
		d.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			writeDockerJSON(w, map[string]string{"message": "No such image: " + reference})
			return
		}
		writeDockerJSON(w, map[string]string{"Id": id})
	default:
		http.Error(w, request.Method+" "+path, http.StatusNotFound)
	}
}

func TestDevelopmentImageSynchronizerTransfersMissingImagesOnce(t *testing.T) {
	images := []devimage.Image{
		{Reference: "discobox-pool-agent:dev-test", ID: "sha256:pool"},
		{Reference: "discobox-harness-codex:dev-test", ID: "sha256:harness"},
	}
	sourceDaemon := &fakeImageDaemon{
		id:      "source",
		images:  imageMap(images),
		archive: []byte("development image archive"),
	}
	destinationDaemon := &fakeImageDaemon{
		id:         "destination",
		images:     map[string]string{},
		loadImages: imageMap(images),
	}
	sourceServer := httptest.NewServer(http.HandlerFunc(sourceDaemon.serveHTTP))
	defer sourceServer.Close()
	destinationServer := httptest.NewServer(http.HandlerFunc(destinationDaemon.serveHTTP))
	defer destinationServer.Close()

	synchronizer, err := newDevelopmentImageSynchronizer(images, func() (*client.Client, error) {
		return testDockerClient(sourceServer.URL)
	})
	if err != nil {
		t.Fatal(err)
	}
	destination, err := testDockerClient(destinationServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()

	if err := synchronizer.Ensure(context.Background(), destination); err != nil {
		t.Fatal(err)
	}
	if err := synchronizer.Ensure(context.Background(), destination); err != nil {
		t.Fatal(err)
	}

	destinationDaemon.mu.Lock()
	defer destinationDaemon.mu.Unlock()
	if destinationDaemon.loadCount != 1 {
		t.Fatalf("load count = %d, want 1", destinationDaemon.loadCount)
	}
	if string(destinationDaemon.loadPayload) != string(sourceDaemon.archive) {
		t.Fatalf("loaded payload = %q, want %q", destinationDaemon.loadPayload, sourceDaemon.archive)
	}
}

func TestDevelopmentImageSynchronizerRejectsSourceDrift(t *testing.T) {
	images := []devimage.Image{{Reference: "discobox-pool-agent:dev-test", ID: "sha256:expected"}}
	sourceDaemon := &fakeImageDaemon{
		id:      "source",
		images:  map[string]string{images[0].Reference: "sha256:different"},
		archive: []byte("unused"),
	}
	destinationDaemon := &fakeImageDaemon{
		id:         "destination",
		images:     map[string]string{},
		loadImages: imageMap(images),
	}
	sourceServer := httptest.NewServer(http.HandlerFunc(sourceDaemon.serveHTTP))
	defer sourceServer.Close()
	destinationServer := httptest.NewServer(http.HandlerFunc(destinationDaemon.serveHTTP))
	defer destinationServer.Close()

	synchronizer, err := newDevelopmentImageSynchronizer(images, func() (*client.Client, error) {
		return testDockerClient(sourceServer.URL)
	})
	if err != nil {
		t.Fatal(err)
	}
	destination, err := testDockerClient(destinationServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()

	if err := synchronizer.Ensure(context.Background(), destination); err == nil || !strings.Contains(err.Error(), "manifest requires") {
		t.Fatalf("Ensure() error = %v, want source drift error", err)
	}
}

func TestDevelopmentImageSynchronizerIntegration(t *testing.T) {
	destinationHost := strings.TrimSpace(os.Getenv("DISCOBOX_DOCKER_IMAGE_SYNC_TEST_DESTINATION"))
	if destinationHost == "" {
		t.Skip("set DISCOBOX_DOCKER_IMAGE_SYNC_TEST_DESTINATION to an empty disposable Docker daemon")
	}
	reference := strings.TrimSpace(os.Getenv("DISCOBOX_DOCKER_IMAGE_SYNC_TEST_IMAGE"))
	if reference == "" {
		reference = "busybox:1.37.0"
	}
	source, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	inspect, err := source.ImageInspect(context.Background(), reference)
	if err != nil {
		t.Fatal(err)
	}
	synchronizer, err := newDevelopmentImageSynchronizer([]devimage.Image{{
		Reference: reference,
		ID:        inspect.ID,
	}}, func() (*client.Client, error) {
		return client.New(client.FromEnv)
	})
	if err != nil {
		t.Fatal(err)
	}
	destination, err := client.New(client.WithHost(destinationHost))
	if err != nil {
		t.Fatal(err)
	}
	defer destination.Close()

	if err := synchronizer.Ensure(context.Background(), destination); err != nil {
		t.Fatal(err)
	}
	loaded, err := destination.ImageInspect(context.Background(), reference)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != inspect.ID {
		t.Fatalf("destination image ID = %s, want %s", loaded.ID, inspect.ID)
	}
}

func imageMap(images []devimage.Image) map[string]string {
	out := make(map[string]string, len(images)*2)
	for _, image := range images {
		out[image.Reference] = image.ID
		out[image.ID] = image.ID
	}
	return out
}

func testDockerClient(host string) (*client.Client, error) {
	return client.New(client.WithHost(host), client.WithAPIVersion("1.51"))
}

func stripDockerAPIVersion(path string) string {
	if !strings.HasPrefix(path, "/v") {
		return path
	}
	if slash := strings.Index(path[1:], "/"); slash >= 0 {
		return path[slash+1:]
	}
	return path
}

func writeDockerJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		panic(err)
	}
}
