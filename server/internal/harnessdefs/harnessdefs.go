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
	for _, definition := range registry.Definitions() {
		if definition.ID == definitionID {
			converted := fromHarness(definition)
			return &converted, true
		}
	}
	return nil, false
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
