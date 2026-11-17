package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/releasemanifest"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	//nolint:gosec // G204: git, with the test's own fixed arguments.
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestMainVersion(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	commit := func(msg string) { git(t, dir, "commit", "-q", "--allow-empty", "-m", msg) }
	commit("one")
	if _, err := mainVersion(t.Context(), dir, "HEAD"); err == nil || !strings.Contains(err.Error(), "no vMAJOR.MINOR.PATCH tag") {
		t.Fatalf("no tag: err %v", err)
	}
	git(t, dir, "tag", "v0.9.0")
	commit("two")
	git(t, dir, "tag", "v0.10.0")
	// Neither a prerelease nor a sub-project's line is the release to follow.
	commit("three")
	git(t, dir, "tag", "v0.11.0-rc.1")
	git(t, dir, "tag", "vm/v40")
	// A side branch merged in counts once, as its merge.
	git(t, dir, "checkout", "-q", "-b", "side")
	commit("side a")
	commit("side b")
	git(t, dir, "checkout", "-q", "main")
	git(t, dir, "merge", "-q", "--no-ff", "-m", "merge", "side")

	got, err := mainVersion(t.Context(), dir, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	sha := git(t, dir, "rev-parse", "HEAD")
	if want := "v0.10.1-main.4.g" + sha[:7]; got != want {
		t.Fatalf("mainVersion = %q, want %q", got, want)
	}
}

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		v, latest string
		want      bool
	}{
		{"v1.0.1-main.11.gbbbbbbb", "v1.0.1-main.10.gaaaaaaa", true},
		{"v1.0.1-main.10.gaaaaaaa", "v1.0.1-main.11.gbbbbbbb", false},
		{"v1.0.1-main.10.gaaaaaaa", "v1.0.1-main.10.gaaaaaaa", false},
		{"v1.0.2-main.12.gccccccc", "v1.0.1-main.11.gbbbbbbb", true},
		// The repository's first build has nothing to be newer than.
		{"v1.0.1-main.10.gaaaaaaa", "", true},
		{"v1.0.1", "", false},
	} {
		if got := newer(tc.v, tc.latest); got != tc.want {
			t.Errorf("newer(%q, %q) = %v, want %v", tc.v, tc.latest, got, tc.want)
		}
	}
	if err := run(t.Context(), []string{"newer", "v1.0.1-main.10.gaaaaaaa", "v1.0.1-main.11.gbbbbbbb"}); err == nil {
		t.Error("newer succeeded for an older build")
	}
}

func TestMainVersionRefusesAShallowCheckout(t *testing.T) {
	src := t.TempDir()
	git(t, src, "init", "-q", "-b", "main")
	git(t, src, "commit", "-q", "--allow-empty", "-m", "one")
	git(t, src, "tag", "v1.0.0")
	git(t, src, "commit", "-q", "--allow-empty", "-m", "two")
	dir := t.TempDir()
	git(t, dir, "clone", "-q", "--depth", "1", "file://"+src, ".")
	if _, err := mainVersion(t.Context(), dir, "HEAD"); err == nil || !strings.Contains(err.Error(), "shallow") {
		t.Fatalf("a shallow checkout: err %v", err)
	}
}

// fakeGitHub serves the releases and packages prune reads, and records what it
// deletes.
type fakeGitHub struct {
	mu       sync.Mutex
	releases []map[string]any
	versions map[string][]map[string]any
	deleted  []string
	auth     map[string]string
	// installationToken answers the organization's package listing the way
	// GitHub answers a workflow's GITHUB_TOKEN.
	installationToken bool
}

