package dockerworker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// preloadDriver hands the engine a Docker client onto the fake daemon, which is
// the only part of a driver preloading uses.
type preloadDriver struct {
	Driver
	url string
}

func (d *preloadDriver) AcquireDockerClient(context.Context, string) (*DockerClientLease, error) {
	cli, err := testDockerClient(d.url)
	if err != nil {
		return nil, err
	}
	return &DockerClientLease{Client: cli, release: func() { _ = cli.Close() }}, nil
}

func preloadEngine(t *testing.T, daemon *fakePullDaemon) (*Engine, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(daemon.serveHTTP))
	engine := &Engine{
		driver: &preloadDriver{url: server.URL},
		cfg: Config{
			Image: testPoolImage,
			// The fake answers at once; without this a daemon that did not
			// would hold the test for the production wait.
			DockerReadyTimeout: 2 * time.Second,
			ProgressReporter:   func(context.Context, string, sandbox.PoolProvisionProgress) {},
		},
	}
	return engine, server
}

// The images a sandbox will want are pulled onto a pool that is already up.
func TestStageImagesPullsEveryImage(t *testing.T) {
	daemon := &fakePullDaemon{present: map[string]struct{}{}}
	engine, server := preloadEngine(t, daemon)
	defer server.Close()

	images := []string{"ghcr.io/x/sandbox-agent:v1", "ghcr.io/x/harness-shell:v1"}
	var lines []string
	var sawBytes bool
	err := engine.StageImages(context.Background(), &model.Pool{ID: "pool_1"}, images,
		func(progress sandbox.PreloadProgress) {
			lines = append(lines, progress.Image)
			if progress.Total != len(images) {
				t.Errorf("total = %d, want %d", progress.Total, len(images))
			}
			if progress.Pull != nil && progress.Pull.Total > 0 {
				sawBytes = true
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	pulled := daemon.pulled()
	if len(pulled) != 2 {
		t.Fatalf("pulled %v, want both images", pulled)
	}
	// The last report closes the set, so a caller can render "n of n".
	if len(lines) == 0 || lines[len(lines)-1] != "" {
		t.Fatalf("reports = %v, want a closing report", lines)
	}
	// And the bytes reach the caller while they move: the image counts alone
	// sit unchanged for the whole of a multi-gigabyte pull.
	if !sawBytes {
		t.Fatal("no report carried the pull's byte counts")
	}
}

// One image that cannot be pulled must not cost the others. Preloading is an
// optimisation for a wait that would otherwise happen later; abandoning it
// halfway leaves the rest of that wait in place for no reason.
func TestStageImagesKeepsGoingPastAFailure(t *testing.T) {
	daemon := &fakePullDaemon{present: map[string]struct{}{}, failing: true}
	engine, server := preloadEngine(t, daemon)
	defer server.Close()

	images := []string{"ghcr.io/x/a:v1", "ghcr.io/x/b:v1", "ghcr.io/x/c:v1"}
	err := engine.StageImages(context.Background(), &model.Pool{ID: "pool_1"}, images, nil)
	if err == nil {
		t.Fatal("expected the failures to be reported")
	}
	if pulled := daemon.pulled(); len(pulled) != 3 {
		t.Fatalf("attempted %v, want all three tried despite the failures", pulled)
	}
	if !strings.Contains(err.Error(), "ghcr.io/x/c:v1") {
		t.Fatalf("error %q does not name the last image tried", err)
	}
}

// An image already on the daemon is not pulled again, which is what makes the
// second server start fast.
func TestStageImagesSkipsWhatIsAlreadyThere(t *testing.T) {
	const present = "ghcr.io/x/sandbox-agent:v1"
	daemon := &fakePullDaemon{present: map[string]struct{}{present: {}}}
	engine, server := preloadEngine(t, daemon)
	defer server.Close()

	if err := engine.StageImages(context.Background(), &model.Pool{ID: "pool_1"}, []string{present}, nil); err != nil {
		t.Fatal(err)
	}
	if pulled := daemon.pulled(); len(pulled) != 0 {
		t.Fatalf("pulled %v, want nothing", pulled)
	}
}
