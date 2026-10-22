package devimage

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/imagecache"
)

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// writeLayout writes an OCI image layout tar holding blobs and an index.json
// listing manifests, as `docker buildx build --output type=oci` does.
func writeLayout(t *testing.T, manifests []map[string]string, blobs ...[]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.tar")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	out := tar.NewWriter(file)
	add := func(name string, data []byte) {
		if err := out.WriteHeader(&tar.Header{Name: name, Mode: 0o444, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := out.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := out.WriteHeader(&tar.Header{Name: "blobs/sha256/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	for _, blob := range blobs {
		add("blobs/sha256/"+strings.TrimPrefix(digestOf(blob), "sha256:"), blob)
	}
	index, err := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": manifests})
	if err != nil {
		t.Fatal(err)
	}
	add("index.json", index)
	add("oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`))
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLayoutDigestIsTheOneManifestItsIndexLists(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2}`)
	path := writeLayout(t, []map[string]string{{"mediaType": imagecache.MediaTypeOCIManifest, "digest": digestOf(manifest)}}, manifest)
	got, err := LayoutDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != digestOf(manifest) {
		t.Fatalf("LayoutDigest() = %s, want %s", got, digestOf(manifest))
	}
}

// A build that attached provenance lists an index of the image and its
// attestation, and one of several platforms lists several: neither is one
// bundle's manifest to pin.
func TestLayoutDigestRefusesAnythingButOneManifest(t *testing.T) {
	one := digestOf([]byte("one"))
	for name, manifests := range map[string][]map[string]string{
		"none":      {},
		"two":       {{"mediaType": imagecache.MediaTypeOCIManifest, "digest": one}, {"mediaType": imagecache.MediaTypeOCIManifest, "digest": digestOf([]byte("two"))}},
		"an index":  {{"mediaType": "application/vnd.oci.image.index.v1+json", "digest": one}},
		"no digest": {{"mediaType": imagecache.MediaTypeOCIManifest, "digest": "sha256:short"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := LayoutDigest(writeLayout(t, manifests)); err == nil {
				t.Fatalf("LayoutDigest() = %s, want an error", got)
			}
		})
	}
}

func TestLayoutBlobsAreReadByTheDigestTheirNamesClaim(t *testing.T) {
	config, layer := []byte(`{"config":{}}`), []byte("layer bytes")
	path := writeLayout(t, nil, config, layer)

	walked := map[string]string{}
	if err := WalkLayoutBlobs(path, func(digest string, blob io.Reader) error {
		data, err := io.ReadAll(blob)
		walked[digest] = string(data)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(walked) != 2 || walked[digestOf(config)] != string(config) || walked[digestOf(layer)] != string(layer) {
		t.Fatalf("walked = %v, want the config and the layer by their digests", walked)
	}

	blob, err := OpenLayoutBlob(path, digestOf(layer))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(blob)
	if closeErr := blob.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if string(data) != string(layer) {
		t.Fatalf("OpenLayoutBlob() read %q, want %q", data, layer)
	}
	if _, err := OpenLayoutBlob(path, digestOf([]byte("absent"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("OpenLayoutBlob() of an absent blob = %v, want os.ErrNotExist", err)
	}
}

func TestCheckRefusesALayoutHoldingAnotherBundle(t *testing.T) {
	manifest, other := []byte(`{"schemaVersion":2}`), []byte(`{"schemaVersion":2,"other":true}`)
	path := writeLayout(t, []map[string]string{{"mediaType": imagecache.MediaTypeOCIManifest, "digest": digestOf(manifest)}}, manifest)
	if err := (Bundle{Reference: "b:dev", Digest: digestOf(manifest), Layout: path}).Check(); err != nil {
		t.Fatalf("Check() = %v, want the layout accepted", err)
	}
	if err := (Bundle{Reference: "b:dev", Digest: digestOf(other), Layout: path}).Check(); err == nil {
		t.Fatal("Check() accepted a layout listing another manifest")
	}
}