func (f *fakeGitHub) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	list := func(w http.ResponseWriter, r *http.Request, all []map[string]any) {
		page := 1
		_, _ = fmt.Sscanf(r.URL.Query().Get("page"), "%d", &page)
		start := min((page-1)*100, len(all))
		end := min(start+100, len(all))
		if err := json.NewEncoder(w).Encode(all[start:end]); err != nil {
			t.Error(err)
		}
	}
	record := func(r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.deleted = append(f.deleted, r.URL.Path)
		f.auth[r.URL.Path] = r.Header.Get("Authorization")
	}
	mux.HandleFunc("GET /repos/o/main/releases", func(w http.ResponseWriter, r *http.Request) { list(w, r, f.releases) })
	mux.HandleFunc("DELETE /repos/o/main/releases/{id}", func(_ http.ResponseWriter, r *http.Request) { record(r) })
	mux.HandleFunc("DELETE /repos/o/main/git/refs/tags/{tag}", func(_ http.ResponseWriter, r *http.Request) { record(r) })
	mux.HandleFunc("GET /users/o", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"type":"Organization"}`) })
	mux.HandleFunc("GET /orgs/o/packages", func(w http.ResponseWriter, _ *http.Request) {
		if f.installationToken {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"message":"Invalid argument."}`)
			return
		}
		t.Error("prune listed the organization's packages")
	})
	mux.HandleFunc("GET /orgs/o/packages/container/{name}/versions", func(w http.ResponseWriter, r *http.Request) {
		list(w, r, f.versions[r.PathValue("name")])
	})
	mux.HandleFunc("DELETE /orgs/o/packages/container/{name}/versions/{id}", func(_ http.ResponseWriter, r *http.Request) { record(r) })
	return mux
}

func imageVersion(id int, created time.Time, tags ...string) map[string]any {
	return map[string]any{"id": id, "created_at": created, "metadata": map[string]any{"container": map[string]any{"tags": tags}}}
}

func TestPrune(t *testing.T) {
	now := time.Now()
	old, older, recent := now.Add(-20*24*time.Hour), now.Add(-30*24*time.Hour), now.Add(-2*24*time.Hour)
	f := &fakeGitHub{auth: map[string]string{}}
	// Newest first, as GitHub lists them; the stale ones past a page boundary.
	for i := range 150 {
		f.releases = append(f.releases, map[string]any{"id": 1000 + i, "tag_name": fmt.Sprintf("v1.0.1-main.%d.gaaaaaaa", 500-i), "created_at": recent})
	}
	f.releases = append(f.releases,
		map[string]any{"id": 1, "tag_name": "v1.0.1-main.20.gbbbbbbb", "created_at": old},
		map[string]any{"id": 2, "tag_name": "v1.0.1-main.10.gccccccc", "created_at": older},
		// Not a main build: never touched, however old.
		map[string]any{"id": 3, "tag_name": "placeholder", "created_at": older},
	)
	f.versions = map[string][]map[string]any{
		"discobox-agent": {
			imageVersion(10, old, "v1.0.1-main.20.gbbbbbbb"),
			imageVersion(11, recent, "v1.0.1-main.500.gaaaaaaa"),
			imageVersion(12, older, "v1.0.0", "latest"),
			// A release that happens to share an image with a main build.
			imageVersion(13, older, "v1.0.1-main.10.gccccccc", "v1.0.0-rc.1"),
			imageVersion(14, older),
			// An orphan of a build whose release was deleted on an earlier run.
			imageVersion(15, older, "v1.0.1-main.5.gddddddd"),
		},
	}
	server := httptest.NewServer(f.handler(t))
	t.Cleanup(server.Close)

	var log strings.Builder
	p := pruner{
		releases: newClient(server.URL, "contents-token"),
		packages: newClient(server.URL, "packages-token"),
		repo:     "o/main", owner: "o", images: []string{"discobox-agent"},
		cutoff: now.Add(-14 * 24 * time.Hour),
		log:    &log,
	}
	if err := p.prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/repos/o/main/releases/1", "/repos/o/main/git/refs/tags/v1.0.1-main.20.gbbbbbbb",
		"/repos/o/main/releases/2", "/repos/o/main/git/refs/tags/v1.0.1-main.10.gccccccc",
		"/orgs/o/packages/container/discobox-agent/versions/10",
		"/orgs/o/packages/container/discobox-agent/versions/15",
	}
	if !slices.Equal(f.deleted, want) {
		t.Fatalf("deleted %v\nwant %v\n%s", f.deleted, want, log.String())
	}
	if got := f.auth["/repos/o/main/releases/1"]; got != "Bearer contents-token" {
		t.Errorf("a release was deleted with %q", got)
	}
	if got := f.auth["/orgs/o/packages/container/discobox-agent/versions/10"]; got != "Bearer packages-token" {
		t.Errorf("an image was deleted with %q", got)
	}
}

