package devimage

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestManifestRoundTripIsCanonical(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images.json")
	manifest, err := NewManifest([]Image{
		{Reference: "example/z:dev", ID: "sha256:zzz"},
		{Reference: "example/a:dev", ID: "sha256:aaa"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(path, manifest); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []Image{
		{Reference: "example/a:dev", ID: "sha256:aaa"},
		{Reference: "example/z:dev", ID: "sha256:zzz"},
	}
	if !reflect.DeepEqual(got.Images, want) {
		t.Fatalf("Images = %#v, want %#v", got.Images, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no POSIX permission bits: the perm argument to os.WriteFile
	// maps only to the read-only attribute, so Perm() reads back 0666 whatever
	// was asked for. The property here is a POSIX one, carried on Windows by
	// the ACL inherited from the parent directory, which this cannot express.
	if runtime.GOOS != "windows" {
		if info.Mode().Perm() != 0o644 {
			t.Fatalf("manifest mode = %o, want 644", info.Mode().Perm())
		}
	}

}

func TestManifestRejectsDuplicateReference(t *testing.T) {
	_, err := NewManifest([]Image{
		{Reference: "example/dev:latest", ID: "sha256:aaa"},
		{Reference: "example/dev:latest", ID: "sha256:bbb"},
	}, nil)
	if err == nil {
		t.Fatal("NewManifest() succeeded with a duplicate reference")
	}
}

func TestReadRejectsUnsupportedVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images.json")
	if err := os.WriteFile(path, []byte(`{"version":2,"images":[{"reference":"example:dev","id":"sha256:aaa"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err == nil {
		t.Fatal("Read() succeeded with an unsupported version")
	}
}

func TestManifestCarriesBundlesBesideImages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images.json")
	digest := "sha256:" + strings.Repeat("0a", 32)
	layout := filepath.Join(t.TempDir(), "discobox-platform.tar")
	manifest, err := NewManifest([]Image{{Reference: "example/a:dev", ID: "sha256:aaa"}},
		[]Bundle{{Reference: "discobox-platform:dev-0a0a0a0a0a0a", Digest: digest, Layout: layout}})
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(path, manifest); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []Bundle{{Reference: "discobox-platform:dev-0a0a0a0a0a0a", Digest: digest, Layout: layout}}
	if !reflect.DeepEqual(got.Bundles, want) {
		t.Fatalf("Bundles = %#v, want %#v", got.Bundles, want)
	}
}

func TestManifestRejectsAnIncompleteBundle(t *testing.T) {
	digest := "sha256:" + strings.Repeat("0a", 32)
	layout := filepath.Join(t.TempDir(), "bundle.tar")
	images := []Image{{Reference: "example/a:dev", ID: "sha256:aaa"}}
	for name, bundle := range map[string]Bundle{
		"no reference":      {Digest: digest, Layout: layout},
		"no digest":         {Reference: "b:dev", Layout: layout},
		"a short digest":    {Reference: "b:dev", Digest: "sha256:0a", Layout: layout},
		"a relative layout": {Reference: "b:dev", Digest: digest, Layout: "build/assets/b.tar"},
		"an image's name":   {Reference: "example/a:dev", Digest: digest, Layout: layout},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewManifest(images, []Bundle{bundle}); err == nil {
				t.Fatalf("NewManifest() accepted the bundle %#v", bundle)
			}
		})
	}
}
