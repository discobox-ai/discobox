package imagecache

import (
	"bytes"
	"testing"
)

func TestParseManifestRefusesWhatIsNoManifest(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{"an image config", `{"architecture":"amd64","os":"linux","config":{"Labels":{}}}`},
		{"an image config naming a schema", `{"schemaVersion":2,"config":{"Labels":{}}}`},
		{"an unknown media type", `{"schemaVersion":2,"mediaType":"text/plain","manifests":[]}`},
		{"no JSON", `layer bytes`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if read, err := ParseManifest([]byte(tc.data)); err == nil {
				t.Fatalf("parsed as %s", read.MediaType)
			}
		})
	}
}

// An OCI manifest may state no media type: an index is named by its
// manifests, an image manifest by its config.
func TestParseManifestNamesWhatStatesNoMediaType(t *testing.T) {
	config := "sha256:" + string(bytes.Repeat([]byte("a"), 64))
	index, err := ParseManifest([]byte(`{"schemaVersion":2,"manifests":[]}`))
	if err != nil || index.MediaType != MediaTypeOCIIndex {
		t.Fatalf("index = %+v, %v", index, err)
	}
	image, err := ParseManifest([]byte(`{"schemaVersion":2,"config":{"digest":"` + config + `"},"layers":[]}`))
	if err != nil || image.MediaType != MediaTypeOCIManifest || !image.References(config) {
		t.Fatalf("image manifest = %+v, %v", image, err)
	}
}

// Reach keeps what a pin reaches in the layout — an index, the manifests it
// lists, their configs and layers — and reads nothing it lacks.
func TestReachWalksWhatTheLayoutHolds(t *testing.T) {
	layout := Open(t.TempDir())
	put := func(data string) string {
		digest := digestOf([]byte(data))
		if err := layout.PutBlob(digest, bytes.NewReader([]byte(data))); err != nil {
			t.Fatal(err)
		}
		return digest
	}
	config, layer := put("config"), put("layer")
	missing := "sha256:" + string(bytes.Repeat([]byte("b"), 64))
	image := put(`{"schemaVersion":2,"mediaType":"` + MediaTypeOCIManifest + `","config":{"digest":"` + config + `"},"layers":[{"digest":"` + layer + `"}]}`)
	index := put(`{"schemaVersion":2,"manifests":[{"digest":"` + image + `"},{"digest":"` + missing + `"}]}`)
	keep := map[string]bool{}
	layout.Reach(index, keep)
	for _, digest := range []string{index, image, config, layer, missing} {
		if !keep[digest] {
			t.Fatalf("%s not kept", digest)
		}
	}
	if len(keep) != 5 {
		t.Fatalf("kept %d digests, want 5", len(keep))
	}
}
