//go:build darwin || linux

package sandboxcreate

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxgit"
)

func TestNoninteractiveLocalDeliveryCannotAskWhenPoolCacheRejectsToken(t *testing.T) {
	pool := newFakePool(t)
	useClientGitCache(t)
	work := t.TempDir()
	deliverGit(t, work, "init", "--quiet", "--initial-branch=main")
	commit := deliverCommit(t, work, "a")
	remote, _ := url.Parse("file://" + filepath.ToSlash(work))
	upstream, _ := url.Parse(pool.server.URL)
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	var challenges atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/git-caches/") {
			challenges.Add(1)
			w.Header().Set("WWW-Authenticate", `Basic realm="pool"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer server.Close()
	marker := filepath.Join(t.TempDir(), "asked")
	helper := filepath.Join(t.TempDir(), "askpass")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\necho asked > '"+marker+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_ASKPASS", helper)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	source := apimodel.GitSource{URL: apiclientgen.NewOptURI(*remote), Checkout: branchCheckout(commit, "main")}
	err := DeliverSource(sandboxgit.WithNoninteractive(t.Context()), pool, "p", cacheSandbox("sbx-one", source), nil, server.URL, "token", nil, DeliveryOptions{Credentials: GitCredentialsLocal})
	if err != nil {
		t.Fatal(err)
	}
	if challenges.Load() == 0 {
		t.Fatal("pool cache auth failure was not exercised")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("pool cache fetch ran askpass: %v", err)
	}
}
