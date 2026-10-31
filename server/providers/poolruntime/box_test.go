package poolruntime

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	sandbox "github.com/discobox-ai/discobox/server/internal/sandbox"
)

// What apply applies is placed with the bootstrap: each blob of the pool's
// bundles in the box's content store, by digest, so the box fetches nothing
// (ADR 26-10-30-875 §5).
func TestApplyTreePlacesEveryBlobOfThePoolsBundles(t *testing.T) {
	manifest, layer := "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64)
	blobs := map[string]string{manifest: "manifest", layer: "layer"}
	box := sandbox.PoolBox{
		Blobs: []string{manifest, layer},
		OpenBlob: func(_ context.Context, digest string) (io.ReadCloser, int64, error) {
			return io.NopCloser(strings.NewReader(blobs[digest])), int64(len(blobs[digest])), nil
		},
	}
	tree := applyTree(context.Background(), []BoxFile{{Path: "/etc/discobox/pool-agent/bootstrap.json", Data: []byte("{}")}}, box)
	defer tree.Close()
	got := map[string]string{}
	tr := tar.NewReader(tree)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		got["/"+strings.TrimSuffix(header.Name, "/")] = string(data)
	}
	for path, want := range map[string]string{
		"/etc/discobox/pool-agent/bootstrap.json": "{}",
		agentlayer.PlacedDir:                      "",
		agentlayer.PlacedBlob(manifest):           "manifest",
		agentlayer.PlacedBlob(layer):              "layer",
	} {
		if data, ok := got[path]; !ok || data != want {
			t.Fatalf("tree = %q; want %s holding %q", got, path, want)
		}
	}
}

func TestApplyTreeFailsOnABlobItCannotRead(t *testing.T) {
	box := sandbox.PoolBox{
		Blobs: []string{"sha256:" + strings.Repeat("1", 64)},
		OpenBlob: func(context.Context, string) (io.ReadCloser, int64, error) {
			return nil, 0, errors.New("the registry is down")
		},
	}
	tree := applyTree(context.Background(), nil, box)
	defer tree.Close()
	if _, err := io.Copy(io.Discard, tree); err == nil || !strings.Contains(err.Error(), "the registry is down") {
		t.Fatalf("read the tree: %v; want the blob's error", err)
	}
}
