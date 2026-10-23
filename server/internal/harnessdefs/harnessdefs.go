// Package harnessdefs owns the seed data for the included harnesses.
//
// There is no user-facing "definition" concept: a harness config is the single
// harness thing. This package only supplies what the server needs to seed the
// built-in harness configs, and the env-override mapping dev builds use to
// point those configs at freshly built bundles.
package harnessdefs

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/harness/registry"
)

// Seed describes a built-in harness config the server seeds into a project. It
// is owned by the harness package under github.com/discobox-ai/discobox/harness
// and surfaced through the harness registry, so harness-specific defaults live
// with the harness they describe.
type Seed struct {
	// Slug is the stable selector (e.g. codex) and the built-in's identity.
	Slug string
	Name string
	// Bundle is the harness bundle its manifest is read from and every box on
	// it applies, already resolved against any override.
	Bundle string
}

// References is what the built-in harnesses resolve against: the platform
// bundle every harness's manifest is composed on (ADR 26-10-16-423 §2), and
// per built-in slug, the bundle replacing its own. Dev builds set them from
// the environment (OverridesFromEnv), a release from its manifest.
type References struct {
	PlatformBundle string
	Bundles        map[string]string
}

// ShellSlug is the slug of the `shell` built-in. It is an ordinary registry
// harness — `harness/shell`, a bundle like every other (ADR 0043) — and not a
// fallback: a sandbox names its harness or the project does (ADR 0048). The
// one path that still ends at it is the upgrade of a legacy sandbox with no
// harness config, which adopts this one (ADR 0032 §4; see
// sandboxes.Service.fallbackHarnessConfig). The name is reserved so that
// nothing else can claim that role.
const ShellSlug = harness.ShellSlug

// Seeds returns the built-in harness configs to seed, with each bundle
// replaced by refs' entry for its slug when present. Dev builds inject freshly
// built ones this way (see BundleEnvVar); empty references yield the baked-in
// ones. With exact set, only the slugs refs names bundles for are seeded,
// including harnesses absent from this binary's registry: a release's manifest
// names every harness it ships, and their metadata comes from their bundles.
func Seeds(refs References, exact bool) []Seed {
	definitions := registry.Definitions()
	if exact {
		names := map[string]string{}
		for _, definition := range definitions {
			names[definition.ID] = definition.Name
		}
		slugs := make([]string, 0, len(refs.Bundles))
		for slug := range refs.Bundles {
			slugs = append(slugs, slug)
		}
		sort.Strings(slugs)
		out := make([]Seed, 0, len(slugs))
		for _, slug := range slugs {
			name := names[slug]
			if name == "" {
				name = slug
			}
			out = append(out, Seed{Slug: slug, Name: name, Bundle: refs.Bundles[slug]})
		}
		return out
	}
	out := make([]Seed, 0, len(definitions))
	for _, definition := range definitions {
		bundle := definition.Bundle
		if override := strings.TrimSpace(refs.Bundles[definition.ID]); override != "" {
			bundle = override
		}
		out = append(out, Seed{Slug: definition.ID, Name: definition.Name, Bundle: bundle})
	}
	return out
}

// BundleEnvVar returns the environment variable that overrides a built-in
// harness's bundle. The dev image watcher writes the same keys; keep the
// mappings in sync.
func BundleEnvVar(slug string) string {
	return "DISCOBOX_HARNESS_" + envSlug(slug) + "_BUNDLE"
}

func envSlug(slug string) string {
	return strings.ToUpper(strings.ReplaceAll(slug, "-", "_"))
}

// OverridesFromEnv reads per-harness bundle overrides from the environment
// via getenv, keyed by built-in slug.
func OverridesFromEnv(getenv func(string) string) map[string]string {
	bundles := map[string]string{}
	for _, definition := range registry.Definitions() {
		if value := strings.TrimSpace(getenv(BundleEnvVar(definition.ID))); value != "" {
			bundles[definition.ID] = value
		}
	}
	return bundles
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
