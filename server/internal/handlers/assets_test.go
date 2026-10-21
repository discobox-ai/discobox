package handlers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	svcapi "github.com/discobox-ai/discobox/server/internal/services"
)

// fakeAssetService serves one manifest and one blob whose body fails after
// half of it, as one found damaged on disk does.
type fakeAssetService struct {
	manifest []byte
	blob     []byte
}

func (f *fakeAssetService) ResolveAssetName(context.Context, string, string) (svcapi.AssetContent, error) {
	return f.manifestContent(), nil
}

func (f *fakeAssetService) AssetManifest(context.Context, string, string, string) (svcapi.AssetContent, error) {
	return f.manifestContent(), nil
}

func (f *fakeAssetService) manifestContent() svcapi.AssetContent {
	return svcapi.AssetContent{
		MediaType: "application/vnd.oci.image.manifest.v1+json", Digest: "sha256:manifest",
		Size: int64(len(f.manifest)), Body: io.NopCloser(bytes.NewReader(f.manifest)),
	}
}

func (f *fakeAssetService) AssetBlob(context.Context, string, string) (svcapi.AssetContent, error) {
	half := f.blob[:len(f.blob)/2]
	return svcapi.AssetContent{
		Digest: "sha256:blob", Size: int64(len(f.blob)),
		Body: io.NopCloser(io.MultiReader(bytes.NewReader(half), failingReader{})),
	}, nil
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("blob does not match its digest")
}

func serveAssets(t *testing.T, svc *fakeAssetService) *httptest.Server {
	t.Helper()
	handler, err := NewServer(svcapi.Services{Assets: svc})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

// A manifest goes out byte for byte, as its own media type, with its digest:
// its digest is over its bytes, so it is never re-encoded.
func TestAnAssetManifestIsServedByteForByte(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,  "config":{"digest":"sha256:x"}}`)
	server := serveAssets(t, &fakeAssetService{manifest: manifest})
	resp, err := get(t, server.URL+"/api/pools/pool-1/assets/discobox-platform/manifests/sha256:"+sixtyFourHex)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, manifest) {
		t.Fatalf("status %d, body %q; want 200 and %q", resp.StatusCode, body, manifest)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/vnd.oci.image.manifest.v1+json" {
		t.Fatalf("Content-Type %q", got)
	}
	if got := resp.Header.Get("Docker-Content-Digest"); got != "sha256:manifest" {
		t.Fatalf("Docker-Content-Digest %q", got)
	}
}

// A blob whose bytes fail on the way out is cut off short of its length, so
// the client never reads a complete response that is not the blob.
func TestAnAssetBlobThatFailsOnTheWayOutIsCutOff(t *testing.T) {
	server := serveAssets(t, &fakeAssetService{blob: bytes.Repeat([]byte("b"), 64<<10)})
	resp, err := get(t, server.URL+"/api/pools/pool-1/assets/blobs/sha256:"+sixtyFourHex)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength != 64<<10 {
		t.Fatalf("status %d, length %d", resp.StatusCode, resp.ContentLength)
	}
	body, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("read %d bytes of a blob that failed, with no error", len(body))
	}
}

func get(t *testing.T, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return http.DefaultClient.Do(req)
}

const sixtyFourHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
