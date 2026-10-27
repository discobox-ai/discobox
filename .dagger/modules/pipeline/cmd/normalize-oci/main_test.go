package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizePreservesLayersAndContents(t *testing.T) {
	root := t.TempDir()
	blobs := map[string][]byte{}
	put := func(v []byte, media string) descriptor {
		h := sha256.Sum256(v)
		d := descriptor{Digest: "sha256:" + hex.EncodeToString(h[:]), Size: len(v), MediaType: media}
		blobs["blobs/sha256/"+hex.EncodeToString(h[:])] = v
		return d
	}
	layers := []descriptor{}
	for _, name := range []string{"features/a/", "features/b/"} {
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0755, ModTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
		content := []byte("executable feature")
		if err := tw.WriteHeader(&tar.Header{Name: name + "run", Mode: 0755, Size: int64(len(content)), ModTime: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		layers = append(layers, put(b.Bytes(), "application/vnd.oci.image.layer.v1.tar+gzip"))
	}
	config := put([]byte(`{"architecture":"amd64","os":"linux","config":{"Env":["PATH=/bin"],"Labels":{"custom":"retained"}}}`), "application/vnd.oci.image.config.v1+json")
	raw, err := json.Marshal(document{SchemaVersion: 2, Config: &config, Layers: layers})
	if err != nil {
		t.Fatal(err)
	}
	manifest := put(raw, "application/vnd.oci.image.manifest.v1+json")
	raw, err = json.Marshal(document{SchemaVersion: 2, Manifests: []descriptor{manifest}})
	if err != nil {
		t.Fatal(err)
	}
	blobs["index.json"] = raw
	blobs["oci-layout"] = []byte(`{"imageLayoutVersion":"1.0.0"}`)
	input := filepath.Join(root, "input.tar")
	f, err := os.Create(input)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for name, data := range blobs {
		if err = tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err = tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err = tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(root, "a.tar"), filepath.Join(root, "b.tar")
	if err = normalize(input, a, 315532800); err != nil {
		t.Fatal(err)
	}
	if err = normalize(a, b, 315532800); err != nil {
		t.Fatal(err)
	}
	av, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	bv, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(av, bv) {
		t.Fatal("normalization is not deterministic/idempotent")
	}
	tr := tar.NewReader(bytes.NewReader(av))
	normalized := map[string][]byte{}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		normalized[h.Name], err = io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
	}
	var index document
	if err = json.Unmarshal(normalized["index.json"], &index); err != nil {
		t.Fatal(err)
	}
	read := func(d descriptor) []byte { return normalized["blobs/sha256/"+d.Digest[len("sha256:"):]] }
	var m document
	if err = json.Unmarshal(read(index.Manifests[0]), &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Layers) != 2 {
		t.Fatal("flattened feature layers")
	}
	if !bytes.Contains(read(*m.Config), []byte(`"custom":"retained"`)) {
		t.Fatal("lost config")
	}
	for _, l := range m.Layers {
		gz, e := gzip.NewReader(bytes.NewReader(read(l)))
		if e != nil {
			t.Fatal(e)
		}
		tr = tar.NewReader(gz)
		for {
			h, e := tr.Next()
			if e == io.EOF {
				break
			}
			if e != nil {
				t.Fatal(e)
			}
			if h.ModTime.Unix() != 315532800 || h.Mode != 0755 {
				t.Fatalf("incorrect metadata: %+v", h)
			}
			if h.Typeflag == tar.TypeReg {
				data, e := io.ReadAll(tr)
				if e != nil || string(data) != "executable feature" {
					t.Fatal("changed feature")
				}
			}
		}
		if e = gz.Close(); e != nil {
			t.Fatal(e)
		}
	}
}
