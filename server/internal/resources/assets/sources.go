package assets

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	dockerclient "github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/discobox-ai/discobox/imagecache"
	"github.com/discobox-ai/discobox/server/internal/localimage"
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

// daemonSource reads the local Docker daemon, for the bundles the release
// names alone: on a development server those are what the image watcher
// built. Any other image the daemon holds — another project's local build,
// or one pulled on this host with credentials — is not a project's to read
// through the server, so a reference a project supplied is never looked up
// here.
//
// Its content comes out of what the daemon saves, an OCI layout whose blobs
// go into the store whole. What each pinned digest's save held is remembered,
// so a digest it did not hold — another platform's manifest under a
// development build's index — is not asked of the daemon again, while one it
// did hold that has left the store since, pruned or found damaged, is saved
// again.
type daemonSource struct {
	imported *sync.Map
}

func newDaemonSource() daemonSource { return daemonSource{imported: &sync.Map{}} }

func (d daemonSource) fetch(ctx context.Context, p pin, digest string, _ bool, blobs *imagecache.Layout) error {
	if !p.release {
		return errNotHere
	}
	if imported, done := d.imported.Load(p.digest); done {
		if held, ok := imported.(map[string]bool); ok && !held[digest] {
			return errNotHere
		}
	}
	client, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		return errNotHere
	}
	defer client.Close()
	inspected, err := client.ImageInspect(ctx, p.ref)
	if err != nil || !localimage.Is(inspected, p.digest) {
		return errNotHere
	}
	held, err := importSave(ctx, client, p.ref, inspected, blobs)
	if err != nil {
		return fmt.Errorf("read %s from the Docker daemon: %w", p.ref, err)
	}
	d.imported.Store(p.digest, held)
	return nil
}

func (d daemonSource) resolve(ctx context.Context, ref string, release bool, blobs *imagecache.Layout) (string, error) {
	if !release {
		return "", errNotHere
	}
	client, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		return "", errNotHere
	}
	defer client.Close()
	inspected, err := client.ImageInspect(ctx, ref)
	if err != nil {
		return "", errNotHere
	}
	digest := localimage.Digest(inspected)
	if _, done := d.imported.Load(digest); !done || !present(blobs, digest) {
		held, err := importSave(ctx, client, ref, inspected, blobs)
		if err != nil {
			return "", fmt.Errorf("read %s from the Docker daemon: %w", ref, err)
		}
		d.imported.Store(digest, held)
	}
	return digest, nil
}

// present reports whether blobs holds digest.
func present(blobs *imagecache.Layout, digest string) bool {
	body, _, err := blobs.OpenBlob(digest)
	if err != nil {
		return false
	}
	_ = body.Close()
	return true
}

// importSave puts every blob of what the daemon saves for ref into blobs, each
// checked against the digest it is named by, and reports which digests it
// held. The image is asked for whole
// first, which keeps an index the daemon holds every platform of; a daemon
// holding one platform of an index refuses that, and is asked for its own.
func importSave(ctx context.Context, client *dockerclient.Client, ref string, inspected dockerclient.ImageInspectResult, blobs *imagecache.Layout) (map[string]bool, error) {
	saved, err := client.ImageSave(ctx, []string{ref})
	if err != nil {
		saved, err = client.ImageSave(ctx, []string{ref}, dockerclient.ImageSaveWithPlatforms(ocispec.Platform{OS: inspected.Os, Architecture: inspected.Architecture}))
	}
	if err != nil {
		return nil, err
	}
	defer saved.Close()
	held := map[string]bool{}
	entries := tar.NewReader(saved)
	for {
		header, err := entries.Next()
		if errors.Is(err, io.EOF) {
			return held, nil
		}
		if err != nil {
			return nil, err
		}
		hex, ok := strings.CutPrefix(header.Name, "blobs/sha256/")
		if !ok || header.Typeflag != tar.TypeReg {
			continue
		}
		if err := blobs.PutBlob("sha256:"+hex, entries); err != nil {
			return nil, err
		}
		held["sha256:"+hex] = true
	}
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
