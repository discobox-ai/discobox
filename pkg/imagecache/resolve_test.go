package imagecache

import (
	"errors"
	"testing"

	"github.com/discobox-ai/discobox/pkg/imagecache/imagecachetest"
)

func TestResolve(t *testing.T) {
	registry := imagecachetest.NewRegistry(t)
	image := registry.Publish("example/discobox-vm", "v1", []byte("guest"))
	ctx := t.Context()

	got, err := Resolve(ctx, image.Reference, registry.Client())
	if err != nil || got != image.Index {
		t.Fatalf("Resolve(%s) = %q, %v; want the index digest %s", image.Reference, got, err, image.Index)
	}
	if _, err := Resolve(ctx, registry.Host()+"/example/discobox-vm:v9", registry.Client()); !errors.Is(err, ErrNoManifest) {
		t.Fatalf("an unpublished tag: err = %v, want ErrNoManifest", err)
	}

	registry.RequireLogin("u", "p")
	if _, err := Resolve(ctx, image.Reference, registry.Client()); err == nil || errors.Is(err, ErrNoManifest) {
		t.Fatalf("a private repository: err = %v, want a refusal that is not ErrNoManifest", err)
	}
}