func TestPruneKeepsTheNewestHoweverOld(t *testing.T) {
	old := time.Now().Add(-60 * 24 * time.Hour)
	f := &fakeGitHub{auth: map[string]string{}, versions: map[string][]map[string]any{
		"discobox-agent": {imageVersion(10, old, "v1.0.1-main.2.gbbbbbbb"), imageVersion(11, old, "v1.0.1-main.1.gaaaaaaa")},
	}}
	f.releases = []map[string]any{
		{"id": 2, "tag_name": "v1.0.1-main.2.gbbbbbbb", "created_at": old},
		{"id": 1, "tag_name": "v1.0.1-main.1.gaaaaaaa", "created_at": old.Add(-time.Hour)},
	}
	server := httptest.NewServer(f.handler(t))
	t.Cleanup(server.Close)
	p := pruner{releases: newClient(server.URL, ""), packages: newClient(server.URL, ""), repo: "o/main", owner: "o", images: []string{"discobox-agent"},
		cutoff: time.Now().Add(-14 * 24 * time.Hour), log: io.Discard}
	if err := p.prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"/repos/o/main/releases/1", "/repos/o/main/git/refs/tags/v1.0.1-main.1.gaaaaaaa", "/orgs/o/packages/container/discobox-agent/versions/11"}
	if !slices.Equal(f.deleted, want) {
		t.Fatalf("deleted %v, want %v", f.deleted, want)
	}
}

// A draft is an upload that never finished: it is never the build kept, and
// it has no tag to delete. The channel's build is the newest published one.
func TestPruneTreatsADraftAsAFailedPublish(t *testing.T) {
	old := time.Now().Add(-60 * 24 * time.Hour)
	f := &fakeGitHub{auth: map[string]string{}, versions: map[string][]map[string]any{}}
	f.releases = []map[string]any{
		{"id": 3, "tag_name": "v1.0.1-main.3.gccccccc", "created_at": old, "draft": true},
		{"id": 2, "tag_name": "v1.0.1-main.2.gbbbbbbb", "created_at": old.Add(-time.Hour)},
		{"id": 1, "tag_name": "v1.0.1-main.1.gaaaaaaa", "created_at": old.Add(-2 * time.Hour)},
	}
	server := httptest.NewServer(f.handler(t))
	t.Cleanup(server.Close)
	p := pruner{releases: newClient(server.URL, ""), packages: newClient(server.URL, ""), repo: "o/main", owner: "o", images: []string{"discobox-agent"},
		cutoff: time.Now().Add(-14 * 24 * time.Hour), log: io.Discard}
	if err := p.prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"/repos/o/main/releases/3", "/repos/o/main/releases/1", "/repos/o/main/git/refs/tags/v1.0.1-main.1.gaaaaaaa"}
	if !slices.Equal(f.deleted, want) {
		t.Fatalf("deleted %v, want %v", f.deleted, want)
	}
}

func TestPruneDryRunDeletesNothing(t *testing.T) {
	old := time.Now().Add(-60 * 24 * time.Hour)
	f := &fakeGitHub{auth: map[string]string{}, versions: map[string][]map[string]any{
		"discobox-agent": {imageVersion(11, old, "v1.0.1-main.1.gaaaaaaa")},
	}}
	f.releases = []map[string]any{
		{"id": 2, "tag_name": "v1.0.1-main.2.gbbbbbbb", "created_at": time.Now()},
		{"id": 1, "tag_name": "v1.0.1-main.1.gaaaaaaa", "created_at": old},
	}
	server := httptest.NewServer(f.handler(t))
	t.Cleanup(server.Close)
	var log strings.Builder
	p := pruner{releases: newClient(server.URL, ""), packages: newClient(server.URL, ""), repo: "o/main", owner: "o", images: []string{"discobox-agent"},
		cutoff: time.Now().Add(-14 * 24 * time.Hour), dryRun: true, log: &log}
	if err := p.prune(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("a dry run deleted %v", f.deleted)
	}
	if !strings.Contains(log.String(), "v1.0.1-main.1.gaaaaaaa") {
		t.Errorf("a dry run did not say what it would delete:\n%s", log.String())
	}
}

