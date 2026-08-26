package dockerworker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// fakePullDaemon answers image inspect and image create, recording what was
// asked for.
type fakePullDaemon struct {
	mu      sync.Mutex
	present map[string]struct{}
	pulls   []string
	failing bool
	// script, when set, is the exact progress stream to replay.
	script []string
}

func (d *fakePullDaemon) serveHTTP(w http.ResponseWriter, request *http.Request) {
	path := stripDockerAPIVersion(request.URL.Path)
	switch {
	case request.Method == http.MethodGet && path == "/_ping":
		w.Header().Set("Api-Version", "1.51")
		w.WriteHeader(http.StatusOK)
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
		if len(d.script) > 0 {
			for _, line := range d.script {
				_, _ = w.Write([]byte(line + "\n"))
			}
			return
		}
		// Two layers, one downloading and one already present, which is the
		// shape a byte counter has to add up correctly.
		_, _ = w.Write([]byte(`{"id":"layer1","status":"Downloading","progressDetail":{"current":50,"total":100}}` + "\n"))
		_, _ = w.Write([]byte(`{"id":"layer2","status":"Already exists"}` + "\n"))
		_, _ = w.Write([]byte(`{"id":"layer1","status":"Pull complete","progressDetail":{"current":100,"total":100}}` + "\n"))
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
	if err := engine.ensureImage(context.Background(), cli, "pool_test"); err != nil {
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
	if err := engine.ensureImage(context.Background(), cli, "pool_test"); err != nil {
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
	err = engine.ensureImage(context.Background(), cli, "pool_test")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), testPoolImage) {
		t.Fatalf("error %q does not name the image", err)
	}
}

// The pull is the one phase of bringing a pool up that can say how far in it
// is, and it is where a sandbox waiting for a pool spends most of its wait on a
// cold host.
func TestEnsureImageReportsPullProgress(t *testing.T) {
	daemon := &fakePullDaemon{present: map[string]struct{}{}}
	server := httptest.NewServer(http.HandlerFunc(daemon.serveHTTP))
	defer server.Close()
	cli, err := testDockerClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	var reports []sandbox.PoolProvisionProgress
	engine := &Engine{cfg: Config{
		Image: testPoolImage,
		ProgressReporter: func(_ context.Context, poolID string, progress sandbox.PoolProvisionProgress) {
			if poolID != "pool_test" {
				t.Errorf("reported against pool %q", poolID)
			}
			reports = append(reports, progress)
		},
	}}
	if err := engine.ensureImage(context.Background(), cli, "pool_test"); err != nil {
		t.Fatal(err)
	}

	if len(reports) == 0 {
		t.Fatal("a pull reported nothing")
	}
	for _, report := range reports {
		if report.Phase != sandbox.PoolPhasePullingPoolImage {
			t.Fatalf("phase = %q, want %q", report.Phase, sandbox.PoolPhasePullingPoolImage)
		}
	}
	final := reports[len(reports)-1]
	if final.Pull == nil || !final.Pull.Done {
		t.Fatalf("the last report does not close the pull: %+v", final.Pull)
	}
	if final.Pull.Image != testPoolImage {
		t.Fatalf("pull image = %q, want %q", final.Pull.Image, testPoolImage)
	}
	// Both layers counted, including the one that was already present and
	// therefore reported no bytes at all.
	if final.Pull.Layers != 2 || final.Pull.LayersComplete != 2 {
		t.Fatalf("layers = %d/%d, want 2/2", final.Pull.LayersComplete, final.Pull.Layers)
	}
	if final.Pull.Current != 100 || final.Pull.Total != 100 {
		t.Fatalf("bytes = %d/%d, want 100/100", final.Pull.Current, final.Pull.Total)
	}
}

// A pool image already on the host says nothing: there is no pull to describe,
// and a phase on the row would outlive the work it names.
func TestEnsureImageReportsNothingWhenTheImageIsPresent(t *testing.T) {
	const image = "discobox-pool-agent:dev-test"
	daemon := &fakePullDaemon{present: map[string]struct{}{image: {}}}
	server := httptest.NewServer(http.HandlerFunc(daemon.serveHTTP))
	defer server.Close()
	cli, err := testDockerClient(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	reported := false
	engine := &Engine{cfg: Config{
		Image:            image,
		ProgressReporter: func(context.Context, string, sandbox.PoolProvisionProgress) { reported = true },
	}}
	if err := engine.ensureImage(context.Background(), cli, "pool_test"); err != nil {
		t.Fatal(err)
	}
	if reported {
		t.Fatal("reported a pull that never happened")
	}
}
