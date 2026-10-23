// Package bundlestore is the box's content store (ADR 26-10-16-423 §7): the
// bundles the box is pinned to, fetched from assets.discobox.internal into
// agentlayer.StoreDir on its root filesystem. Layers are kept by digest,
// extracted once, and each bundle's tree by manifest digest, its layers side
// by side. Apply fetches only what the store lacks, so an upgrade downloads
// the layers that changed and nothing it already holds.
package bundlestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/pkg/agentlayer"
	"github.com/discobox-ai/discobox/pkg/imagecache"
	"github.com/discobox-ai/discobox/sandbox-agent/provision"
)

// maxManifestBytes bounds a manifest or an index, which name digests and
// nothing more.
const maxManifestBytes = 4 << 20

// stateFile records the trees the store's current and previous pins name,
// which are what it keeps (Store.collect).
const stateFile = agentlayer.StoreDir + "/state.json"

// Fetcher reads a path of the asset host: /<name>/manifests/<digest> or
// /blobs/<digest>. What it returns is checked against its digest by the
// store, never trusted.
type Fetcher interface {
	Fetch(ctx context.Context, path string) (io.ReadCloser, error)
}

// Store is the content store under Root, the box's own root but in tests.
type Store struct {
	Root     string
	Fetcher  Fetcher
	Platform imagecache.Platform
	Logger   *slog.Logger
}

// state is stateFile: the tree each pin resolved to, now and before, and
// which tree each pin digest it kept resolved to, so a pin whose tree the
// store holds is applied with nothing fetched (ADR 26-10-16-423 §7: a box
// booted with its plan in its store fetches nothing).
type state struct {
	Current  []string          `json:"current"`
	Previous []string          `json:"previous,omitempty"`
	Resolved map[string]string `json:"resolved,omitempty"`
}

// treeMeta sits beside a tree, TreeDir(digest)+".json": the layers it is
// made of, which keep them.
type treeMeta struct {
	Layers []string `json:"layers"`
}

func (s *Store) path(p string) string { return filepath.Join(s.Root, filepath.FromSlash(p)) }

func (s *Store) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// Apply makes pins the box's bundles: each pin's tree in the store, the
// platform's at provision.PlatformTree and the harness's at
// provision.HarnessTree, where provisioning reads them. The trees the
// current and previous pins name are kept, for rollback, and every other
// tree and every layer no kept tree uses is removed.
func (s *Store) Apply(ctx context.Context, pins agentlayer.Pins) error {
	if err := pins.Validate(); err != nil {
		return err
	}
	for _, dir := range []string{agentlayer.LayersDir, agentlayer.TreesDir} {
		if err := os.MkdirAll(s.path(dir), 0o755); err != nil {
			return err
		}
	}
	recorded, err := s.readState()
	if err != nil {
		s.logger().Warn("read the content store's state; keeping only the current trees", "error", err)
	}
	resolved := map[string]string{}
	platformTree, err := s.tree(ctx, pins.Platform, recorded.Resolved)
	if err != nil {
		return fmt.Errorf("platform bundle %s@%s: %w", pins.Platform.Name, pins.Platform.Digest, err)
	}
	resolved[pins.Platform.Digest] = platformTree
	current := []string{platformTree}
	harnessTree := ""
	if pins.Harness != nil {
		if harnessTree, err = s.tree(ctx, *pins.Harness, recorded.Resolved); err != nil {
			return fmt.Errorf("harness bundle %s@%s: %w", pins.Harness.Name, pins.Harness.Digest, err)
		}
		resolved[pins.Harness.Digest] = harnessTree
		current = append(current, harnessTree)
	}
	if err := s.point(provision.PlatformTree, platformTree); err != nil {
		return err
	}
	if harnessTree == "" {
		if err := s.removeTreeLink(provision.HarnessTree); err != nil {
			return err
		}
	} else if err := s.point(provision.HarnessTree, harnessTree); err != nil {
		return err
	}
	next := state{Current: current, Previous: recorded.Previous}
	if !slices.Equal(recorded.Current, current) && len(recorded.Current) > 0 {
		next.Previous = recorded.Current
	}
	// What the previous pins resolved to is kept with their trees.
	for pin, tree := range recorded.Resolved {
		if _, ok := resolved[pin]; !ok && slices.Contains(next.Previous, tree) {
			resolved[pin] = tree
		}
	}
	next.Resolved = resolved
	if err := s.writeState(next); err != nil {
		return err
	}
	return s.collect(next)
}