// The publish job prunes images with its own GITHUB_TOKEN, which GitHub lets
// read and delete a named package's versions but never list the
// organization's packages.
func TestPruneImagesWithAWorkflowToken(t *testing.T) {
	old := time.Now().Add(-60 * 24 * time.Hour)
	f := &fakeGitHub{auth: map[string]string{}, installationToken: true, versions: map[string][]map[string]any{
		"discobox-agent": {imageVersion(10, old, "v1.0.1-main.1.gaaaaaaa")},
	}}
	f.releases = []map[string]any{
		{"id": 2, "tag_name": "v1.0.1-main.2.gbbbbbbb", "created_at": time.Now()},
		{"id": 1, "tag_name": "v1.0.1-main.1.gaaaaaaa", "created_at": old},
	}
	server := httptest.NewServer(f.handler(t))
	t.Cleanup(server.Close)
	p := pruner{releases: newClient(server.URL, ""), packages: newClient(server.URL, ""), repo: "o/main", owner: "o",
		images: []string{"discobox-agent"}, cutoff: time.Now().Add(-14 * 24 * time.Hour), log: io.Discard}
	if err := p.prune(context.Background()); err != nil {
		t.Fatalf("prune with a workflow token: %v", err)
	}
	want := []string{"/repos/o/main/releases/1", "/repos/o/main/git/refs/tags/v1.0.1-main.1.gaaaaaaa", "/orgs/o/packages/container/discobox-agent/versions/10"}
	if !slices.Equal(f.deleted, want) {
		t.Fatalf("deleted %v, want %v", f.deleted, want)
	}
}

// Only a discobox-* package is ever pruned, whatever the manifest names, and
// one that is not fails the run before any build is deleted.
func TestPruneRefusesAnImageNotDiscobox(t *testing.T) {
	old := time.Now().Add(-60 * 24 * time.Hour)
	f := &fakeGitHub{auth: map[string]string{}, versions: map[string][]map[string]any{}}
	f.releases = []map[string]any{
		{"id": 2, "tag_name": "v1.0.1-main.2.gbbbbbbb", "created_at": time.Now()},
		{"id": 1, "tag_name": "v1.0.1-main.1.gaaaaaaa", "created_at": old},
	}
	server := httptest.NewServer(f.handler(t))
	t.Cleanup(server.Close)
	p := pruner{releases: newClient(server.URL, ""), packages: newClient(server.URL, ""), repo: "o/main", owner: "o",
		images: []string{"discobox-agent", "other-thing"}, cutoff: time.Now().Add(-14 * 24 * time.Hour), log: io.Discard}
	if err := p.prune(context.Background()); err == nil || !strings.Contains(err.Error(), "other-thing") {
		t.Fatalf("prune of other-thing: err %v", err)
	}
	if len(f.deleted) != 0 {
		t.Fatalf("deleted %v before refusing other-thing", f.deleted)
	}
}

// The images pruned are those the build pushed: what its manifest pins at its
// own version under the owner's namespace, not the VM guest and kernel it pins
// by digest from their own line, nor an image under another owner.
func TestReleaseImages(t *testing.T) {
	v := "v1.0.1-main.3.gccccccc"
	m := releasemanifest.Manifest{
		Format:  1,
		Version: v,
		Images: releasemanifest.Images{
			SandboxBase: "ghcr.io/o/discobox-sandbox-base:" + v,
			VM:          "ghcr.io/o/discobox-vm@sha256:" + strings.Repeat("a", 64),
			Kernel:      "ghcr.io/o/discobox-vm-kernel:v40",
		},
		Bundles: releasemanifest.Bundles{
			Platform:  "ghcr.io/o/discobox-platform:" + v,
			Agent:     "ghcr.io/o/discobox-agent:" + v,
			PoolAgent: "ghcr.io/o/discobox-pool-agent-bundle:" + v,
			Harnesses: map[string]string{
				"shell":       "ghcr.io/o/discobox-harness-shell:" + v,
				"claude-code": "ghcr.io/o/discobox-harness-claude-code:" + v,
				"elsewhere":   "ghcr.io/other/discobox-harness-elsewhere:" + v,
			},
		},
	}
	got, err := releaseImages(m, "o")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"discobox-agent", "discobox-harness-claude-code", "discobox-harness-shell", "discobox-platform", "discobox-pool-agent-bundle", "discobox-sandbox-base"}
	if !slices.Equal(got, want) {
		t.Fatalf("releaseImages = %v\nwant %v", got, want)
	}
	if _, err := releaseImages(m, "nobody"); err == nil {
		t.Error("a manifest naming no image under the owner gave no error")
	}
}
