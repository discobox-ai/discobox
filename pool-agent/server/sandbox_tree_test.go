package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pool-agent/assetcache"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// failingTreeRuntime exports a tree whose walk fails after the response has
// begun.
type failingTreeRuntime struct {
	*sandboxruntime.MemorySandboxRuntime
}

func (failingTreeRuntime) ExportTree(context.Context, string, assetcache.Delivery) (io.ReadCloser, error) {
	reader, writer := io.Pipe()
	go func() {
		_, _ = writer.Write(make([]byte, 1024))
		_ = writer.CloseWithError(errors.New("walk failed"))
	}()
	return reader, nil
}

// A walk that fails once the body has begun has to reach the caller as a
// failed read. Returning from the handler instead lets net/http end the chunked
// body cleanly, and the caller holds a short tar that ends like a whole one.
func TestExportTreeAbortsTheResponseWhenTheWalkFails(t *testing.T) {
	const projectID, poolID, sandboxID = "project-1", "pool-1", "sandbox-1"
	publicKey, sign := testPoolTokenSigner(t)
	router, err := NewRouter(Config{
		Identity:              Identity{ProjectID: projectID, PoolID: poolID},
		Runtime:               failingTreeRuntime{MemorySandboxRuntime: sandboxruntime.NewMemorySandboxRuntime()},
		ControlPlanePublicKey: publicKey,
	})
	if err != nil {
		t.Fatalf("new router: %v", err)
	}
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		server.URL+"/api/project/project-1/pool/pool-1/sandboxes/sandbox-1/tree", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+sign(projectID, poolID, sandboxID, ScopeSandboxRead))
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: the failure comes after the body has begun", resp.StatusCode)
	}
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("the body ended cleanly; a failed walk has to read as a failure")
	}
}

// pinRecordingRuntime records what an export was asked to read with.
type pinRecordingRuntime struct {
	*sandboxruntime.MemorySandboxRuntime
	pins   *assetcache.Delivery
	called *bool
	err    error
}

func (r pinRecordingRuntime) ExportTree(_ context.Context, _ string, delivered assetcache.Delivery) (io.ReadCloser, error) {
	*r.pins = delivered
	*r.called = true
	if r.err != nil {
		return nil, r.err
	}
	return io.NopCloser(strings.NewReader("")), nil
}

// The control plane owns the pins, so what an export reads with arrives on
// the request: the agent once, and each bundle in order, each as
// <name>@<digest>, with nothing saying which bundle is which. One that does
// not parse, or an agent with no bundles or bundles with no agent, is the
// caller's mistake, refused before anything is read. A sandbox delivered
// nothing has nothing to be read with, which is a conflict the caller can act
// on — by pinning it — not a failure (ADR 0129 §3, ADR 26-10-16-423 §10).
func TestExportTreeTakesThePinsFromTheRequest(t *testing.T) {
	const projectID, poolID, sandboxID = "project-1", "pool-1", "sandbox-1"
	agent := "discobox-agent@sha256:" + strings.Repeat("c", 64)
	platform := "sha256:" + strings.Repeat("a", 64)
	harness := "sha256:" + strings.Repeat("b", 64)
	pinned := []string{"discobox-platform@" + platform, "discobox-harness-shell@" + harness}
	want := assetcache.Delivery{
		Agent:   assetcache.Pin{Name: "discobox-agent", Digest: "sha256:" + strings.Repeat("c", 64)},
		Bundles: []assetcache.Pin{{Name: "discobox-platform", Digest: platform}, {Name: "discobox-harness-shell", Digest: harness}},
	}
	for name, tc := range map[string]struct {
		agent   string
		bundles []string
		err     error
		status  int
		want    assetcache.Delivery
	}{
		"pinned to its bundles":   {agent: agent, bundles: pinned, status: http.StatusOK, want: want},
		"delivered nothing":       {err: sandboxruntime.ErrUndelivered, status: http.StatusUnprocessableEntity},
		"bundles with no agent":   {bundles: pinned, status: http.StatusBadRequest},
		"an agent with no bundle": {agent: agent, status: http.StatusBadRequest},
		"no name":                 {agent: agent, bundles: []string{"@" + platform}, status: http.StatusBadRequest},
		"no digest":               {agent: agent, bundles: []string{"discobox-platform"}, status: http.StatusBadRequest},
		"a tag, not a digest":     {agent: agent, bundles: []string{"discobox-platform@latest"}, status: http.StatusBadRequest},
		"one malformed among two": {agent: agent, bundles: []string{pinned[0], "discobox-harness-shell@sha256:abc"}, status: http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			var got assetcache.Delivery
			var called bool
			publicKey, sign := testPoolTokenSigner(t)
			router, err := NewRouter(Config{
				Identity:              Identity{ProjectID: projectID, PoolID: poolID},
				Runtime:               pinRecordingRuntime{MemorySandboxRuntime: sandboxruntime.NewMemorySandboxRuntime(), pins: &got, called: &called, err: tc.err},
				ControlPlanePublicKey: publicKey,
			})
			if err != nil {
				t.Fatalf("new router: %v", err)
			}
			server := httptest.NewServer(router)
			t.Cleanup(server.Close)
			query := url.Values{TreeBundleParam: tc.bundles}
			if tc.agent != "" {
				query.Set(TreeAgentParam, tc.agent)
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
				server.URL+"/api/project/project-1/pool/pool-1/sandboxes/sandbox-1/tree?"+query.Encode(), nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+sign(projectID, poolID, sandboxID, ScopeSandboxRead))
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if tc.status == http.StatusBadRequest {
				if called {
					t.Fatalf("a malformed bundle reached the export with %+v", got)
				}
				return
			}
			if !called || got.Agent != tc.want.Agent || !slices.Equal(got.Bundles, tc.want.Bundles) {
				t.Fatalf("pins = %+v, want %+v", got, tc.want)
			}
		})
	}
}
