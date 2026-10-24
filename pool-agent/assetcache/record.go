package assetcache

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/discobox-ai/discobox/pkg/imagecache"
	"github.com/discobox-ai/discobox/pkg/layout"
)

// RecordFileName is, per sandbox, what it is delivered (ADR 26-10-16-423
// §§7, 13, ADR 26-10-24-953 §§1–2): the agent its pool places in it and the
// bundles it applies, in order. It sits at the root of the sandbox's tree,
// beside the volumes rather than in one, as source-caches.json does: it is
// this pool's own record of what the sandbox may fetch, the sandbox never
// sees it, and every create rewrites it from the request, so nothing the box
// reports ever reaches it.
const RecordFileName = "delivery.json"

// legacyRecordFileName is the record a pool kept before the agent was a
// bundle of its own: the sandbox's bundles, each named by its kind. It is
// read when there is no RecordFileName, as the bundles it names, platform
// first, and no agent: the pool places nothing in such a sandbox, which keeps
// the agent placed in it then, until a create records what it is delivered
// now and removes it.
const legacyRecordFileName = "bundles.json"

// Pin is a bundle a sandbox is pinned to: the name the asset API addresses
// it by, and the manifest or index digest it is pinned to. Nothing says which
// of the sandbox's bundles it is.
type Pin struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// Delivery is what a sandbox is delivered: the agent bundle its pool places
// in it, and the bundles it applies, in order. The zero Delivery is none: a
// sandbox from before bundles.
type Delivery struct {
	Agent   Pin   `json:"agent"`
	Bundles []Pin `json:"bundles"`
}

// Delivered reports whether d delivers anything.
func (d Delivery) Delivered() bool { return d.Agent != (Pin{}) || len(d.Bundles) > 0 }

// PlacesAgent reports whether d names the agent its pool places: false for
// nothing delivered, and for a sandbox recorded before the agent was a bundle
// (legacyRecordFileName), whose container keeps the agent placed in it then.
func (d Delivery) PlacesAgent() bool { return d.Agent != (Pin{}) }

// pins is every bundle d reaches, its agent's included: what the pool keeps
// blobs for. What the sandbox may fetch is its bundles alone; its agent is
// placed in it (Cache.OpenAgentLayer).
func (d Delivery) pins() []Pin {
	if d.Agent == (Pin{}) {
		return d.Bundles
	}
	return append([]Pin{d.Agent}, d.Bundles...)
}

// Validate refuses a delivery with no agent or no bundles, or a pin that
// names no bundle or no digest. The zero Delivery is valid.
func (d Delivery) Validate() error {
	if !d.Delivered() {
		return nil
	}
	if d.Agent == (Pin{}) || len(d.Bundles) == 0 {
		return errors.New("a delivery names both the agent and the bundles, or neither")
	}
	for _, pin := range d.pins() {
		if err := pin.validate(); err != nil {
			return err
		}
	}
	return nil
}

// validName is a bundle name as one path segment: the last component of a
// repository, which is lowercase letters, digits and separators.
var validName = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

func (p Pin) validate() error {
	if !validName.MatchString(p.Name) {
		return fmt.Errorf("bundle name %q is not a repository name component", p.Name)
	}
	if !imagecache.IsDigest(p.Digest) {
		return fmt.Errorf("bundle %s: digest %q is not sha256:<64 hex>", p.Name, p.Digest)
	}
	return nil
}

// WriteRecord records d as what the sandbox whose tree is sandboxRoot is
// delivered, replacing what was recorded before. Nothing delivered is no
// file. A delivery Validate refuses is refused, and nothing is written.
func WriteRecord(sandboxRoot string, d Delivery) error {
	path := filepath.Join(sandboxRoot, RecordFileName)
	// What is recorded now replaces the record kept before it, which would
	// otherwise be read again once this one is removed.
	if err := os.Remove(filepath.Join(sandboxRoot, legacyRecordFileName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if !d.Delivered() {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := d.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(sandboxRoot, 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadRecord is what the sandbox whose tree is sandboxRoot is delivered.
// None is the zero Delivery, not an error.
func ReadRecord(sandboxRoot string) (Delivery, error) {
	data, err := os.ReadFile(filepath.Join(sandboxRoot, RecordFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return readLegacyRecord(sandboxRoot)
	}
	if err != nil {
		return Delivery{}, err
	}
	var d Delivery
	if err := json.Unmarshal(data, &d); err != nil {
		return Delivery{}, fmt.Errorf("decode %s: %w", RecordFileName, err)
	}
	return d, d.Validate()
}

// readLegacyRecord is what a sandbox recorded in legacyRecordFileName is
// delivered: the bundles it names, platform first, and no agent. None is the
// zero Delivery.
func readLegacyRecord(sandboxRoot string) (Delivery, error) {
	data, err := os.ReadFile(filepath.Join(sandboxRoot, legacyRecordFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return Delivery{}, nil
	}
	if err != nil {
		return Delivery{}, err
	}
	var legacy []struct {
		Kind   string `json:"kind"`
		Name   string `json:"name"`
		Digest string `json:"digest"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return Delivery{}, fmt.Errorf("decode %s: %w", legacyRecordFileName, err)
	}
	var d Delivery
	for _, kind := range []string{"platform", "harness"} {
		for _, pin := range legacy {
			if pin.Kind != kind {
				continue
			}
			p := Pin{Name: pin.Name, Digest: pin.Digest}
			if err := p.validate(); err != nil {
				return Delivery{}, fmt.Errorf("%s: %w", legacyRecordFileName, err)
			}
			d.Bundles = append(d.Bundles, p)
		}
	}
	return d, nil
}

// delivery is the record of the sandbox or snapshot build named sandboxID.
func (c *Cache) delivery(sandboxID string) (Delivery, error) {
	if layout.IsSnapshotID(sandboxID) {
		return sandboxDelivery(c.snapshots, sandboxID)
	}
	return sandboxDelivery(c.sandboxes, sandboxID)
}

// pins is the bundles the sandbox or snapshot build named sandboxID applies,
// which is all it may fetch.
func (c *Cache) pins(sandboxID string) ([]Pin, error) {
	d, err := c.delivery(sandboxID)
	return d.Bundles, err
}

// allPins is every pin recorded, by any sandbox or snapshot build.
func (c *Cache) allPins() ([]Pin, error) {
	sandboxes, err := allPins(c.sandboxes)
	snapshots, snapshotErr := allPins(c.snapshots)
	return append(sandboxes, snapshots...), errors.Join(err, snapshotErr)
}

// sandboxDelivery is the record of the sandbox named sandboxID under
// sandboxes, the pool's parent of every sandbox's tree. An ID that is not one
// path segment names no sandbox.
func sandboxDelivery(sandboxes, sandboxID string) (Delivery, error) {
	if sandboxes == "" || sandboxID == "" || sandboxID == "." || sandboxID == ".." || strings.ContainsAny(sandboxID, `/\`) {
		return Delivery{}, nil
	}
	return ReadRecord(filepath.Join(sandboxes, sandboxID))
}

// allPins is every pin recorded under sandboxes, by any sandbox.
func allPins(sandboxes string) ([]Pin, error) {
	if sandboxes == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(sandboxes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pins []Pin
	var failures []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		recorded, err := ReadRecord(filepath.Join(sandboxes, entry.Name()))
		if err != nil {
			failures = append(failures, fmt.Errorf("sandbox %s: %w", entry.Name(), err))
			continue
		}
		pins = append(pins, recorded.pins()...)
	}
	return pins, errors.Join(failures...)
}
