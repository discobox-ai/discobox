// Verify exported OCI artifacts before they are offered for publication.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

type descriptor struct {
	MediaType string `json:"mediaType"`
	Digest    string `json:"digest"`
}
type manifest struct {
	MediaType string       `json:"mediaType"`
	Manifests []descriptor `json:"manifests"`
	Config    descriptor   `json:"config"`
	Layers    []descriptor `json:"layers"`
}

func main() {
	if err := verify(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func verify() error {
	archive := flag.String("archive", "", "OCI archive")
	bundle := flag.Bool("bundle", false, "validate scratch bundle paths and timestamps")
	epoch := flag.Int64("epoch", 0, "canonical bundle epoch")
	flag.Parse()
	f, err := os.Open(*archive)
	if err != nil {
		return err
	}
	defer f.Close()
	blobs := map[string][]byte{}
	r := tar.NewReader(f)
	for {
		header, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(r)
		if err != nil {
			return err
		}
		blobs[path.Clean(header.Name)] = data
	}
	var index manifest
	if err := json.Unmarshal(blobs["index.json"], &index); err != nil {
		return fmt.Errorf("missing/invalid OCI index: %w", err)
	}
	if len(index.Manifests) != 1 {
		return fmt.Errorf("single-platform export has %d manifests", len(index.Manifests))
	}
	readBlob := func(d descriptor) ([]byte, error) {
		parts := strings.SplitN(d.Digest, ":", 2)
		if len(parts) != 2 || parts[0] != "sha256" {
			return nil, fmt.Errorf("unsupported digest %q", d.Digest)
		}
		data, ok := blobs["blobs/sha256/"+parts[1]]
		if !ok {
			return nil, fmt.Errorf("missing blob %s", d.Digest)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != parts[1] {
			return nil, fmt.Errorf("corrupt blob %s", d.Digest)
		}
		return data, nil
	}
	data, err := readBlob(index.Manifests[0])
	if err != nil {
		return err
	}
	var image manifest
	if err := json.Unmarshal(data, &image); err != nil {
		return err
	}
	if len(image.Manifests) > 0 {
		return fmt.Errorf("single-platform export contains a nested platform index")
	}
	if _, err := readBlob(image.Config); err != nil {
		return err
	}
	for _, layer := range image.Layers {
		data, err := readBlob(layer)
		if err != nil {
			return err
		}
		if !*bundle {
			continue
		}
		var stream io.Reader = bytes.NewReader(data)
		if strings.Contains(layer.MediaType, "gzip") {
			gz, err := gzip.NewReader(stream)
			if err != nil {
				return err
			}
			defer gz.Close()
			stream = gz
		}
		tr := tar.NewReader(stream)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			name := strings.TrimPrefix(path.Clean(h.Name), "/")
			if name == "." {
				continue
			}
			if name != "features" && !strings.HasPrefix(name, "features/") {
				return fmt.Errorf("bundle layer contains non-feature path %q", h.Name)
			}
			if strings.HasPrefix(path.Base(name), ".wh.") {
				return fmt.Errorf("bundle contains whiteout %q", name)
			}
			if h.ModTime.Unix() != *epoch {
				return fmt.Errorf("%s timestamp %d differs from canonical epoch %d", name, h.ModTime.Unix(), *epoch)
			}
		}
	}
	fmt.Println("OCI blobs, manifest, and bundle contracts verified")
	return nil
}
