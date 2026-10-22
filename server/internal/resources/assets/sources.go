package assets

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/discobox-ai/discobox/pkg/devimage"
	"github.com/discobox-ai/discobox/pkg/imagecache"
	"github.com/discobox-ai/discobox/server/internal/registryauth"
)

// errNotHere is a source not holding what it was asked for, which sends the
// read on to the next.
var errNotHere = errors.New("not held here")

// source is somewhere a bundle's content is. fetch puts digest, a manifest or
// a blob beneath p, into blobs; resolve answers the manifest digest ref names
// now and leaves that manifest in blobs. Either answers errNotHere for what it
// does not hold.
type source interface {
	fetch(ctx context.Context, p pin, digest string, isManifest bool, blobs *imagecache.Layout) error
	resolve(ctx context.Context, ref string, release bool, blobs *imagecache.Layout) (string, error)
}

// layoutSource reads the bundles the image watcher exported as OCI image
// layouts (devimage.Bundle), for the release's references alone: on a
// development server, the platform bundle and the built-in harnesses' bundles
// it is configured with. A reference a project supplied is never looked up
// here, even one naming a development bundle.
//
// A pin's content is the whole of its layout, which goes into the store at
// once and only while the layout's index still lists the pin's digest
// (devimage.Bundle.Check). Nothing is remembered between reads, so a blob that
// has left the store since, pruned or found damaged, is put back from the
// layout as long as the watcher keeps it.
type layoutSource struct {
	bundles map[string]devimage.Bundle
}

func newLayoutSource(bundles []devimage.Bundle) layoutSource {
	byRef := make(map[string]devimage.Bundle, len(bundles))
	for _, bundle := range bundles {
		byRef[bundle.Reference] = bundle
	}
	return layoutSource{bundles: byRef}
}

func (l layoutSource) fetch(_ context.Context, p pin, digest string, _ bool, blobs *imagecache.Layout) error {
	bundle, ok := l.bundles[p.ref]
	if !p.release || !ok || bundle.Digest != p.digest {
		return errNotHere
	}
	if err := importLayout(bundle, blobs); err != nil {
		return err
	}
	if !blobs.HasBlob(digest) {
		return errNotHere
	}
	return nil
}

func (l layoutSource) resolve(_ context.Context, ref string, release bool, blobs *imagecache.Layout) (string, error) {
	bundle, ok := l.bundles[ref]
	if !release || !ok {
		return "", errNotHere
	}
	if err := importLayout(bundle, blobs); err != nil {
		return "", err
	}
	return bundle.Digest, nil
}

// importLayout puts every blob of bundle's layout into blobs, each checked
// against the digest it is named by. One the store holds already is skipped
// unread.
func importLayout(bundle devimage.Bundle, blobs *imagecache.Layout) error {
	if err := bundle.Check(); err != nil {
		return fmt.Errorf("read %s: %w", bundle.Reference, err)
	}
	if err := devimage.WalkLayoutBlobs(bundle.Layout, blobs.PutBlob); err != nil {
		return fmt.Errorf("read %s: %w", bundle.Reference, err)
	}
	return nil
}

// registrySource reads a bundle's registry. The server's credentials are
// offered only for a reference the release names; any other — one a project
// supplied — is read anonymously, since a project's own registry credentials
// are not the server's to lend.
type registrySource struct{}

func (registrySource) options(ctx context.Context, release bool) []remote.Option {
	options := []remote.Option{remote.WithContext(ctx)}
	if release {
		return append(options, remote.WithAuthFromKeychain(registryauth.Keychain()))
	}
	return append(options, remote.WithAuth(authn.Anonymous))
}

func (r registrySource) fetch(ctx context.Context, p pin, digest string, isManifest bool, blobs *imagecache.Layout) error {
	ref, err := name.ParseReference(p.ref)
	if err != nil {
		return errNotHere
	}
	at := ref.Context().Digest(digest)
	if isManifest {
		descriptor, err := remote.Get(at, r.options(ctx, p.release)...)
		if err != nil {
			return fmt.Errorf("fetch %s: %w", at, registryauth.Explain(ref, err))
		}
		return blobs.PutBlob(digest, bytes.NewReader(descriptor.Manifest))
	}
	layer, err := remote.Layer(at, r.options(ctx, p.release)...)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", at, registryauth.Explain(ref, err))
	}
	body, err := layer.Compressed()
	if err != nil {
		return fmt.Errorf("fetch %s: %w", at, registryauth.Explain(ref, err))
	}
	defer body.Close()
	return blobs.PutBlob(digest, body)
}

func (r registrySource) resolve(ctx context.Context, refName string, release bool, blobs *imagecache.Layout) (string, error) {
	ref, err := name.ParseReference(refName)
	if err != nil {
		return "", fmt.Errorf("parse %q: %w", refName, err)
	}
	// The registry's own top-level manifest, an index for a multi-platform
	// bundle: no platform is chosen, since pools of every platform it lists
	// read it.
	descriptor, err := remote.Get(ref, r.options(ctx, release)...)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", refName, registryauth.Explain(ref, err))
	}
	digest := descriptor.Digest.String()
	if err := blobs.PutBlob(digest, bytes.NewReader(descriptor.Manifest)); err != nil {
		return "", err
	}
	return digest, nil
}
