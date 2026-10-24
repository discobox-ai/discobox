// Package releasemanifest defines the runtime artifacts belonging to a release.
package releasemanifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/discobox-ai/discobox/pkg/imagecache"
	"github.com/discobox-ai/discobox/pkg/serverstage"
)

// Env names the local release manifest shared by the CLI and server.
const Env = "DISCOBOX_RELEASE_MANIFEST"

// Manifest describes release binaries and runtime images. Binary descriptors
// include platform, URLs, sizes, and verified digests. Development binaries may
// adopt this metadata while keeping their own build identity.
type Manifest struct {
	Format   int                    `json:"format"`
	Version  string                 `json:"version"`
	Revision string                 `json:"revision,omitempty"`
	Images   Images                 `json:"images"`
	Bundles  Bundles                `json:"bundles,omitzero"`
	Servers  []serverstage.Manifest `json:"servers,omitempty"`
	Clients  []serverstage.Manifest `json:"clients,omitempty"`
}

// Images names each image by its runtime role. The staging list is derived
// from these roles, never maintained separately from them.
type Images struct {
	PoolAgent string `json:"poolAgent"`
	// SandboxBase is the Docker driver's sandbox base image, every sandbox
	// container's (ADR 26-10-16-423 §10). A release cut before it names none.
	SandboxBase string `json:"sandboxBase,omitempty"`
	VM          string `json:"vm"`
	Kernel      string `json:"kernel"`
	// SandboxAgent and Harnesses are the runnable sandbox-agent and harness
	// images a release from before bundles ran its sandboxes on, the last of
	// them shipping both. A release from bundles on names neither: no harness
	// is an image (ADR 26-10-16-423 §10). They stay so the CLI still reads,
	// and stages the images of, an older server's manifest.
	SandboxAgent string            `json:"sandboxAgent,omitempty"`
	Harnesses    map[string]string `json:"harnesses,omitempty"`
}

// Bundles names each bundle a release publishes (ADR 26-10-16-423 §§2, 7): the
// platform bundle the server composes under every harness's manifest and
// delivers to every box, each built-in harness's bundle by slug, and the
// pool-agent bundle, the pool agent for a pool machine booted from a driver
// base (ADR 26-10-23-551 §2).
//
// A release from before bundles names none, and is still a valid manifest:
// the CLI reads the manifests of older servers. A server reading its own
// release's requires them (HasBundles).
//
// A release cut before the pool-agent bundle names no PoolAgent, and is still
// valid; a server on it has no bundle to boot a pool machine with.
type Bundles struct {
	Platform  string            `json:"platform"`
	Harnesses map[string]string `json:"harnesses"`
	PoolAgent string            `json:"poolAgent,omitempty"`
}

func Read(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	return Parse(data)
}

func Parse(data []byte) (Manifest, error) {
	var m Manifest
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, fmt.Errorf("release manifest: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return m, fmt.Errorf("release manifest has trailing data")
	}
	return m, m.Validate()
}

