package gitcache

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/discobox-ai/discobox/pkg/sourcecache"
	"github.com/discobox-ai/discobox/pool-agent/githttp"
)

func TestPullPinsOnlyTheRemoteAndLendsForOneRequest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pool Git credential helper requires POSIX shell")
	}
	w := newWork(t)
	commit := w.commit("public")
	remote := filepath.Join(t.TempDir(), "remote.git")
	w.git("clone", "--bare", w.dir, remote)
	secret := w.commit("private")
	w.git("--git-dir="+remote, "update-ref", "refs/heads/main", commit)
	var requireCredential atomic.Bool
	var authStatus atomic.Int32
	authStatus.Store(http.StatusUnauthorized)
	server := httptest.NewTLSServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if r.UserAgent() == "discobox-sandbox-agent (port probe)" {
			out.WriteHeader(200)
			return
		}
		if requireCredential.Load() {
			u, p, ok := r.BasicAuth()
			if !ok || u != "reader" || p != "password" {
				out.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				out.WriteHeader(int(authStatus.Load()))
				return
			}
		}
		_, suffix, _ := strings.Cut(r.URL.Path, ".git")
		githttp.ServeBackend(out, r, githttp.Repository{Path: remote, UID: -1, GID: -1}, suffix)
	}))
	defer server.Close()
	url := server.URL + "/repo.git"
	key := sourcecache.RemoteKey(url)
	store := New(t.TempDir())
	path, release, err := store.Use(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := git(t.Context(), path, nil, nil, "config", "http.sslCAInfo", cert); err != nil {
		t.Fatal(err)
	}
	// An unrelated namespace already holds the private commit. An anonymous pull
	// may not pin it unless the remote actually serves it.
	w.git("push", path, secret+":refs/namespaces/sbx-other-primary/refs/heads/main")
	release()
	result, err := store.Pull(t.Context(), sourcecache.PullRequest{Key: key, Remote: url, SandboxID: "sbx-one", Slug: "primary", Commit: commit, Branch: "main", Snapshot: false, Credential: nil}, nil)
	if err != nil || result.Status != sourcecache.Delivered {
		t.Fatalf("public pull: %+v, %v", result, err)
	}
	if got := resolve(t.Context(), path, namespaceRef(Namespace("sbx-one", "primary"), "refs/heads/main")); got != commit {
		t.Fatalf("branch=%s", got)
	}
	if _, err := git(t.Context(), path, nil, nil, "update-ref", "-d", RemoteRefPrefix+"heads/main"); err != nil {
		t.Fatal(err)
	}
	result, err = store.Pull(t.Context(), sourcecache.PullRequest{Key: key, Remote: url, SandboxID: "sbx-missing", Slug: "primary", Commit: secret, Branch: "main", Snapshot: false, Credential: nil}, nil)
	if err != nil || result.Status != sourcecache.MissingCommit {
		t.Fatalf("private cached object leaked to anonymous source: %+v, %v", result, err)
	}
	if got := resolve(t.Context(), path, RemoteRefPrefix+"heads/main"); got != commit {
		t.Fatal("failed pin discarded fetched remote history")
	}
	result, err = store.Pull(t.Context(), sourcecache.PullRequest{Key: key, Remote: url, SandboxID: "sbx-dirty", Slug: "primary", Commit: commit, Branch: "main", Snapshot: true, Credential: nil}, nil)
	if err != nil || result.Status != sourcecache.MissingCommit {
		t.Fatalf("dirty workspace delivered without client snapshot: %+v, %v", result, err)
	}
	if got := resolve(t.Context(), path, namespaceRef(Namespace("sbx-dirty", "primary"), "refs/heads/main")); got != "" {
		t.Fatal("dirty namespace was pinned")
	}
	dirtyPath := sourcecache.PullRequest{Key: key, Remote: url, SandboxID: "sbx-dirty-path", Slug: "primary", Commit: commit, Branch: "main", WorkingPath: "snapshot-only-dir", Snapshot: true}
	if result, err := store.Pull(t.Context(), dirtyPath, nil); err != nil || result.Status != sourcecache.MissingCommit {
		t.Fatalf("snapshot path incorrectly checked against base: %+v %v", result, err)
	}
	invalid := sourcecache.PullRequest{Key: key, Remote: url, SandboxID: "sbx-bad-path", Slug: "primary", Commit: commit, Branch: "main", WorkingPath: "missing-dir"}
	if _, err := store.Pull(t.Context(), invalid, nil); err == nil {
		t.Fatal("pool delivered a source with no requested working path")
	}
	// Stop the remote from serving arbitrary object IDs.
	w.git("--git-dir="+remote, "config", "uploadpack.hideRefs", "refs/namespaces")
	requireCredential.Store(true)
	authStatus.Store(http.StatusForbidden)
	result, err = store.Pull(t.Context(), sourcecache.PullRequest{Key: key, Remote: url, SandboxID: "sbx-forbidden", Slug: "primary", Commit: commit, Branch: "main"}, nil)
	if err != nil || result.Status != sourcecache.NeedsCredential {
		t.Fatalf("403 challenge: %+v, %v", result, err)
	}
	result, err = store.Pull(t.Context(), sourcecache.PullRequest{Key: key, Remote: url, SandboxID: "sbx-forbidden", Slug: "primary", Commit: commit, Branch: "main", Credential: &sourcecache.Credential{Username: "reader", Password: "password"}}, nil)
	if err != nil || result.Status != sourcecache.Delivered {
		t.Fatalf("lent credential after403: %+v, %v", result, err)
	}
	authStatus.Store(http.StatusUnauthorized)
	result, err = store.Pull(t.Context(), sourcecache.PullRequest{Key: key, Remote: url, SandboxID: "sbx-two", Slug: "primary", Commit: commit, Branch: "main", Snapshot: false, Credential: nil}, nil)
	if err != nil || result.Status != sourcecache.NeedsCredential {
		t.Fatalf("private pull: %+v, %v", result, err)
	}
	result, err = store.Pull(t.Context(), sourcecache.PullRequest{Key: key, Remote: url, SandboxID: "sbx-two", Slug: "primary", Commit: commit, Branch: "main", Snapshot: false, Credential: &sourcecache.Credential{Username: "reader", Password: "password"}}, nil)
	if err != nil || result.Status != sourcecache.Delivered {
		t.Fatalf("lent pull: %+v, %v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(path, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "password") || strings.Contains(string(data), "reader") {
		t.Fatal("credential persisted in git config")
	}
	result, err = store.Pull(t.Context(), sourcecache.PullRequest{Key: key, Remote: url, SandboxID: "sbx-three", Slug: "primary", Commit: commit, Branch: "main", Snapshot: false, Credential: nil}, nil)
	if err != nil || result.Status != sourcecache.NeedsCredential {
		t.Fatalf("later anonymous pull retained credential: %+v, %v", result, err)
	}
}

func TestAuthChallengeTraceIsBoundedAndRequires403(t *testing.T) {
	for _, test := range []struct {
		trace     string
		challenge bool
	}{
		{"<= Recv header: HTTP/2 403\n<= Recv header: WWW-Authenticate: Basic realm=git\n", true},
		{"<= Recv header: HTTP/2 403\n", false},
		{"<= Recv header: HTTP/2 500\n<= Recv header: WWW-Authenticate: Basic realm=git\n", false},
		{strings.Repeat("x", 1<<20), false},
	} {
		if got := authChallenge(strings.NewReader(test.trace)); got != test.challenge {
			t.Fatalf("challenge=%v want%v", got, test.challenge)
		}
	}
}
