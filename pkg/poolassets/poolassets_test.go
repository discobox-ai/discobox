package poolassets

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/poolauth"
)

// A read of the control plane's asset routes goes to the pool's own asset
// prefix as this pool, signed with its key; anything but a 200 is an error.
func TestFetchReadsThePoolsAssetRoute(t *testing.T) {
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
	client := &Client{BaseURL: server.URL, HTTP: server.Client(), ProjectID: "prj_1", PoolID: "pool_1", Key: private}

	body, err := client.Fetch(context.Background(), "/blobs/sha256:00")
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
	if _, err := client.Fetch(context.Background(), "/blobs/missing"); err == nil {
		t.Fatal("a 404 was read as the asset")
	}
}
