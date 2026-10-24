package poolagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/poolauth"
)

// A read of the control plane's asset routes goes to the pool's own asset
// prefix as this pool, signed with its key; anything but a 200 is an error.
func TestFetchAssetReadsThePoolsAssetRoute(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var path, assertion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, assertion = r.URL.Path, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if strings.HasSuffix(r.URL.Path, "missing") {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "blob")
	}))
	defer server.Close()
	client := NewHTTPClient(server.URL, WithHTTPClient(server.Client()))
	request := AssetRequest{ProjectID: "prj_1", PoolID: "pool_1", PrivateKey: private, Path: "/blobs/sha256:00"}

	body, err := client.FetchAsset(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	_ = body.Close()
	if string(data) != "blob" || path != "/api/pools/pool_1/assets/blobs/sha256:00" {
		t.Fatalf("read %q from %s", data, path)
	}
	encoded, err := poolauth.EncodePublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := poolauth.VerifyToken(encoded, assertion)
	if err != nil || claims.PoolID != "pool_1" || claims.ProjectID != "prj_1" {
		t.Fatalf("assertion = %+v, %v; want pool_1 of prj_1", claims, err)
	}

	request.Path = "/blobs/missing"
	if _, err := client.FetchAsset(context.Background(), request); err == nil {
		t.Fatal("a 404 was read as the asset")
	}
}

// The asset socket replaces one an earlier agent left, and only root may
// open it.
func TestListenAssetsReplacesAStaleSocket(t *testing.T) {
	// A short directory: a socket path over ~100 bytes does not bind, and a
	// test's own temporary directory can be longer.
	dir, err := os.MkdirTemp("", "as")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "proxy", "assets.sock")
	first, err := listenAssets(socket)
	if err != nil {
		t.Fatal(err)
	}
	// Left behind, as an agent that died does.
	if l, ok := first.(interface{ SetUnlinkOnClose(bool) }); ok {
		l.SetUnlinkOnClose(false)
	}
	_ = first.Close()
	second, err := listenAssets(socket)
	if err != nil {
		t.Fatalf("listen over a stale socket: %v", err)
	}
	defer second.Close()
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatal(err)
	}
	// Windows keeps no mode bits; the pool agent runs on Linux.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v, want 0600", info.Mode().Perm())
	}
}
