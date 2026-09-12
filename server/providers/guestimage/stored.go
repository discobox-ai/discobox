package guestimage

import (
	"fmt"
	"io"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/partial"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/discobox-ai/discobox/imagecache"
)

// storedImage presents an image in the image store as a go-containerregistry
// image, so extracting a guest is the mutate.Extract it always was, whatever
// fetched its bytes. Every blob is read through the store's verifying reader.
func storedImage(image *imagecache.Image) (v1.Image, error) {
	manifest, err := readAll(image, image.Manifest)
	if err != nil {
		return nil, err
	}
	config, err := readAll(image, image.Config)
	if err != nil {
		return nil, err
	}
	return partial.CompressedToImage(&storedCore{image: image, manifest: manifest, config: config})
}

func readAll(image *imagecache.Image, blob imagecache.Descriptor) ([]byte, error) {
	reader, err := image.Open(blob)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

type storedCore struct {
	image    *imagecache.Image
	manifest []byte
	config   []byte
}

func (s *storedCore) RawConfigFile() ([]byte, error) { return s.config, nil }

func (s *storedCore) RawManifest() ([]byte, error) { return s.manifest, nil }

func (s *storedCore) MediaType() (types.MediaType, error) {
	return types.MediaType(s.image.Manifest.MediaType), nil
}

func (s *storedCore) LayerByDigest(hash v1.Hash) (partial.CompressedLayer, error) {
	for _, layer := range s.image.Layers {
		if layer.Digest == hash.String() {
			return &storedLayer{image: s.image, blob: layer}, nil
		}
	}
	return nil, fmt.Errorf("%s has no layer %s", s.image.Reference.Name(), hash)
}

type storedLayer struct {
	image *imagecache.Image
	blob  imagecache.Descriptor
}

func (l *storedLayer) Digest() (v1.Hash, error) { return v1.NewHash(l.blob.Digest) }

func (l *storedLayer) Compressed() (io.ReadCloser, error) { return l.image.Open(l.blob) }

func (l *storedLayer) Size() (int64, error) { return l.blob.Size, nil }

func (l *storedLayer) MediaType() (types.MediaType, error) {
	return types.MediaType(l.blob.MediaType), nil
}
