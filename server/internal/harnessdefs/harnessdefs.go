// Package harnessdefs owns the seed data for the included harness images.
//
// There is no user-facing "definition" concept: a harness config is the single
// harness thing. This package only supplies what the server needs to seed the
// three built-in harness configs, and the env-override mapping dev builds use to
// point those configs at freshly tagged images.
package harnessdefs

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/obot-platform/discobox/harness/registry"
)

// Seed describes a built-in harness config the server seeds into a project. It
// is owned by the harness package under github.com/obot-platform/discobox/harness
// and surfaced through the harness registry, so harness-specific defaults live
// with the harness they describe.
type Seed struct {
	// Slug is the stable selector (e.g. codex) and the built-in's identity.
	Slug string
	Name string
	// Image is the harness image, already resolved against any env override.
	Image string
	// Digest, when set, is the image's already-known identity and is used in
	// place of inspecting the image. The reserved `shell` built-in sets it:
	// its image is the sandbox agent, which deliberately declares no harness
	// label to read, and whose identity the server is configured with anyway.
	Digest string
}

// Seeds returns the built-in harness configs to seed, with each image replaced
// by imageOverrides[slug] when present. Dev builds inject freshly tagged images
// this way (see ImageEnvVar); an empty map yields the baked-in images.
// ShellSlug is the reserved slug of the built-in that ends the harness
// resolution chain (ADR 0025). It is not in the registry: the registry holds
// harness products, each built on top of the sandbox agent image, while this
// one *is* the sandbox agent image with no product on it. Its name is reserved
// so nothing else can claim the end of the chain.
const ShellSlug = "shell"

// ShellName is the display name of the ShellSlug built-in.
const ShellName = "Shell"

func Seeds(imageOverrides map[string]string) []Seed {
	definitions := registry.Definitions()
	out := make([]Seed, 0, len(definitions))
	for _, definition := range definitions {
		image := definition.Image
		if override := strings.TrimSpace(imageOverrides[definition.ID]); override != "" {
			image = override
		}
		out = append(out, Seed{Slug: definition.ID, Name: definition.Name, Image: image})
	}
	return out
}

// ImageEnvVar returns the environment variable that overrides a built-in
// harness's image. The dev image watcher writes the same key; keep the two
// mappings in sync.
func ImageEnvVar(slug string) string {
	return "DISCOBOX_HARNESS_" + strings.ToUpper(strings.ReplaceAll(slug, "-", "_")) + "_IMAGE"
}

// ImageOverridesFromEnv reads per-harness image overrides from the environment
// via getenv, keyed by built-in slug.
func ImageOverridesFromEnv(getenv func(string) string) map[string]string {
	overrides := map[string]string{}
	for _, definition := range registry.Definitions() {
		if value := strings.TrimSpace(getenv(ImageEnvVar(definition.ID))); value != "" {
			overrides[definition.ID] = value
		}
	}
	return overrides
}

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidateSlug reports whether slug is a valid URL-safe harness-config slug.
func ValidateSlug(slug string) error {
	if !slugPattern.MatchString(slug) {
		return fmt.Errorf("slug %q must be URL-safe: lowercase letters, digits, and hyphens, starting with a letter or digit", slug)
	}
	return nil
}

// Slugify derives a URL-safe slug from a display name.
func Slugify(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if b.Len() > 0 && !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
