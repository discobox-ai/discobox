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

	"github.com/discobox-ai/discobox/imagecache"
	"github.com/discobox-ai/discobox/serverstage"
)

// Env names the local release manifest shared by the CLI and server.
const Env = "DISCOBOX_RELEASE_MANIFEST"

// Format is the manifest format this package writes.
//
// Format 1 predates ADR 0148. It names the guest and libkrun's kernel as two
// images, where format 2 names the one libkrun image that packages the guest's
// root disk with its kernel and runtime. Format 1 is still read, because a
// published release's manifest is durable and a development build is pointed
// at one to run its images (DESIGN.md). It has no libkrun image to give, so
// under one a libkrun pool boots the image its own build pins. Written back it
// stays format 1, without the kernel it no longer names, and reads again.
const Format = 2

// Manifest describes release binaries and runtime images. Binary descriptors
// include platform, URLs, sizes, and verified digests. Development binaries may
// adopt this metadata while keeping their own build identity.
type Manifest struct {
	Format   int                    `json:"format"`
	Version  string                 `json:"version"`
	Revision string                 `json:"revision,omitempty"`
	Images   Images                 `json:"images"`
	Servers  []serverstage.Manifest `json:"servers,omitempty"`
	Clients  []serverstage.Manifest `json:"clients,omitempty"`
}

// Images names each image by its runtime role. The staging list is derived
// from these roles, never maintained separately from the harness references.
type Images struct {
	PoolAgent    string `json:"poolAgent"`
	SandboxAgent string `json:"sandboxAgent"`
	// VM is the guest image vz boots.
	VM string `json:"vm"`
	// Libkrun is the image a libkrun pool boots: the guest's root disk with
	// the kernel and runtime built for it. Empty only when read from format 1.
	Libkrun   string            `json:"libkrun,omitempty"`
	Harnesses map[string]string `json:"harnesses"`
}

// formatV1 is format 1 as it was written: images named a separate kernel and
// no libkrun image.
type formatV1 struct {
	Format   int    `json:"format"`
	Version  string `json:"version"`
	Revision string `json:"revision,omitempty"`
	Images   struct {
		PoolAgent    string            `json:"poolAgent"`
		SandboxAgent string            `json:"sandboxAgent"`
		VM           string            `json:"vm"`
		Kernel       string            `json:"kernel"`
		Harnesses    map[string]string `json:"harnesses"`
	} `json:"images"`
	Servers []serverstage.Manifest `json:"servers,omitempty"`
	Clients []serverstage.Manifest `json:"clients,omitempty"`
}

func Read(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	return Parse(data)
}

func Parse(data []byte) (Manifest, error) {
	var header struct {
		Format int `json:"format"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return Manifest{}, fmt.Errorf("release manifest: %w", err)
	}
	if header.Format == 1 {
		var old formatV1
		if err := decodeStrict(data, &old); err != nil {
			return Manifest{}, err
		}
		// Optional: a format-1 manifest this package read and wrote back
		// carries none, and has to read again.
		if old.Images.Kernel != "" {
			if err := validateImage(old.Images.Kernel); err != nil {
				return Manifest{}, fmt.Errorf("release manifest kernel: %w", err)
			}
		}
		m := Manifest{
			Format:   old.Format,
			Version:  old.Version,
			Revision: old.Revision,
			Images: Images{
				PoolAgent:    old.Images.PoolAgent,
				SandboxAgent: old.Images.SandboxAgent,
				VM:           old.Images.VM,
				Harnesses:    old.Images.Harnesses,
			},
			Servers: old.Servers,
			Clients: old.Clients,
		}
		return m, m.Validate()
	}
	var m Manifest
	if err := decodeStrict(data, &m); err != nil {
		return m, err
	}
	return m, m.Validate()
}

// decodeStrict decodes one JSON document with no field the target lacks.
func decodeStrict(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return fmt.Errorf("release manifest: %w", err)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("release manifest has trailing data")
	}
	return nil
}

func (m Manifest) Validate() error {
	if m.Format != 1 && m.Format != Format {
		return fmt.Errorf("unsupported release manifest format %d", m.Format)
	}
	if strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("release manifest has no version")
	}
	roles := map[string]string{"poolAgent": m.Images.PoolAgent, "sandboxAgent": m.Images.SandboxAgent, "vm": m.Images.VM}
	if m.Format != 1 {
		roles["libkrun"] = m.Images.Libkrun
	} else if m.Images.Libkrun != "" {
		return fmt.Errorf("release manifest format 1 has no libkrun image")
	}
	for role, ref := range roles {
		if err := validateImage(ref); err != nil {
			return fmt.Errorf("release manifest %s: %w", role, err)
		}
	}
	if len(m.Images.Harnesses) == 0 {
		return fmt.Errorf("release manifest has no harness images")
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

// References includes the boot image of the host's VM backend: vz's guest on
// macOS, the libkrun image on amd64 Linux (ADR 0148 §6). Windows supplies its
// own guest, and Linux elsewhere has no VM backend to boot one.
func (i Images) References(hostOS, hostArch string) []string {
	var refs []string
	switch hostOS {
	case "darwin":
		refs = append(refs, i.VM)
	case "linux":
		if hostArch == "amd64" && i.Libkrun != "" {
			refs = append(refs, i.Libkrun)
		}
	}
	refs = append(refs, i.PoolAgent, i.SandboxAgent)
	slugs := make([]string, 0, len(i.Harnesses))
	for slug := range i.Harnesses {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	for _, slug := range slugs {
		refs = append(refs, i.Harnesses[slug])
	}
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
