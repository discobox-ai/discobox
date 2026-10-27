package dockerworker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/discobox-ai/discobox/pkg/imagecache"
	poolagent "github.com/discobox-ai/discobox/pool-agent"
	"github.com/discobox-ai/discobox/server/internal/model"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
	"github.com/discobox-ai/discobox/server/providers/poolruntime"
)

// fakeMaker is a PoolMaker whose Box answers what the test says, and which
// records whether it was asked to enroll.
type fakeMaker struct {
	box      sandbox.PoolBox
	boxErr   error
	enrolled bool
}

func (m *fakeMaker) Bootstrap(context.Context) (poolagent.Bootstrap, error) {
	return poolagent.Bootstrap{ProjectID: "project-1", PoolID: "pool-1"}, nil
}

func (m *fakeMaker) Box(context.Context, imagecache.Platform) (sandbox.PoolBox, error) {
	return m.box, m.boxErr
}

func (m *fakeMaker) Enroll(context.Context, string) error {
	m.enrolled = true
	return nil
}

// infoDaemon answers the one request resolving a pool's box makes of its
// daemon: what platform it runs.
func infoDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if stripDockerAPIVersion(r.URL.Path) == "/info" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"OSType":"linux","Architecture":"x86_64"}`))
			return
		}
		t.Errorf("unexpected daemon request %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)
	return server
}

// A running pool's box is never taken down over its bundles (ADR 26-10-24-953
// §5): a release that does not resolve leaves the box as it is, without
// closing placement or failing the reconcile that would repair it, and only
// newer intent, through begin, stops the pass.
func TestApplyMovedPoolBundlesKeepsARunningBox(t *testing.T) {
	cli, err := testDockerClient(infoDaemon(t).URL)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	engine := newTestEngine(t, Config{})
	pool := &model.Pool{ID: "pool-1", ProjectID: "project-1"}
	recorded := poolruntime.BoxRecord{Agent: "discobox-agent@sha256:a", Bundles: []string{"discobox-pool-agent-bundle@sha256:b"}}

	began := false
	begin := func(context.Context) error { began = true; return nil }
	unresolvable := &fakeMaker{boxErr: errors.New("registry unreachable")}
	got, err := engine.applyMovedPoolBundles(context.Background(), cli, "container-1", pool, unresolvable, recorded, begin)
	if err != nil || began || got.Agent != recorded.Agent || len(got.Bundles) != 1 {
		t.Fatalf("an unresolvable release: record %+v, began %v, err %v; want the box kept as it is", got, began, err)
	}

	same := &fakeMaker{box: sandbox.PoolBox{Bundles: []sandbox.BundlePin{{Name: "discobox-pool-agent-bundle", Digest: "sha256:b"}}}}
	if _, err := engine.applyMovedPoolBundles(context.Background(), cli, "container-1", pool, same, recorded, begin); err != nil || began {
		t.Fatalf("unmoved bundles: began %v, err %v; want nothing done", began, err)
	}

	superseded := errors.New("pool generation changed")
	moved := &fakeMaker{box: sandbox.PoolBox{Bundles: []sandbox.BundlePin{{Name: "discobox-pool-agent-bundle", Digest: "sha256:c"}}}}
	_, err = engine.applyMovedPoolBundles(context.Background(), cli, "container-1", pool, moved, recorded, func(context.Context) error { return superseded })
	if !errors.Is(err, superseded) || moved.enrolled {
		t.Fatalf("moved bundles under newer intent: err %v, enrolled %v; want begin's error and nothing applied", err, moved.enrolled)
	}
}
