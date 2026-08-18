package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/cli/internal/tui"
	"github.com/obot-platform/discobox/harness"
)

// The launcher's harnesses screen, on this side of the seam. What the window
// calls a harness is the control plane's harness config, and everything here is
// the same API and the same flows the `box harnesses` subcommands run, so what
// the screen does is reproducible from a shell.

// Harnesses is the project's harness configs, oldest first, which is the order
// they were registered in — the built-ins the server ships lead, and anything
// registered by hand follows.
func (d *apiDataSource) Harnesses(ctx context.Context) ([]tui.Harness, error) {
	configs, err := d.app.listHarnessConfigs(ctx, d.client, d.projectID)
	if err != nil {
		return nil, err
	}
	// The default is a property of the project rather than of the harness, so
	// it takes a request of its own.
	defaultID, err := d.app.defaultHarnessConfigID(ctx, d.client, d.projectID)
	if err != nil {
		return nil, err
	}
	sorted := append([]apimodel.HarnessConfig(nil), configs...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].CreatedAt.Before(sorted[j].CreatedAt) })

	harnesses := make([]tui.Harness, 0, len(sorted))
	for _, cfg := range sorted {
		harnesses = append(harnesses, toTUIHarness(cfg, defaultID))
	}
	return harnesses, nil
}

func toTUIHarness(cfg apimodel.HarnessConfig, defaultID string) tui.Harness {
	harness := tui.Harness{
		ID:      cfg.ID,
		Name:    strings.TrimSpace(cfg.Name),
		Slug:    strings.TrimSpace(cfg.Slug),
		State:   harnessState(cfg),
		Default: cfg.ID == defaultID,
		BuiltIn: cfg.BuiltIn,
		Shell:   strings.TrimSpace(cfg.Slug) == harness.ShellSlug,
		// What the server decides both enable and disable on: an image that
		// declares no configure command has neither to offer.
		Configurable: len(cfg.ConfigCommand.Or(nil)) > 0,
		Error:        strings.TrimSpace(cfg.ConfigureError.Or("")),
		Image:        cfg.Image.Or(""),
		Digest:       cfg.ImageDigest.Or(""),
		Run:          cfg.RunCommand,
		Relaunch:     cfg.RelaunchCommand.Or(nil),
		Updated:      cfg.UpdatedAt,
	}
	for _, secret := range cfg.Secrets.Or(nil) {
		harness.Secrets = append(harness.Secrets, tui.HarnessSecret{
			Name:     secret.Name,
			Required: secret.Required.Or(false),
			OneOf:    secret.OneOfGroup.Or(""),
			Declared: true,
		})
	}
	// The files its configure flow wrote lead, because they overlay the
	// image-declared file of the same path and are the ones worth editing.
	for _, file := range cfg.ConfiguredFiles.Or(nil) {
		harness.Files = append(harness.Files, toTUIHarnessFile(file, true))
	}
	for _, file := range cfg.Files.Or(nil) {
		harness.Files = append(harness.Files, toTUIHarnessFile(file, false))
	}
	return harness
}

func toTUIHarnessFile(file apimodel.HarnessConfigFile, configured bool) tui.HarnessFile {
	return tui.HarnessFile{
		Path:       file.Path,
		Content:    file.Content,
		Configured: configured,
		CreateOnly: file.CreateOnly.Or(false),
		Template:   file.Template.Or(false),
	}
}

// harnessState narrows the config to the three states the screen draws. A
// configure error is only worth reporting while the harness is not configured:
// a reconfigure that failed after one that worked leaves the working
// configuration in place.
func harnessState(cfg apimodel.HarnessConfig) tui.HarnessState {
	switch {
	case cfg.Configured:
		return tui.HarnessEnabled
	case strings.TrimSpace(cfg.ConfigureError.Or("")) != "":
		return tui.HarnessFailed
	default:
		return tui.HarnessDisabled
	}
}

// HarnessSecrets is what actually answers each environment variable the harness
// needs: the image's declarations resolved against the project's secret
// bindings, plus the bindings the image never declared, which are the ones
// somebody bound by hand.
func (d *apiDataSource) HarnessSecrets(ctx context.Context, harnessID string) ([]tui.HarnessSecret, error) {
	res, err := d.client.GetHarnessConfig(ctx, apiclientgen.GetHarnessConfigParams{ProjectId: d.projectID, HarnessConfigId: harnessID})
	if err != nil {
		return nil, err
	}
	cfg, err := expectResponse[apimodel.HarnessConfig](res)
	if err != nil {
		return nil, err
	}
	bindings, secretsByID, err := d.app.harnessSecretAssignments(ctx, d.client, d.projectID, harnessID)
	if err != nil {
		return nil, err
	}
	return harnessSecrets(*cfg, bindings, secretsByID), nil
}

