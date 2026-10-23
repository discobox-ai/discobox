package devbundles

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/discobox-ai/discobox/pkg/devimage"
	"github.com/discobox-ai/discobox/pkg/imagecache"
)

func blob(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// nestedLayout is what the containerd image store saves a bundle as: an
// index.json listing an index, which lists the image and an attestation.
func nestedLayout(t *testing.T) ([]byte, string) {
	t.Helper()
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	attestation := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","attestation":true}`)
	nested := mustJSON(t, map[string]any{"schemaVersion": 2, "mediaType": imagecache.MediaTypeOCIIndex, "manifests": []map[string]any{
		{"mediaType": imagecache.MediaTypeOCIManifest, "digest": blob(manifest), "size": len(manifest), "platform": map[string]string{"os": "linux", "architecture": "amd64"}},
		{"mediaType": imagecache.MediaTypeOCIManifest, "digest": blob(attestation), "size": len(attestation), "platform": map[string]string{"os": "unknown", "architecture": "unknown"}},
	}})
	top := mustJSON(t, map[string]any{"schemaVersion": 2, "manifests": []map[string]any{
		{"mediaType": imagecache.MediaTypeOCIIndex, "digest": blob(nested), "size": len(nested)},
	}})
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	for name, data := range map[string][]byte{
		"blobs/sha256/" + blob(manifest)[7:]:    manifest,
		"blobs/sha256/" + blob(attestation)[7:]: attestation,
		"blobs/sha256/" + blob(nested)[7:]:      nested,
		"index.json":                            top,
	} {
		if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), blob(manifest)
}

// A bundle a pool's daemon builds is saved listing its one image manifest,
// whatever index the daemon wrapped it in, and a later server finds it saved.
func TestABuiltBundleIsSavedAsItsImageManifest(t *testing.T) {
	dir := t.TempDir()
	const ref = "discobox-harness-codex:dev-0123"
	bundles := []devimage.Bundle{{Reference: ref, Build: &devimage.BuildSpec{Context: "/src", Dockerfile: "harness/codex-cli/Dockerfile", Target: "bundle"}}}
	set := New(bundles, dir)
	if _, ok := set.Unbuilt(ref); !ok || len(set.Builds()) != 1 {
		t.Fatal("a build-mode bundle is not to be built")
	}
	if _, ok := set.Layout(ref); ok {
		t.Fatal("an unbuilt bundle has a layout")
	}
	built := make(chan struct{}, 1)
	set.OnBuilt(func() { built <- struct{}{} })
	layout, want := nestedLayout(t)
	if err := set.Save(ref, bytes.NewReader(layout)); err != nil {
		t.Fatal(err)
	}
	saved, ok := set.Layout(ref)
	if !ok || saved.Digest != want || saved.Check() != nil {
		t.Fatalf("Layout = %+v, %v, want the image manifest %s", saved, ok, want)
	}
	select {
	case <-built:
	case <-time.After(5 * time.Second):
		t.Fatal("no one was told the bundle was built")
	}
	if len(set.Builds()) != 0 {
		t.Fatal("a saved bundle is still to be built")
	}
	if again, ok := New(bundles, dir).Layout(ref); !ok || again.Digest != want {
		t.Fatalf("a later server's Layout = %+v, %v, want the saved one", again, ok)
	}
}

// Two pools coming up at once build the same bundle, and the one that saves
// it second finds it saved: no error, and no second notice.
func TestASecondSaveOfABuiltBundleIsNoError(t *testing.T) {
	dir := t.TempDir()
	const ref = "discobox-platform:dev-0123"
	set := New([]devimage.Bundle{{Reference: ref, Build: &devimage.BuildSpec{Context: "/src", Dockerfile: "sandbox-agent/Dockerfile", Target: "platform"}}}, dir)
	layout, _ := nestedLayout(t)
	if err := set.Save(ref, bytes.NewReader(layout)); err != nil {
		t.Fatal(err)
	}
	if err := set.Save(ref, bytes.NewReader(layout)); err != nil {
		t.Fatalf("a second save: %v", err)
	}
}

// A layout saved for a build an edit has since superseded is removed when a
// set no longer naming it is made.
func TestASupersededSaveIsRemoved(t *testing.T) {
	dir := t.TempDir()
	build := &devimage.BuildSpec{Context: "/src", Dockerfile: "sandbox-agent/Dockerfile", Target: "platform"}
	old := New([]devimage.Bundle{{Reference: "discobox-platform:dev-old", Build: build}}, dir)
	layout, _ := nestedLayout(t)
	if err := old.Save("discobox-platform:dev-old", bytes.NewReader(layout)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".bundle-cut-short.tar"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	current := New([]devimage.Bundle{{Reference: "discobox-platform:dev-new", Build: build}}, dir)
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("dir holds %d files, want the superseded layout and the cut-short save removed", len(left))
	}
	if _, ok := current.Unbuilt("discobox-platform:dev-new"); !ok {
		t.Fatal("the current bundle is not to be built")
	}
}

func TestOnlyABuildModeBundleIsSaved(t *testing.T) {
	set := New([]devimage.Bundle{{Reference: "x:dev-1", Digest: "sha256:" + string(bytes.Repeat([]byte("a"), 64)), Layout: "/x.tar"}}, t.TempDir())
	if err := set.Save("x:dev-1", bytes.NewReader(nil)); err == nil {
		t.Fatal("a watcher-exported bundle was saved over")
	}
	var none *Set
	if _, ok := none.Layout("x:dev-1"); ok || none.Builds() != nil {
		t.Fatal("a nil set holds bundles")
	}
}