func (m Manifest) Validate() error {
	if m.Format != 1 {
		return fmt.Errorf("unsupported release manifest format %d", m.Format)
	}
	if strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("release manifest has no version")
	}
	for role, ref := range map[string]string{"poolAgent": m.Images.PoolAgent, "vm": m.Images.VM, "kernel": m.Images.Kernel} {
		if err := validateImage(ref); err != nil {
			return fmt.Errorf("release manifest %s: %w", role, err)
		}
	}
	for role, ref := range map[string]string{"sandboxBase": m.Images.SandboxBase, "sandboxAgent": m.Images.SandboxAgent} {
		if ref == "" {
			continue
		}
		if err := validateImage(ref); err != nil {
			return fmt.Errorf("release manifest %s: %w", role, err)
		}
	}
	// A release runs its harnesses as bundles, or, before bundles, as images.
	if !m.HasBundles() && len(m.Images.Harnesses) == 0 {
		return fmt.Errorf("release manifest names no harness bundles")
	}
	for slug, ref := range m.Images.Harnesses {
		if strings.TrimSpace(slug) == "" {
			return fmt.Errorf("release manifest has an unnamed harness")
		}
		if err := validateImage(ref); err != nil {
			return fmt.Errorf("release manifest harness %s: %w", slug, err)
		}
	}
	for role, binaries := range map[string][]serverstage.Manifest{"server": m.Servers, "client": m.Clients} {
		seen := map[string]bool{}
		for _, binary := range binaries {
			if err := binary.Validate(); err != nil {
				return fmt.Errorf("%s: %w", role, err)
			}
			if binary.Version != m.Version {
				return fmt.Errorf("%s version %s differs from release %s", role, binary.Version, m.Version)
			}
			if seen[binary.Platform()] {
				return fmt.Errorf("duplicate %s platform %s", role, binary.Platform())
			}
			seen[binary.Platform()] = true
		}
	}
	return m.validateBundles()
}

// HasBundles reports whether the manifest names its release's bundles, which
// every release from harness bundles on does.
func (m Manifest) HasBundles() bool {
	return m.Bundles.Platform != "" || len(m.Bundles.Harnesses) > 0
}

func (m Manifest) validateBundles() error {
	if !m.HasBundles() {
		return nil
	}
	if err := validateImage(m.Bundles.Platform); err != nil {
		return fmt.Errorf("release manifest platform bundle: %w", err)
	}
	if m.Bundles.PoolAgent != "" {
		if err := validateImage(m.Bundles.PoolAgent); err != nil {
			return fmt.Errorf("release manifest pool-agent bundle: %w", err)
		}
	}
	if len(m.Bundles.Harnesses) == 0 {
		return fmt.Errorf("release manifest names no harness bundles")
	}
	for slug, ref := range m.Bundles.Harnesses {
		if strings.TrimSpace(slug) == "" {
			return fmt.Errorf("release manifest has an unnamed harness bundle")
		}
		if err := validateImage(ref); err != nil {
			return fmt.Errorf("release manifest harness bundle %s: %w", slug, err)
		}
	}
	return nil
}

func validateImage(value string) error {
	ref, err := imagecache.ParseReference(value)
	if err != nil {
		return err
	}
	if value != strings.TrimSpace(value) || ref.Tag == "local" || ref.Tag == "latest" {
		return fmt.Errorf("image %q must name a released tag or digest", value)
	}
	return nil
}

// References includes boot artifacts needed by the selected host backend.
// Linux also stages libkrun's boot artifacts; Windows supplies its own guest.
//
// The bundles are not staged. What is staged is loaded into pool daemons,
// which never hold a bundle; the server reads a bundle from its registry and
// serves it to pools by blob (ADR 26-10-16-423 §13). An older release's
// sandbox-agent and harness images are staged, since its server runs them.
func (i Images) References(hostOS, hostArch string) []string {
	var refs []string
	switch hostOS {
	case "darwin":
		refs = append(refs, i.VM)
	case "linux":
		refs = append(refs, i.VM)
		if hostArch == "amd64" {
			refs = append(refs, i.Kernel)
		}
	}
	refs = append(refs, i.PoolAgent)
	if i.SandboxBase != "" {
		refs = append(refs, i.SandboxBase)
	}
	if i.SandboxAgent != "" {
		refs = append(refs, i.SandboxAgent)
	}
	refs = append(refs, bySlug(i.Harnesses)...)
	seen := map[string]bool{}
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if !seen[ref] {
			out = append(out, ref)
			seen[ref] = true
		}
	}
	return out
}

// bySlug returns refs' values in the order of their slugs.
func bySlug(refs map[string]string) []string {
	slugs := make([]string, 0, len(refs))
	for slug := range refs {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	out := make([]string, 0, len(slugs))
	for _, slug := range slugs {
		out = append(out, refs[slug])
	}
	return out
}
