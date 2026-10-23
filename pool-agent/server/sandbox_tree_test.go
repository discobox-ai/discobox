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

func (failingTreeRuntime) ExportTree(context.Context, string, []assetcache.Pin) (io.ReadCloser, error) {
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

// pinRecordingRuntime records the pins an export was asked to read with.
type pinRecordingRuntime struct {
	*sandboxruntime.MemorySandboxRuntime
	pins   *[]assetcache.Pin
	called *bool
	err    error
}

func (r pinRecordingRuntime) ExportTree(_ context.Context, _ string, pins []assetcache.Pin) (io.ReadCloser, error) {
	*r.pins = pins
	*r.called = true
	if r.err != nil {
		return nil, r.err
	}
	return io.NopCloser(strings.NewReader("")), nil
}

// The control plane owns the pins, so the bundles an export reads with arrive
// on the request, each as <kind>:<name>@<digest>; one that does not parse is
// the caller's mistake, refused before anything is read. A sandbox pinned to
// no platform has nothing to be read with, which is a conflict the caller can
// act on — by pinning it — not a failure (ADR 0129 §3, ADR 26-10-16-423 §10).
func TestExportTreeTakesThePinsFromTheRequest(t *testing.T) {
	const projectID, poolID, sandboxID = "project-1", "pool-1", "sandbox-1"
	platform := "sha256:" + strings.Repeat("a", 64)
	harness := "sha256:" + strings.Repeat("b", 64)
	pinned := []string{"platform:discobox-platform@" + platform, "harness:discobox-harness-shell@" + harness}
	want := []assetcache.Pin{
		{Kind: assetcache.KindPlatform, Name: "discobox-platform", Digest: platform},
		{Kind: assetcache.KindHarness, Name: "discobox-harness-shell", Digest: harness},
	}
	for name, tc := range map[string]struct {
		bundles []string
		err     error
		status  int
		want    []assetcache.Pin
	}{
		"pinned to its bundles":   {bundles: pinned, status: http.StatusOK, want: want},
		"pinned to no platform":   {err: sandboxruntime.ErrUndelivered, status: http.StatusUnprocessableEntity, want: []assetcache.Pin{}},
		"no kind":                 {bundles: []string{"discobox-platform@" + platform}, status: http.StatusBadRequest},
		"an unknown kind":         {bundles: []string{"image:discobox-platform@" + platform}, status: http.StatusBadRequest},
		"no name":                 {bundles: []string{"platform:@" + platform}, status: http.StatusBadRequest},
		"no digest":               {bundles: []string{"platform:discobox-platform"}, status: http.StatusBadRequest},
		"a tag, not a digest":     {bundles: []string{"platform:discobox-platform@latest"}, status: http.StatusBadRequest},
		"one malformed among two": {bundles: []string{pinned[0], "harness:discobox-harness-shell@sha256:abc"}, status: http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			var got []assetcache.Pin
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
			if !called || !slices.Equal(got, tc.want) {
				t.Fatalf("pins = %+v, want %+v", got, tc.want)
			}
		})
	}
}
