package imagecache

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// dockerArchiveManifest is one entry of Docker's own archive format, the
// manifest.json `docker save` writes and the classic image store reads.
type dockerArchiveManifest struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

// WriteArchive writes the image as a tar archive `docker load` takes,
// checking every blob against its digest on the way out.
//
// One archive serves both of Docker's image stores, because the daemon it is
// loaded into may use either:
//
//   - index.json names the registry's top-level manifest, annotated with the
//     reference. The containerd store imports it as that manifest, so the
//     loaded image's ID and RepoDigests are the digest a sandbox is pinned to.
//     Beneath an index only one platform's manifest is present; the store
//     imports what is there and unpacks the platform it runs.
//   - manifest.json names the config and layers under the reference. The
//     classic store reads only this, and records no registry digest at all,
//     which is why a load into one is followed by a pull by digest that finds
//     every layer already present.
//
// report, when set, is called as blob bytes are written, with the running byte
// count and how many of the image's layers are complete.
func (i *Image) WriteArchive(w io.Writer, report func(current int64, layersComplete int)) error {
	name := i.Reference.Name()
	entry := Descriptor{
		MediaType:   i.Top.MediaType,
		Digest:      i.Top.Digest,
		Size:        i.Top.Size,
		Annotations: map[string]string{AnnotationImageName: name},
	}
	var repoTags []string
	if i.Reference.Tag != "" {
		entry.Annotations[AnnotationRefName] = i.Reference.Tag
		repoTags = []string{i.Reference.qualifiedRepository() + ":" + i.Reference.Tag}
	}
	index, err := json.Marshal(imageIndex{SchemaVersion: 2, MediaType: mediaTypeOCIIndex, Manifests: []Descriptor{entry}})
	if err != nil {
		return err
	}
	layers := make([]string, 0, len(i.Layers))
	for _, layer := range i.Layers {
		layers = append(layers, blobName(layer.Digest))
	}
	manifest, err := json.Marshal([]dockerArchiveManifest{{Config: blobName(i.Config.Digest), RepoTags: repoTags, Layers: layers}})
	if err != nil {
		return err
	}

	tw := tar.NewWriter(w)
	for _, dir := range []string{"blobs/", "blobs/sha256/"} {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: dir, Mode: 0o755, ModTime: archiveTime}); err != nil {
			return err
		}
	}
	for _, file := range []struct {
		name string
		data []byte
	}{
		{layoutFileName, []byte(layoutVersion)},
		{indexFileName, index},
		{"manifest.json", manifest},
	} {
		if err := writeTarFile(tw, file.name, int64(len(file.data)), strings.NewReader(string(file.data))); err != nil {
			return err
		}
	}

	progress := &archiveProgress{report: report}
	written := map[string]bool{}
	blobs := []Descriptor{i.Top, i.Manifest, i.Config}
	for _, blob := range blobs {
		if err := i.writeBlob(tw, blob, written, progress); err != nil {
			return err
		}
	}
	for _, layer := range i.Layers {
		if err := i.writeBlob(tw, layer, written, progress); err != nil {
			return err
		}
		progress.layersComplete++
		progress.emit()
	}
	return tw.Close()
}

// archiveTime is every entry's modification time. The archive is a transport,
// and a fixed time keeps two archives of one image the same bytes.
var archiveTime = time.Unix(0, 0)

func blobName(digest string) string {
	return "blobs/sha256/" + strings.TrimPrefix(digest, "sha256:")
}

// writeBlob copies one blob into the archive once, whatever number of times the
// image names it, and fails the archive if its bytes are not its digest: a
// layout damaged on disk must not load as the image it claims to be.
func (i *Image) writeBlob(tw *tar.Writer, blob Descriptor, written map[string]bool, progress *archiveProgress) error {
	if written[blob.Digest] {
		return nil
	}
	written[blob.Digest] = true
	path, err := i.layout.blobPath(blob.Digest)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("%s: %w", i.Reference.Name(), err)
	}
	defer file.Close()
	hash := sha256.New()
	counted := io.TeeReader(io.TeeReader(file, hash), progress)
	if err := writeTarFile(tw, blobName(blob.Digest), blob.Size, counted); err != nil {
		return fmt.Errorf("%s: blob %s: %w", i.Reference.Name(), blob.Digest, err)
	}
	if got := "sha256:" + hex.EncodeToString(hash.Sum(nil)); got != blob.Digest {
		return fmt.Errorf("%s: blob %s on disk has digest %s", i.Reference.Name(), blob.Digest, got)
	}
	return nil
}

func writeTarFile(tw *tar.Writer, name string, size int64, r io.Reader) error {
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: size, Mode: 0o644, ModTime: archiveTime}); err != nil {
		return err
	}
	// Exactly size bytes: a short file fails here, rather than as a tar the
	// daemon rejects for reasons it describes less well.
	_, err := io.CopyN(tw, r, size)
	return err
}

// archiveProgress counts the blob bytes an archive has written.
type archiveProgress struct {
	report         func(current int64, layersComplete int)
	current        int64
	layersComplete int
}

func (p *archiveProgress) Write(b []byte) (int, error) {
	p.current += int64(len(b))
	p.emit()
	return len(b), nil
}

func (p *archiveProgress) emit() {
	if p.report != nil {
		p.report(p.current, p.layersComplete)
	}
}
