// Package agentlayer is how the sandbox agent reaches a box (ADR 26-10-16-423
// §7): the platform bundle's agent layer, placed in the box's content store
// before it boots, the units that start the agent from it, and the pins that
// say which bundles the box applies.
//
// A pool's Docker runtime writes Archive into a sandbox container between
// create and start, so systemd stays PID 1 and discobox-wire.service still
// wires the volumes before sysinit.target, and the box then fetches the rest
// of its bundles into the same store itself.
package agentlayer

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pkg/imagecache"
)

// The box's content store (ADR 26-10-16-423 §7), on its root filesystem so a
// snapshot keeps it with the step records of what it provisioned.
const (
	// StoreDir is the store.
	StoreDir = "/opt/discobox/bundles"
	// LayersDir holds each layer extracted once, by digest (LayerDir).
	LayersDir = StoreDir + "/layers"
	// TreesDir holds each bundle's tree, its layers side by side, by
	// manifest digest.
	TreesDir = StoreDir + "/trees"
	// PinsFile is the pins the box applies, placed with the agent before
	// every start (Pins).
	PinsFile = StoreDir + "/pins.json"
	// BundlesDir holds a link for each bundle the box applies, by its name,
	// to the tree its pin resolved to (BundleTree).
	BundlesDir = StoreDir + "/by-name"
	// CurrentLink is the stable path the agent runs from: a link to the
	// directory of the agent layer placed last.
	CurrentLink = "/opt/discobox/current"
	// AgentFile is the agent's path within its layer, the agent bundle's one
	// (ADR 26-10-24-953 §2).
	AgentFile = "features/core/discobox-sandbox-agent"
	// AgentPath is the agent the units run.
	AgentPath = CurrentLink + "/" + AgentFile
)

// completeSuffix names the marker beside a layer's directory that says the
// directory holds the whole layer. It is written last — by the store after
// it renames a layer into place, and as an archive's last entry — so a
// placement cut short leaves a layer the box fetches again rather than one
// it trusts.
const completeSuffix = ".complete"

// LayerDir is where the store keeps the layer digest, extracted.
func LayerDir(digest string) string { return LayersDir + "/" + Hex(digest) }

// LayerComplete is the marker that says LayerDir(digest) holds the whole
// layer.
func LayerComplete(digest string) string { return LayerDir(digest) + completeSuffix }

// TreeDir is where the store keeps the tree of the manifest digest.
func TreeDir(digest string) string { return TreesDir + "/" + Hex(digest) }

// BundleTree is the path the box reads the bundle name's tree at: a link,
// pointed at the tree of whatever digest the bundle is pinned to now, so the
// path a step links from stays the same across upgrades and follows the
// tree.
func BundleTree(name string) string { return BundlesDir + "/" + name }

// Hex is a digest's hex, which names its directories.
func Hex(digest string) string { return strings.TrimPrefix(digest, "sha256:") }

// Pin is one bundle a box is pinned to, as the asset API addresses it.
type Pin struct {
	// Name is the bundle's name at assets.discobox.internal.
	Name string `json:"name"`
	// Digest is the manifest or index digest it is pinned to.
	Digest string `json:"digest"`
}

func (p Pin) validate(what string) error {
	if strings.TrimSpace(p.Name) == "" || strings.ContainsAny(p.Name, "/\\") {
		return fmt.Errorf("%s bundle name %q is not one the asset API addresses", what, p.Name)
	}
	if !imagecache.IsDigest(p.Digest) {
		return fmt.Errorf("%s bundle digest %q is not a sha256 digest", what, p.Digest)
	}
	return nil
}

// Pins are the bundles a box applies, in order (ADR 26-10-24-953 §1): a
// sandbox's are its sandbox bundle and then its harness's, a pool's its pool
// bundle. Nothing in them says which is which; order is the composition and
// tie-break order of what the box provisions. The box's maker writes them
// with every placement, from its own record of the box's pins, which is also
// all the box may fetch (ADR 26-10-16-423 §13); a pin moved while the box
// runs reaches it at its next start.
type Pins []Pin

// Validate refuses pins naming no bundle, a bundle the store could not
// fetch, or one bundle twice.
func (p Pins) Validate() error {
	if len(p) == 0 {
		return errors.New("the pins name no bundle")
	}
	seen := map[string]bool{}
	for i, pin := range p {
		if err := pin.validate(fmt.Sprintf("pin %d's", i)); err != nil {
			return err
		}
		if seen[pin.Name] {
			return fmt.Errorf("bundle %q is pinned twice", pin.Name)
		}
		seen[pin.Name] = true
	}
	return nil
}

// ParsePins reads PinsFile.
func ParsePins(data []byte) (Pins, error) {
	var pins Pins
	if err := json.Unmarshal(data, &pins); err != nil {
		return nil, fmt.Errorf("decode %s: %w", PinsFile, err)
	}
	if err := pins.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", PinsFile, err)
	}
	return pins, nil
}

// ErrNotAgentLayer is a layer that is not the agent's alone.
var ErrNotAgentLayer = errors.New("not the agent layer")

