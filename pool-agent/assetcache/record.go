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

// RecordFileName is, per sandbox, the bundles it is pinned to (ADR
// 26-10-16-423 §§7, 13). It sits at the root of the sandbox's tree, beside
// the volumes rather than in one, as source-caches.json does: it is this
// pool's own record of what the sandbox may fetch, the sandbox never sees it,
// and every create rewrites it from the request, so nothing the box reports
// ever reaches it.
const RecordFileName = "bundles.json"

// Pin is a bundle a sandbox is pinned to: which of its bundles it is, the
// name the asset API addresses it by, and the manifest or index digest it is
// pinned to.
type Pin struct {
	Kind   Kind   `json:"kind,omitempty"`
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// Kind is which of a sandbox's bundles a pin is.
type Kind string

const (
	// KindPlatform is the platform bundle, whose agent layer the pool places
	// in the box (ADR 26-10-16-423 §7).
	KindPlatform Kind = "platform"
	// KindHarness is the harness bundle.
	KindHarness Kind = "harness"
)

// Platform is the platform pin among pins, and false when there is none: a
// sandbox its pool delivers no platform to.
func Platform(pins []Pin) (Pin, bool) {
	for _, pin := range pins {
		if pin.Kind == KindPlatform {
			return pin, true
		}
	}
	return Pin{}, false
}

// validName is a bundle name as one path segment: the last component of a
// repository, which is lowercase letters, digits and separators.
var validName = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)

func (p Pin) validate() error {
	switch p.Kind {
	case KindPlatform, KindHarness:
	default:
		return fmt.Errorf("bundle %s: kind %q is neither %s nor %s", p.Name, p.Kind, KindPlatform, KindHarness)
	}
	if !validName.MatchString(p.Name) {
		return fmt.Errorf("bundle name %q is not a repository name component", p.Name)
	}
	if !imagecache.IsDigest(p.Digest) {
		return fmt.Errorf("bundle %s: digest %q is not sha256:<64 hex>", p.Name, p.Digest)
	}
	return nil
}

// WriteRecord records pins as the bundles of the sandbox whose tree is
// sandboxRoot, replacing what was recorded before. No pins is no file. A pin
// that names no bundle or no digest is refused, and nothing is written.
func WriteRecord(sandboxRoot string, pins []Pin) error {
	path := filepath.Join(sandboxRoot, RecordFileName)
	if len(pins) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	for _, pin := range pins {
		if err := pin.validate(); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(pins, "", "  ")
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

// ReadRecord is the bundles recorded for the sandbox whose tree is
// sandboxRoot. None is not an error.
func ReadRecord(sandboxRoot string) ([]Pin, error) {
	data, err := os.ReadFile(filepath.Join(sandboxRoot, RecordFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pins []Pin
	if err := json.Unmarshal(data, &pins); err != nil {
		return nil, fmt.Errorf("decode %s: %w", RecordFileName, err)
	}
	return pins, nil
}

// pins is the record of the sandbox or snapshot build named sandboxID.
func (c *Cache) pins(sandboxID string) ([]Pin, error) {
	if layout.IsSnapshotID(sandboxID) {
		return sandboxPins(c.snapshots, sandboxID)
	}
	return sandboxPins(c.sandboxes, sandboxID)
}

// allPins is every pin recorded, by any sandbox or snapshot build.
func (c *Cache) allPins() ([]Pin, error) {
	sandboxes, err := allPins(c.sandboxes)
	snapshots, snapshotErr := allPins(c.snapshots)
	return append(sandboxes, snapshots...), errors.Join(err, snapshotErr)
}

// sandboxPins is the record of the sandbox named sandboxID under sandboxes,
// the pool's parent of every sandbox's tree. An ID that is not one path
// segment names no sandbox.
func sandboxPins(sandboxes, sandboxID string) ([]Pin, error) {
	if sandboxes == "" || sandboxID == "" || sandboxID == "." || sandboxID == ".." || strings.ContainsAny(sandboxID, `/\`) {
		return nil, nil
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
		pins = append(pins, recorded...)
	}
	return pins, errors.Join(failures...)
}
