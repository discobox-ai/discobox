// Package harnessdefs owns the built-in harness-config definitions and the
// resolution between a sparse stored HarnessConfig and the definition it extends.
//
// It deliberately depends only on the harness registry and the model package so
// that both the harnessconfigs resource service and the higher-level services
// package can resolve configs without an import cycle.
package harnessdefs

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
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

// Resolve returns a copy of the harness config with unset (nil) command and file
// fields filled in from the built-in definition it extends (DefinitionID). This
// is the runtime view: a definition upgrade propagates to every field the user
// did not explicitly override. Configs without a known definition are returned
// unchanged.
func Resolve(config *model.HarnessConfig) *model.HarnessConfig {
	if config == nil {
		return nil
	}
	resolved := *config
	if strings.TrimSpace(config.DefinitionID) == "" {
		return &resolved
	}
	definition, ok := DefinitionByID(config.DefinitionID)
	if !ok {
		return &resolved
	}
	if resolved.Name == "" {
		resolved.Name = definition.Name
	}
	if resolved.InstallCommand == nil {
		resolved.InstallCommand = definition.InstallCommand
	}
	if resolved.RunCommand == nil {
		resolved.RunCommand = definition.RunCommand
	}
	if resolved.RelaunchCommand == nil {
		resolved.RelaunchCommand = definition.RelaunchCommand
	}
	if resolved.Files == nil {
		resolved.Files = definition.Files
	}
	if resolved.Secrets == nil {
		resolved.Secrets = definition.Secrets
	}
	return &resolved
}

// Sparsify is the inverse of Resolve: for a config that extends a definition, it
// drops any command/file field equal to the definition value back to nil
// (inherit). This keeps the stored overlay canonical — a field is an override
// only if it differs from the definition — so a client that reads the resolved
// config and writes the whole object back does not accidentally pin every field
// and freeze it against future definition upgrades. It is a no-op for custom
// configs (no definition to compare against). Name is left concrete.
func Sparsify(config *model.HarnessConfig) {
	if config == nil || strings.TrimSpace(config.DefinitionID) == "" {
		return
	}
	definition, ok := DefinitionByID(config.DefinitionID)
	if !ok {
		return
	}
	if slices.Equal(config.InstallCommand, definition.InstallCommand) {
		config.InstallCommand = nil
	}
	if slices.Equal(config.RunCommand, definition.RunCommand) {
		config.RunCommand = nil
	}
	if slices.Equal(config.RelaunchCommand, definition.RelaunchCommand) {
		config.RelaunchCommand = nil
	}
	if reflect.DeepEqual(config.Files, definition.Files) {
		config.Files = nil
	}
	if reflect.DeepEqual(config.Secrets, definition.Secrets) {
		config.Secrets = nil
	}
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
	return model.HarnessDefinition{
		ID:              definition.ID,
		Name:            definition.Name,
		Description:     definition.Description,
		InstallCommand:  definition.InstallCommand,
		RunCommand:      definition.RunCommand,
		RelaunchCommand: definition.RelaunchCommand,
		Files:           filesFromHarness(definition.Files),
		Secrets:         secretsFromHarness(definition.Secrets),
	}
}

func secretsFromHarness(secrets []harness.Secret) []model.HarnessConfigSecret {
	if len(secrets) == 0 {
		return nil
	}
	out := make([]model.HarnessConfigSecret, 0, len(secrets))
	for _, secret := range secrets {
		out = append(out, model.HarnessConfigSecret{
			Name:       secret.Name,
			Required:   secret.Required,
			OneOfGroup: secret.OneOfGroup,
		})
	}
	return out
}

func filesFromHarness(files []harness.File) []model.HarnessConfigFile {
	if len(files) == 0 {
		return nil
	}
	out := make([]model.HarnessConfigFile, 0, len(files))
	for _, file := range files {
		out = append(out, model.HarnessConfigFile{
			Path:       file.Path,
			Content:    file.Content,
			CreateOnly: file.CreateOnly,
		})
	}
	return out
}
