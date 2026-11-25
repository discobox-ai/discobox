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

// A refresh of a delivered source moves only its namespace's record of the
// remote (ADR 26-11-24-142 §1): remote-tracking branches and tags, pruned
// when every branch is refreshed, and the cache's shared record with them.
// The box's own branch and its delivery stay as they were.
func TestRefreshMovesOnlyTheNamespacesRecordOfTheRemote(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pool Git credential helper requires POSIX shell")
	}
	w := newWork(t)
	base := w.commit("base")
	remote := filepath.Join(t.TempDir(), "remote.git")
	w.git("init", "--quiet", "--bare", remote)
	w.git("push", remote, base+":refs/heads/main", base+":refs/heads/staging", base+":refs/heads/gone")
	var requireCredential atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(out http.ResponseWriter, r *http.Request) {
		if requireCredential.Load() {
			u, p, ok := r.BasicAuth()
			if !ok || u != "reader" || p != "password" {
				out.Header().Set("WWW-Authenticate", `Basic realm="git"`)
				out.WriteHeader(http.StatusUnauthorized)
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
	if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := git(t.Context(), path, nil, nil, "config", "http.sslCAInfo", cert); err != nil {
		t.Fatal(err)
	}
	release()
	request := sourcecache.RefreshRequest{Key: key, Remote: url, SandboxID: "sbx-one", Slug: "primary"}
	if _, err := store.Refresh(t.Context(), request, nil); err == nil {
		t.Fatal("a source never delivered was refreshed")
	}
	if result, err := store.Pull(t.Context(), sourcecache.PullRequest{Key: key, Remote: url, SandboxID: "sbx-one", Slug: "primary", Commit: base, Branch: "main"}, nil); err != nil || result.Status != sourcecache.Delivered {
		t.Fatalf("deliver: %+v, %v", result, err)
	}
	namespace := Namespace("sbx-one", "primary")
	// The box's branch has moved on since, by a push of its own.
	boxCommit := w.commit("box")
	w.git("push", path, boxCommit+":"+namespaceRef(namespace, "refs/heads/main"))

	// The remote moves: staging gains a commit, a tag appears, gone is deleted.
	staging := w.commit("staging")
	w.git("push", remote, staging+":refs/heads/staging", staging+":refs/tags/v1", ":refs/heads/gone")

	// One branch: only it and the tags; nothing is pruned.
	request.Branch = "staging"
	result, err := store.Refresh(t.Context(), request, nil)
	if err != nil || result.Status != sourcecache.Refreshed {
		t.Fatalf("refresh staging: %+v, %v", result, err)
	}
	if got := updates(result); got != "refs/remotes/origin/staging "+base+" "+staging+"|refs/tags/v1  "+staging {
		t.Fatalf("refresh staging moved %s", got)
	}
	if got := resolve(t.Context(), path, namespaceRef(namespace, "refs/remotes/origin/gone")); got != base {
		t.Fatal("a refresh of one branch pruned another")
	}
	if got := resolve(t.Context(), path, RemoteRefPrefix+"heads/staging"); got != staging {
		t.Fatal("the cache's shared record of the remote did not follow the refresh")
	}

	request.Branch = "absent"
	if result, err := store.Refresh(t.Context(), request, nil); err != nil || result.Status != sourcecache.MissingBranch {
		t.Fatalf("refresh of a branch the remote does not have: %+v, %v", result, err)
	}

	// Every branch: gone is pruned, and a second refresh moves nothing.
	request.Branch = ""
	result, err = store.Refresh(t.Context(), request, nil)
	if err != nil || result.Status != sourcecache.Refreshed {
		t.Fatalf("refresh: %+v, %v", result, err)
	}
	if got := updates(result); got != "refs/remotes/origin/gone "+base+" " {
		t.Fatalf("refresh moved %s", got)
	}
	if result, err = store.Refresh(t.Context(), request, nil); err != nil || len(result.Updates) != 0 {
		t.Fatalf("a refresh of a current namespace moved %+v, %v", result, err)
	}
	if got := resolve(t.Context(), path, namespaceRef(namespace, "refs/heads/main")); got != boxCommit {
		t.Fatalf("a refresh wrote the box's own branch: %s", got)
	}

	// A private remote: refused for a credential, then refreshed with one,
	// which nothing keeps.
	requireCredential.Store(true)
	main := w.commit("main")
	w.git("push", remote, main+":refs/heads/main")
	if result, err = store.Refresh(t.Context(), request, nil); err != nil || result.Status != sourcecache.NeedsCredential || result.Host == "" {
		t.Fatalf("anonymous refresh of a private remote: %+v, %v", result, err)
	}
	request.Credential = &sourcecache.Credential{Username: "reader", Password: "password"}
	result, err = store.Refresh(t.Context(), request, nil)
	if err != nil || result.Status != sourcecache.Refreshed || updates(result) != "refs/remotes/origin/main "+base+" "+main {
		t.Fatalf("lent refresh: %+v, %v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(path, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "password") {
		t.Fatal("credential persisted in git config")
	}
}

func updates(result sourcecache.Refresh) string {
	var out []string
	for _, update := range result.Updates {
		out = append(out, update.Ref+" "+update.Old+" "+update.New)
	}
	return strings.Join(out, "|")
}

// A client's own refresh (ADR 26-11-24-142 §3, local) is a member's push
// marked with sourcecache.RefreshPushOption: it may drop the remote's branches the remote
// no longer has, and write nothing but the remote's branches and tags. No
// other push deletes.
func TestARefreshPushWritesOnlyTheRemotesRefs(t *testing.T) {
	w := newWork(t)
	base := w.commit("base")
	store := New(t.TempDir())
	path := use(t, store, sourcecache.RemoteKey("https://example.com/acme/widget"))
	if out, err := w.push(store, path, "sbx-one", "primary", "", base+":refs/heads/main", base+":refs/remotes/origin/main", base+":refs/remotes/origin/gone"); err != nil {
		t.Fatalf("deliver: %v\n%s", err, out)
	}
	namespace := Namespace("sbx-one", "primary")
	if out, err := w.push(store, path, "sbx-one", "primary", "", ":refs/remotes/origin/gone"); err == nil {
		t.Fatalf("a push that is not a refresh deleted a ref:\n%s", out)
	}
	if out, err := pushRefresh(w, store, path, "sbx-one", "primary", base+":refs/heads/other"); err == nil {
		t.Fatalf("a refresh wrote a branch of the box's:\n%s", out)
	}
	next := w.commit("next")
	w.git("update-ref", "refs/remotes/origin/main", next)
	if out, err := pushRefresh(w, store, path, "sbx-one", "primary", "--prune", "+refs/remotes/origin/*:refs/remotes/origin/*"); err != nil {
		t.Fatalf("refresh: %v\n%s", err, out)
	}
	if got := resolve(t.Context(), path, namespaceRef(namespace, "refs/remotes/origin/main")); got != next {
		t.Fatalf("origin/main = %s, want %s", got, next)
	}
	if got := resolve(t.Context(), path, namespaceRef(namespace, "refs/remotes/origin/gone")); got != "" {
		t.Fatal("a refresh did not drop a branch the remote no longer has")
	}
	if got := resolve(t.Context(), path, namespaceRef(namespace, "refs/heads/main")); got != base {
		t.Fatal("a refresh moved the box's own branch")
	}
}

// pushRefresh pushes args as a project member's refresh of a namespace.
func pushRefresh(w *work, s *Store, path, sandboxID, slug string, args ...string) (string, error) {
	w.t.Helper()
	config, env, err := s.MemberReceiveConfig()
	if err != nil {
		w.t.Fatal(err)
	}
	receive := "git " + strings.Join(append(ReceiveConfig(), config...), " ") + " receive-pack"
	env = append(env, "GIT_NAMESPACE="+Namespace(sandboxID, slug))
	return w.try(env, append([]string{"push", "--atomic", "-o", sourcecache.RefreshPushOption, "--receive-pack=" + receive, path}, args...)...)
}