func harnessSecrets(cfg apimodel.HarnessConfig, bindings []apimodel.HarnessConfigSecretBinding, secretsByID map[string]apimodel.Secret) []tui.HarnessSecret {
	boundByEnv := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		boundByEnv[binding.EnvName] = binding.SecretId
	}

	out := make([]tui.HarnessSecret, 0, len(bindings))
	declared := map[string]bool{}
	for _, secret := range cfg.Secrets.Or(nil) {
		declared[secret.Name] = true
		out = append(out, resolveHarnessSecret(tui.HarnessSecret{
			Name:     secret.Name,
			Required: secret.Required.Or(false),
			OneOf:    secret.OneOfGroup.Or(""),
			Declared: true,
		}, boundByEnv[secret.Name], secretsByID))
	}
	// A binding for a variable the image never declared still answers one, so
	// it is listed too rather than silently dropped.
	for _, binding := range bindings {
		if declared[binding.EnvName] {
			continue
		}
		out = append(out, resolveHarnessSecret(tui.HarnessSecret{Name: binding.EnvName}, binding.SecretId, secretsByID))
	}
	return out
}

func resolveHarnessSecret(out tui.HarnessSecret, secretID string, secretsByID map[string]apimodel.Secret) tui.HarnessSecret {
	if secretID == "" {
		return out
	}
	out.SecretID = secretID
	// The secret itself is only describable when the project can see it, which
	// is the usual case but not the guaranteed one.
	if secret, ok := secretsByID[secretID]; ok {
		out.SecretType = string(secret.Type)
		out.SecretName = strings.TrimSpace(secret.Name)
		out.Anonymous = secret.Anonymous.Or(false)
	}
	return out
}

// DoHarness runs one of the harness verbs against the API.
func (d *apiDataSource) DoHarness(ctx context.Context, verb tui.HarnessVerb, harnessID string) error {
	switch verb {
	case tui.HarnessSetDefault:
		return d.app.setDefaultHarnessConfig(ctx, d.client, d.projectID, harnessID)

	case tui.HarnessDisable:
		// The server refuses to deconfigure the project's default harness, so
		// the default is released first when the target is it. That is the
		// unset the user would otherwise have to do by hand before the verb
		// they actually asked for would be accepted.
		defaultID, err := d.app.defaultHarnessConfigID(ctx, d.client, d.projectID)
		if err != nil {
			return err
		}
		if defaultID == harnessID {
			res, err := d.client.UnsetDefaultHarnessConfig(ctx, apiclientgen.UnsetDefaultHarnessConfigParams{
				ProjectId: d.projectID, HarnessConfigId: harnessID,
			})
			if err != nil {
				return err
			}
			if _, err := expectResponse[apimodel.Project](res); err != nil {
				return err
			}
		}
		res, err := d.client.DeconfigureHarnessConfig(ctx, apiclientgen.DeconfigureHarnessConfigParams{
			ProjectId: d.projectID, HarnessConfigId: harnessID,
		})
		if err != nil {
			return err
		}
		_, err = expectResponse[apimodel.HarnessConfig](res)
		return err

	default:
		return fmt.Errorf("unknown harness action %q", verb)
	}
}

// ConfigureHarness runs the harness's own interactive configure flow on the
// real terminal the window has stepped aside from. The flow is the same one
// `disco box harnesses configure` runs, streams and all.
func (d *apiDataSource) ConfigureHarness(ctx context.Context, harnessID string, stdin io.Reader, stdout, stderr io.Writer) error {
	_, err := d.app.runHarnessConfigure(ctx, d.client, d.projectID, harnessID, stdin, stdout, stderr)
	return err
}

// EditHarnessFile opens one of the harness's files in the user's editor and
// saves what it wrote back. The config is re-read first, so the editor opens on
// what the file is now rather than on what the listing said it was.
func (d *apiDataSource) EditHarnessFile(ctx context.Context, harnessID, path string, stdin io.Reader, stdout, stderr io.Writer) (bool, error) {
	res, err := d.client.GetHarnessConfig(ctx, apiclientgen.GetHarnessConfigParams{ProjectId: d.projectID, HarnessConfigId: harnessID})
	if err != nil {
		return false, err
	}
	cfg, err := expectResponse[apimodel.HarnessConfig](res)
	if err != nil {
		return false, err
	}
	return d.app.editHarnessFile(ctx, d.client, d.projectID, cfg, path, stdin, stdout, stderr)
}