// tree is the tree of pin's image manifest for the store's platform,
// composed of its layers, fetching what the store lacks. It returns the
// image manifest's digest, which names the tree.
func (s *Store) tree(ctx context.Context, pin agentlayer.Pin, known map[string]string) (string, error) {
	if digest, ok := known[pin.Digest]; ok && imagecache.IsDigest(digest) {
		if _, err := os.Stat(s.path(agentlayer.TreeDir(digest) + ".json")); err == nil {
			return digest, nil
		}
	}
	manifest, digest, err := imagecache.ImageManifest(pin.Digest, s.Platform, func(digest string) (imagecache.Manifest, error) {
		read, _, err := s.manifest(ctx, pin.Name, digest)
		return read, err
	})
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(s.path(agentlayer.TreeDir(digest) + ".json")); err == nil {
		return digest, nil
	}
	var layers []string
	for _, layer := range manifest.Layers {
		if !imagecache.IsDigest(layer.Digest) {
			return "", fmt.Errorf("a layer digest %q is not a sha256 digest", layer.Digest)
		}
		if err := s.layer(ctx, layer.Digest); err != nil {
			return "", fmt.Errorf("layer %s: %w", layer.Digest, err)
		}
		layers = append(layers, layer.Digest)
	}
	if err := s.compose(digest, layers); err != nil {
		return "", fmt.Errorf("compose its tree: %w", err)
	}
	return digest, nil
}

// manifest fetches the manifest or index digest of name, checked against
// the digest.
func (s *Store) manifest(ctx context.Context, name, digest string) (imagecache.Manifest, string, error) {
	body, err := s.Fetcher.Fetch(ctx, "/"+name+"/manifests/"+digest)
	if err != nil {
		return imagecache.Manifest{}, "", err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, maxManifestBytes+1))
	if err != nil {
		return imagecache.Manifest{}, "", fmt.Errorf("read manifest %s: %w", digest, err)
	}
	if len(data) > maxManifestBytes {
		return imagecache.Manifest{}, "", fmt.Errorf("manifest %s is larger than %d bytes", digest, maxManifestBytes)
	}
	if got := digestOf(data); got != digest {
		return imagecache.Manifest{}, "", fmt.Errorf("manifest %s arrived as %s", digest, got)
	}
	manifest, err := imagecache.ParseManifest(data)
	if err != nil {
		return imagecache.Manifest{}, "", fmt.Errorf("manifest %s: %w", digest, err)
	}
	return manifest, digest, nil
}

// layer puts the layer digest in the store, unless its marker says it is
// whole there already — extracted by an earlier Apply, or placed with the
// agent by the pool. The blob is checked against its digest before a byte
// of it is extracted.
func (s *Store) layer(ctx context.Context, digest string) error {
	if _, err := os.Stat(s.path(agentlayer.LayerComplete(digest))); err == nil {
		return nil
	}
	layers := s.path(agentlayer.LayersDir)
	blob, err := os.CreateTemp(layers, ".blob-*")
	if err != nil {
		return err
	}
	defer os.Remove(blob.Name())
	defer blob.Close()
	body, err := s.Fetcher.Fetch(ctx, "/blobs/"+digest)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(blob, hash), body)
	body.Close()
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	if got := "sha256:" + hex.EncodeToString(hash.Sum(nil)); got != digest {
		return fmt.Errorf("arrived as %s", got)
	}
	if _, err := blob.Seek(0, io.SeekStart); err != nil {
		return err
	}
	staged, err := os.MkdirTemp(layers, "."+agentlayer.Hex(digest)+".*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staged)
	if err := os.Chmod(staged, 0o755); err != nil {
		return err
	}
	if err := provision.UnpackLayer(blob, staged); err != nil {
		return err
	}
	dir := s.path(agentlayer.LayerDir(digest))
	// A directory with no marker is a placement or an extraction cut short.
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.Rename(staged, dir); err != nil {
		return err
	}
	return os.WriteFile(s.path(agentlayer.LayerComplete(digest)), nil, 0o600)
}

