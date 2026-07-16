// Package harnessdefs owns built-in shortcuts for included harness images.
package harnessdefs

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/obot-platform/discobox/harness"
	"github.com/obot-platform/discobox/harness/registry"

	"github.com/obot-platform/discobox/server/internal/model"
)

// Definitions returns the built-in harness-config templates. Each is owned by its
// harness package under github.com/obot-platform/discobox/harness and surfaced
// through the harness registry so all harness-specific defaults live with the
// harness they describe.
func Definitions() []model.HarnessDefinition {
	harnessDefinitions := registry.Definitions()
	out := make([]model.HarnessDefinition, 0, len(harnessDefinitions))
	for _, definition := range harnessDefinitions {
		out = append(out, fromHarness(definition))
	}
	return out
}

// DefinitionByID returns the built-in definition with the given ID.
func DefinitionByID(definitionID string) (*model.HarnessDefinition, bool) {
	return DefinitionByIDWithImages(definitionID, nil)
}

// DefinitionsWithImages returns the built-in definitions with each image
// replaced by imageOverrides[id] when present. Dev builds inject freshly tagged
// images this way (see ImageEnvVar); an empty map yields the baked-in images.
func DefinitionsWithImages(imageOverrides map[string]string) []model.HarnessDefinition {
	harnessDefinitions := registry.Definitions()
	out := make([]model.HarnessDefinition, 0, len(harnessDefinitions))
	for _, definition := range harnessDefinitions {
		out = append(out, fromHarnessWithImage(definition, imageOverrides[definition.ID]))
	}
	return out
}

// DefinitionByIDWithImages returns the built-in definition with the given ID,
// applying an image override when imageOverrides holds one for that ID.
func DefinitionByIDWithImages(definitionID string, imageOverrides map[string]string) (*model.HarnessDefinition, bool) {
	for _, definition := range registry.Definitions() {
		if definition.ID == definitionID {
			converted := fromHarnessWithImage(definition, imageOverrides[definitionID])
			return &converted, true
		}
	}
	return nil, false
}

// ImageEnvVar returns the environment variable that overrides a harness
// definition's image. The dev image watcher writes the same key; keep the two
// mappings in sync.
func ImageEnvVar(definitionID string) string {
	return "DISCOBOX_HARNESS_" + strings.ToUpper(strings.ReplaceAll(definitionID, "-", "_")) + "_IMAGE"
}

// ImageOverridesFromEnv reads per-definition image overrides from the
// environment via getenv, keyed by definition ID.
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

func fromHarnessWithImage(definition harness.Definition, imageOverride string) model.HarnessDefinition {
	if strings.TrimSpace(imageOverride) != "" {
		definition.Image = imageOverride
	}
	return fromHarness(definition)
}

func fromHarness(definition harness.Definition) model.HarnessDefinition {
	out := model.HarnessDefinition{
		ID: definition.ID, Name: definition.Name, Description: definition.Description,
		Image: definition.Image, Configure: configureFromHarness(definition.Configure),
	}
	if out.Configure != nil {
		out.Configure.Image = definition.Image
	}
	return out
}

func configureFromHarness(configure *harness.Configure) *model.ConfigureSandbox {
	if configure == nil {
		return nil
	}
	return &model.ConfigureSandbox{
		Image:        configure.Image,
		Env:          configure.Env,
		CPUVCPUs:     configure.CPUVCPUs,
		MemoryBytes:  configure.MemoryBytes,
		StorageBytes: configure.StorageBytes,
	}
}
