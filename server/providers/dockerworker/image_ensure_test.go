package dockerworker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakePullDaemon answers image inspect and image create, recording what was
// asked for.
type fakePullDaemon struct {
	mu      sync.Mutex
	present map[string]struct{}
	pulls   []string
	failing bool
}

func (d *fakePullDaemon) serveHTTP(w http.ResponseWriter, request *http.Request) {
	path := stripDockerAPIVersion(request.URL.Path)
	switch {
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/images/") && strings.HasSuffix(path, "/json"):
		reference, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(path, "/images/"), "/json"))
		d.mu.Lock()
		_, ok := d.present[reference]
		d.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			writeDockerJSON(w, map[string]string{"message": "No such image: " + reference})
			return
		}
		writeDockerJSON(w, map[string]string{"Id": "sha256:" + reference})
	case request.Method == http.MethodPost && path == "/images/create":
		query := request.URL.Query()
		reference := query.Get("fromImage")
		if tag := query.Get("tag"); tag != "" {
			reference += ":" + tag
		}
		d.mu.Lock()
		d.pulls = append(d.pulls, reference)
		d.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if d.failing {
			// A pull reports failure inside the stream, not in the status code.
			_, _ = w.Write([]byte(`{"errorDetail":{"message":"unauthorized"},"error":"unauthorized"}` + "\n"))
			return
		}
		_, _ = w.Write([]byte(`{"status":"Pull complete"}` + "\n"))
	default:
		http.Error(w, request.Method+" "+path, http.StatusNotFound)
	}
}

func (d *fakePullDaemon) pulled() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.pulls...)
}

const testPoolImage = "ghcr.io/discobox-ai/discobox-pool-agent:v1.2.3"

func TestEnsureImagePullsWhenAbsent(t *testing.T) {
	daemon := &fakePullDaemon{present: map[string]struct{}{}}
	server := httptest.NewServer(http.HandlerFunc(daemon.serveHTTP))
	defer server.Close()
	cli, err := testDockerClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	engine := &Engine{cfg: Config{Image: testPoolImage}}
	if err := engine.ensureImage(context.Background(), cli); err != nil {
		t.Fatal(err)
	}
	if pulls := daemon.pulled(); len(pulls) != 1 || pulls[0] != testPoolImage {
		t.Fatalf("pulls = %v, want [%s]", pulls, testPoolImage)
	}
}

// A development image tag exists on no registry, so pulling one that the image
// synchronizer has already placed would fail on exactly the images that are
// correctly in place.
func TestEnsureImageDoesNotPullWhenPresent(t *testing.T) {
	const image = "discobox-pool-agent:dev-test"
	daemon := &fakePullDaemon{present: map[string]struct{}{image: {}}}
	server := httptest.NewServer(http.HandlerFunc(daemon.serveHTTP))
	defer server.Close()
	cli, err := testDockerClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	engine := &Engine{cfg: Config{Image: image}}
	if err := engine.ensureImage(context.Background(), cli); err != nil {
		t.Fatal(err)
	}
	if pulls := daemon.pulled(); len(pulls) != 0 {
		t.Fatalf("pulls = %v, want none", pulls)
	}
}

// A pull that fails has to fail the reconcile. Docker reports it inside the
// progress stream, so a caller that does not read the stream sees success.
func TestEnsureImageReportsPullFailure(t *testing.T) {
	daemon := &fakePullDaemon{present: map[string]struct{}{}, failing: true}
	server := httptest.NewServer(http.HandlerFunc(daemon.serveHTTP))
	defer server.Close()
	cli, err := testDockerClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	engine := &Engine{cfg: Config{Image: testPoolImage}}
	err = engine.ensureImage(context.Background(), cli)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), testPoolImage) {
		t.Fatalf("error %q does not name the image", err)
	}
}
