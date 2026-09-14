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

// Manifest can be used with locally built binaries. Servers, when present,
// describe downloadable binaries; Images always describes what the server runs.
type Manifest struct {
	Format  int                    `json:"format"`
	Version string                 `json:"version"`
	Images  Images                 `json:"images"`
	Servers []serverstage.Manifest `json:"servers,omitempty"`
}

// Images names each image by its runtime role. The staging list is derived
// from these roles, never maintained separately from the harness references.
type Images struct {
	PoolAgent    string            `json:"poolAgent"`
	SandboxAgent string            `json:"sandboxAgent"`
	VM           string            `json:"vm"`
	Kernel       string            `json:"kernel"`
	Harnesses    map[string]string `json:"harnesses"`
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
	for role, ref := range map[string]string{"poolAgent": m.Images.PoolAgent, "sandboxAgent": m.Images.SandboxAgent, "vm": m.Images.VM, "kernel": m.Images.Kernel} {
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
	seen := map[string]bool{}
	for _, server := range m.Servers {
		if err := server.Validate(); err != nil {
			return err
		}
		if server.Version != m.Version {
			return fmt.Errorf("server version %s differs from release %s", server.Version, m.Version)
		}
		if seen[server.Platform()] {
			return fmt.Errorf("duplicate server platform %s", server.Platform())
		}
		seen[server.Platform()] = true
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