// compose builds the tree of the manifest digest from its layers, side by
// side: each layer's files linked into one directory, which is refused when
// two layers both hold a file at one path.
func (s *Store) compose(digest string, layers []string) error {
	trees := s.path(agentlayer.TreesDir)
	staged, err := os.MkdirTemp(trees, "."+agentlayer.Hex(digest)+".*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staged)
	if err := os.Chmod(staged, 0o755); err != nil {
		return err
	}
	for _, layer := range layers {
		if err := overlay(s.path(agentlayer.LayerDir(layer)), staged); err != nil {
			return fmt.Errorf("layer %s: %w", layer, err)
		}
	}
	dir := s.path(agentlayer.TreeDir(digest))
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.Rename(staged, dir); err != nil {
		return err
	}
	meta, err := json.Marshal(treeMeta{Layers: layers})
	if err != nil {
		return err
	}
	return writeFileAtomic(dir+".json", meta)
}

// overlay links every file of the layer at src into dst.
func overlay(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil || rel == "." {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		existing, statErr := os.Lstat(target)
		switch {
		case d.IsDir():
			if statErr == nil {
				if !existing.IsDir() {
					return fmt.Errorf("%s is a directory here and a file in an earlier layer", rel)
				}
				return nil
			}
			return os.Mkdir(target, info.Mode().Perm())
		case statErr == nil:
			return fmt.Errorf("%s is in an earlier layer too", rel)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			//nolint:gosec // G122: both trees are the store's, under a directory only root writes; the only party that could race these paths is the box's root.
			return os.Symlink(link, target)
		default:
			//nolint:gosec // G122: as above.
			if err := os.Link(p, target); err == nil {
				return nil
			}
			return copyFile(p, target, info.Mode().Perm())
		}
	})
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return err
}

// point makes the tree path at, provision.PlatformTree or HarnessTree, a
// link to the tree of digest, replaced whole.
func (s *Store) point(at, digest string) error {
	link := s.path(at)
	target := "trees/" + agentlayer.Hex(digest)
	if current, err := os.Readlink(link); err == nil && current == target {
		return nil
	}
	tmp := link + ".tmp"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

// removeTreeLink removes whatever is at the tree path at: a box whose pins
// name no harness bundle has no harness tree.
func (s *Store) removeTreeLink(at string) error {
	if err := os.RemoveAll(s.path(at)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) readState() (state, error) {
	data, err := os.ReadFile(s.path(stateFile))
	if errors.Is(err, fs.ErrNotExist) {
		return state{}, nil
	}
	if err != nil {
		return state{}, err
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return state{}, fmt.Errorf("decode %s: %w", stateFile, err)
	}
	return st, nil
}

func (s *Store) writeState(st state) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path(stateFile), data)
}

// collect removes every tree st does not keep and every layer no kept tree
// uses, but the agent's, which CurrentLink runs from. Leftovers of an
// interrupted fetch or composition go too.
func (s *Store) collect(st state) error {
	keepTrees := map[string]bool{}
	keepLayers := map[string]bool{}
	for _, digest := range append(append([]string{}, st.Current...), st.Previous...) {
		keepTrees[agentlayer.Hex(digest)] = true
		data, err := os.ReadFile(s.path(agentlayer.TreeDir(digest) + ".json"))
		if err != nil {
			continue
		}
		var meta treeMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}
		for _, layer := range meta.Layers {
			keepLayers[agentlayer.Hex(layer)] = true
		}
	}
	if agent, err := os.Readlink(s.path(agentlayer.CurrentLink)); err == nil {
		keepLayers[filepath.Base(agent)] = true
	}
	var errs []error
	errs = append(errs, sweep(s.path(agentlayer.TreesDir), keepTrees, ".json"))
	errs = append(errs, sweep(s.path(agentlayer.LayersDir), keepLayers, ".complete"))
	return errors.Join(errs...)
}

// sweep removes each entry of dir whose name, less suffix, keep does not
// name.
func sweep(dir string, keep map[string]bool, suffix string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), suffix)
		if keep[name] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