// ExtractAgent reads a layer, its tar gzipped or not, and writes the agent it
// holds to path, failing with ErrNotAgentLayer unless it holds the agent and
// nothing else: AgentFile, a regular file, and the directories above it. The
// agent bundle is that one layer (sandbox-agent/Dockerfile's agent stage, ADR
// 26-10-24-953 §2), which whoever makes a box knows only by digest; this is
// what makes a wrong one fail by name rather than boot a box from whatever
// file it holds. Nothing is at path unless the whole layer was the agent's.
func ExtractAgent(layer io.Reader, path string) error {
	buffered := bufio.NewReader(layer)
	var tarStream io.Reader = buffered
	if magic, _ := buffered.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		zr, err := gzip.NewReader(buffered)
		if err != nil {
			return fmt.Errorf("read layer: %w", err)
		}
		defer zr.Close()
		tarStream = zr
	}
	tmp := path + ".tmp"
	defer func() { _ = os.Remove(tmp) }()
	tr := tar.NewReader(tarStream)
	found := false
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read layer: %w", err)
		}
		name := strings.TrimPrefix(pathpkg.Clean("/"+header.Name), "/")
		switch {
		case header.Typeflag == tar.TypeDir && (name == "features" || name == "features/core"):
		case header.Typeflag == tar.TypeReg && name == AgentFile && !found:
			found = true
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755) //nolint:gosec // G302: the agent is an executable.
			if err != nil {
				return err
			}
			// The entry's own size: a layer that holds less fails, not a
			// short agent.
			_, copyErr := io.CopyN(out, tr, header.Size)
			if err := errors.Join(copyErr, out.Close()); err != nil {
				return fmt.Errorf("extract the agent: %w", err)
			}
		default:
			return fmt.Errorf("%w: it holds %s", ErrNotAgentLayer, header.Name)
		}
	}
	if !found {
		return fmt.Errorf("%w: it holds no %s", ErrNotAgentLayer, AgentFile)
	}
	return os.Rename(tmp, path)
}

// Agent is an agent layer to place: its digest, and the agent binary it
// holds.
type Agent struct {
	Digest string
	// Open opens the agent binary; Size is its length.
	Open func() (io.ReadCloser, error)
	Size int64
}

// Archive writes what a pool places in a container before it starts: the
// agent layer into the store under its digest, unless placed is set because
// the box already holds it; CurrentLink pointing at it; the units and their
// enablement; and pins. It is extracted at the container's root. Everything
// but the layer is written on every placement, so a box always boots with the
// units of the pool that starts it and the pins it last recorded.
func Archive(w io.Writer, agent Agent, placed bool, pins Pins) error {
	if err := pins.Validate(); err != nil {
		return err
	}
	if !imagecache.IsDigest(agent.Digest) {
		return fmt.Errorf("agent layer digest %q is not a sha256 digest", agent.Digest)
	}
	tw := tar.NewWriter(w)
	a := archive{tw: tw}
	for _, dir := range []string{"/opt", "/opt/discobox", StoreDir, LayersDir} {
		a.dir(dir)
	}
	if !placed {
		layer := LayerDir(agent.Digest)
		a.dir(layer)
		a.dir(layer + "/features")
		a.dir(layer + "/features/core")
		if a.err == nil {
			a.err = a.copy(layer+"/"+AgentFile, 0o755, agent)
		}
		// Last, so an extraction cut short leaves no marker (completeSuffix).
		a.file(LayerComplete(agent.Digest), 0o644, nil)
	}
	a.link(CurrentLink, "bundles/layers/"+Hex(agent.Digest))
	data, err := json.MarshalIndent(pins, "", "  ")
	if err != nil {
		return err
	}
	a.file(PinsFile, 0o644, data)
	for _, unit := range Units {
		content, err := unitFiles.ReadFile("units/" + unit.Name)
		if err != nil {
			return err
		}
		a.file(unit.Path, 0o644, content)
		if unit.target != "" {
			a.dir(unit.linkDir())
			a.link(unit.linkDir()+"/"+unit.Name, unit.Path)
		}
	}
	if a.err != nil {
		return a.err
	}
	return tw.Close()
}

// archive writes a placement's entries, holding the first error.
type archive struct {
	tw  *tar.Writer
	err error
}

// modTime is every entry's: a placement's bytes are its inputs', so the
// same placement is the same archive.
var modTime = time.Unix(0, 0)

func (a *archive) header(header *tar.Header) {
	if a.err != nil {
		return
	}
	header.Name = strings.TrimPrefix(header.Name, "/")
	header.ModTime = modTime
	header.Format = tar.FormatPAX
	a.err = a.tw.WriteHeader(header)
}

func (a *archive) dir(name string) {
	a.header(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755})
}

func (a *archive) link(name, target string) {
	a.header(&tar.Header{Typeflag: tar.TypeSymlink, Name: name, Linkname: target, Mode: 0o777})
}

func (a *archive) file(name string, mode int64, data []byte) {
	a.header(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: int64(len(data))})
	if a.err == nil {
		_, a.err = a.tw.Write(data)
	}
}

func (a *archive) copy(name string, mode int64, agent Agent) error {
	body, err := agent.Open()
	if err != nil {
		return err
	}
	defer body.Close()
	a.header(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: mode, Size: agent.Size})
	if a.err != nil {
		return a.err
	}
	if _, err := io.CopyN(a.tw, body, agent.Size); err != nil {
		return fmt.Errorf("copy the agent into the placement: %w", err)
	}
	return nil
}
