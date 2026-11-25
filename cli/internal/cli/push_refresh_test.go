package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// refreshServer answers with one running discobox whose primary source is a
// delivered remote, and answers a pull of that source with answer. It
// records every request the command makes.
func refreshServer(t *testing.T, answer map[string]any, requests *[]string) *httptest.Server {
	return refreshServerDelivered(t, "cache", answer, requests)
}

// refreshServerDelivered is refreshServer with the source recorded as delivered
// by delivery.
func refreshServerDelivered(t *testing.T, delivery string, answer map[string]any, requests *[]string) *httptest.Server {
	t.Helper()
	sandbox := map[string]any{
		"id":              pushTestSandboxID,
		"projectId":       "project-1",
		"createdByUserId": "user-1",
		"displayName":     "running",
		"config": map[string]any{"name": "running", "image": "", "source": map[string]any{
			"kind":     "git",
			"slug":     "primary",
			"url":      "https://example.com/acme/widget.git",
			"delivery": delivery,
			"checkout": map[string]any{"commit": "0123456789abcdef0123456789abcdef01234567", "refName": "main", "refType": "branch"},
		}},
		"runtime": map[string]any{
			"state":              "ready",
			"displayState":       "running",
			"desiredState":       "present",
			"sourceDeliveredAt":  "2026-06-17T00:00:01Z",
			"generation":         1,
			"observedGeneration": 1,
		},
		"createdAt": "2026-06-17T00:00:00Z",
		"updatedAt": "2026-06-17T00:00:01Z",
	}
	server := httptest.NewServer(ignoringPortProbe(func(w http.ResponseWriter, r *http.Request) {
		*requests = append(*requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sandboxes/"+pushTestSandboxID):
			_ = json.NewEncoder(w).Encode(sandbox)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/sandboxes/"+pushTestSandboxID+"/sources/primary/pull"):
			_ = json.NewEncoder(w).Encode(answer)
		default:
			http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func runPushCapturing(t *testing.T, serverURL string, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(append([]string{"--server", serverURL, "--project", "project-1", "push", pushTestSandboxID}, args...))
	err := cmd.Execute()
	return out.String(), err
}

// A remote source is refreshed, and push says what moved and how the
// discobox takes it in.
func TestPushRefreshesARemoteSource(t *testing.T) {
	t.Setenv("DISCOBOX", "")
	var requests []string
	server := refreshServer(t, map[string]any{"status": "refreshed", "updates": []map[string]any{
		{"ref": "refs/remotes/origin/staging", "old": "55b27230aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "new": "ca783c63bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	}}, &requests)
	out, err := runPushCapturing(t, server.URL, "--branch", "staging")
	if err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	for _, want := range []string{"refreshed https://example.com/acme/widget.git, by pool", "origin/staging", "55b2723", "ca783c6", "git fetch discobox '+refs/remotes/origin/*:refs/remotes/origin/*'"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// A private remote under the ask policy, with nobody to ask, is skipped
// when every source is pushed, and fails the push that names it.
func TestPushSkipsAPrivateRemoteItCannotAskAbout(t *testing.T) {
	t.Setenv("DISCOBOX", "")
	t.Setenv("DISCOBOX_CLIENT_CONFIG_FILE", "")
	var requests []string
	server := refreshServer(t, map[string]any{"status": "needs_credential", "protocol": "https", "host": "example.com", "path": "acme/widget.git"}, &requests)
	out, err := runPushCapturing(t, server.URL, "--git-credentials=ask")
	if err != nil || !strings.Contains(out, "skipped") || !strings.Contains(out, "--git-credentials=local") {
		t.Fatalf("push of every source = %v:\n%s", err, out)
	}
	if _, err := runPushCapturing(t, server.URL, "--git-credentials=ask", "--source", "primary"); err == nil {
		t.Fatal("push --source of a remote it could not refresh succeeded")
	}
}

// --force means nothing to a refresh, so it is refused when a remote source
// is all that was asked for, before the pool is asked anything.
func TestPushRefusesForceForARemoteSourceAlone(t *testing.T) {
	var requests []string
	server := refreshServer(t, map[string]any{"status": "refreshed"}, &requests)
	if _, err := runPushCapturing(t, server.URL, "--force", "--source", "primary"); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("push --force --source primary = %v", err)
	}
	for _, request := range requests {
		if strings.HasSuffix(request, "/pull") {
			t.Fatal("the pool was asked to refresh")
		}
	}
}

// A remote source from before pools cached Git has no pool copy to refresh:
// the discobox fetches it itself, and push skips it as it always has, without
// asking the pool or failing.
func TestPushSkipsARemoteSourceFromBeforePoolsCachedGit(t *testing.T) {
	var requests []string
	server := refreshServerDelivered(t, "clone", map[string]any{"status": "refreshed"}, &requests)
	out, err := runPushCapturing(t, server.URL)
	if err != nil || !strings.Contains(out, "skipped") {
		t.Fatalf("push = %v:\n%s", err, out)
	}
	for _, request := range requests {
		if strings.HasSuffix(request, "/pull") {
			t.Fatal("the pool was asked to refresh a source it holds no copy of")
		}
	}
}
