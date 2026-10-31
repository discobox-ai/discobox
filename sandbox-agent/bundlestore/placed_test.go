//go:build !windows

package bundlestore

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
)

// A pool's maker places its bundles' blobs by digest, and the store applies
// them from there as it would from the asset host, fetching nothing.
func TestApplyAppliesWhatAPoolsMakerPlaced(t *testing.T) {
	a := newAssets()
	pinned, _ := a.bundle(t, "discobox-pool", true, layer(t, coreLayer), layer(t, nodeLayer))
	s := newStore(t, a)
	s.Fetcher = PlacedFetcher{Root: s.Root}
	if err := os.MkdirAll(s.path(agentlayer.PlacedDir), 0o700); err != nil {
		t.Fatal(err)
	}
	for p, data := range a.content {
		// The attestation the index lists is never placed, nor read.
		if err := os.WriteFile(s.path(agentlayer.PlacedBlob(path.Base(p))), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Apply(context.Background(), agentlayer.Pins{{Name: "discobox-pool", Digest: pinned}}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, s, tree(t, s, 0)+"/features/node/provision.yaml"); got != "node" {
		t.Fatalf("node = %q", got)
	}
	if len(a.asked) != 0 {
		t.Fatalf("the store fetched %v", a.asked)
	}
}

func TestPlacedFetcherRefusesWhatIsNoDigestOrWasNotPlaced(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(agentlayer.PlacedDir)), 0o700); err != nil {
		t.Fatal(err)
	}
	f := PlacedFetcher{Root: root}
	missing := digestOf([]byte("missing"))
	for _, p := range []string{
		"/blobs/" + missing,
		"/discobox-pool/manifests/" + missing,
		"/blobs/sha256:../../etc",
		"/elsewhere/" + missing,
	} {
		if body, err := f.Fetch(context.Background(), p); err == nil {
			body.Close()
			t.Fatalf("Fetch(%q) answered", p)
		}
	}
}
