// Normalize OCI layer timestamps without flattening reusable feature layers.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int               `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Platform    map[string]any    `json:"platform,omitempty"`
}
type document struct {
	SchemaVersion int               `json:"schemaVersion"`
	MediaType     string            `json:"mediaType,omitempty"`
	Manifests     []descriptor      `json:"manifests,omitempty"`
	Config        *descriptor       `json:"config,omitempty"`
	Layers        []descriptor      `json:"layers,omitempty"`
	Annotations   map[string]string `json:"annotations,omitempty"`
}

func main() {
	input := flag.String("input", "", "input OCI archive")
	output := flag.String("output", "", "output OCI archive")
	epoch := flag.Int64("epoch", 0, "layer timestamp")
	flag.Parse()
	if err := normalize(*input, *output, *epoch); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func normalize(input, output string, epoch int64) error {
	f, err := os.Open(input)
	if err != nil {
		return err
	}
	defer f.Close()
	blobs := map[string][]byte{}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		blobs[strings.TrimPrefix(h.Name, "./")] = data
	}
	read := func(d descriptor) ([]byte, error) {
		data, ok := blobs["blobs/"+strings.Replace(d.Digest, ":", "/", 1)]
		if !ok {
			return nil, fmt.Errorf("missing blob %s", d.Digest)
		}
		sum := sha256.Sum256(data)
		if d.Digest != "sha256:"+hex.EncodeToString(sum[:]) {
			return nil, fmt.Errorf("corrupt blob %s", d.Digest)
		}
		return data, nil
	}
	put := func(data []byte) descriptor {
		sum := sha256.Sum256(data)
		digest := hex.EncodeToString(sum[:])
		blobs["blobs/sha256/"+digest] = data
		return descriptor{Digest: "sha256:" + digest, Size: len(data)}
	}
	var index document
	if err = json.Unmarshal(blobs["index.json"], &index); err != nil {
		return err
	}
	if len(index.Manifests) != 1 {
		return fmt.Errorf("expected one manifest")
	}
	raw, err := read(index.Manifests[0])
	if err != nil {
		return err
	}
	var manifest document
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return err
	}
	if manifest.Config == nil {
		return fmt.Errorf("missing config")
	}
	configRaw, err := read(*manifest.Config)
	if err != nil {
		return err
	}
	var config map[string]any
	if err = json.Unmarshal(configRaw, &config); err != nil {
		return err
	}
	diffIDs := []string{}
	stamp := time.Unix(epoch, 0).UTC()
	for i, layer := range manifest.Layers {
		raw, err = read(layer)
		if err != nil {
			return err
		}
		var reader io.Reader = bytes.NewReader(raw)
		if strings.Contains(layer.MediaType, "gzip") {
			gz, err := gzip.NewReader(reader)
			if err != nil {
				return err
			}
			defer gz.Close()
			reader = gz
		} else if layer.MediaType != "application/vnd.oci.image.layer.v1.tar" {
			return fmt.Errorf("unsupported layer type %s", layer.MediaType)
		}
		var plain bytes.Buffer
		tw := tar.NewWriter(&plain)
		tr = tar.NewReader(reader)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			h.ModTime = stamp
			h.AccessTime = time.Time{}
			h.ChangeTime = time.Time{}
			for _, key := range []string{"mtime", "atime", "ctime"} {
				delete(h.PAXRecords, key)
			}
			if err = tw.WriteHeader(h); err != nil {
				return err
			}
			if _, err = io.Copy(tw, tr); err != nil {
				return err
			}
		}
		if err = tw.Close(); err != nil {
			return err
		}
		sum := sha256.Sum256(plain.Bytes())
		diffIDs = append(diffIDs, "sha256:"+hex.EncodeToString(sum[:]))
		var compressed bytes.Buffer
		gz := gzip.NewWriter(&compressed)
		if _, err = gz.Write(plain.Bytes()); err != nil {
			return err
		}
		if err = gz.Close(); err != nil {
			return err
		}
		d := put(compressed.Bytes())
		d.MediaType = "application/vnd.oci.image.layer.v1.tar+gzip"
		manifest.Layers[i] = d
	}
	config["rootfs"] = map[string]any{"type": "layers", "diff_ids": diffIDs}
	config["created"] = stamp.Format(time.RFC3339)
	if history, ok := config["history"].([]any); ok {
		for _, h := range history {
			if entry, ok := h.(map[string]any); ok {
				entry["created"] = stamp.Format(time.RFC3339)
			}
		}
	}
	raw, err = json.Marshal(config)
	if err != nil {
		return err
	}
	configDescriptor := put(raw)
	configDescriptor.MediaType = manifest.Config.MediaType
	manifest.Config = &configDescriptor
	raw, err = json.Marshal(manifest)
	if err != nil {
		return err
	}
	d := put(raw)
	d.MediaType = index.Manifests[0].MediaType
	d.Platform = index.Manifests[0].Platform
	d.Annotations = index.Manifests[0].Annotations
	index.Manifests[0] = d
	raw, err = json.Marshal(index)
	if err != nil {
		return err
	}
	blobs["index.json"] = raw
	// Write only reachable blobs, so stale timestamped layers are not exported.
	names := []string{"oci-layout", "index.json", "blobs/" + strings.Replace(d.Digest, ":", "/", 1), "blobs/" + strings.Replace(manifest.Config.Digest, ":", "/", 1)}
	for _, layer := range manifest.Layers {
		names = append(names, "blobs/"+strings.Replace(layer.Digest, ":", "/", 1))
	}
	sort.Strings(names)
	out, err := os.Create(output)
	if err != nil {
		return err
	}
	defer out.Close()
	tw := tar.NewWriter(out)
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		data := blobs[name]
		if err = tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data)), ModTime: stamp}); err != nil {
			return err
		}
		if _, err = tw.Write(data); err != nil {
			return err
		}
	}
	return tw.Close()
}
