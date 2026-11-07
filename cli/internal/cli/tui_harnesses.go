package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/console"
	"github.com/discobox-ai/discobox/harness"
)

// The launcher's harnesses screen, on this side of the seam. What the window
// calls a harness is the control plane's harness config, and everything here is
// the same API and the same flows the `admin harness` subcommands run, so what
// the screen does is reproducible from a shell.

// Harnesses is one server's harness configs, oldest first, which is the order
// they were registered in — the built-ins the server ships lead, and anything
// registered by hand follows.
func (d *apiDataSource) Harnesses(ctx context.Context, server string) ([]console.Harness, error) {
	d, err := d.on(ctx, server)
	if err != nil {
		return nil, err
	}
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

	harnesses := make([]console.Harness, 0, len(sorted))
	for _, cfg := range sorted {
		harnesses = append(harnesses, toTUIHarness(cfg, defaultID))
	}
	return harnesses, nil
}

// CreateHarnessVariant uses the same create contract as harness create --extends.
func (d *apiDataSource) CreateHarnessVariant(ctx context.Context, server string, variant console.NewHarnessVariant) (console.Harness, error) {
	d, err := d.on(ctx, server)
	if err != nil {
		return console.Harness{}, err
	}
	res, err := d.client.CreateHarnessConfig(ctx, &apimodel.CreateHarnessConfigBody{
		Extends: apiclientgen.NewOptString(variant.ParentID),
		Name:    apiclientgen.NewOptString(variant.Name),
		Slug:    apiclientgen.NewOptString(variant.Slug),
	}, apiclientgen.CreateHarnessConfigParams{ProjectId: d.projectID})
	if err != nil {
		return console.Harness{}, err
	}
	cfg, err := expectResponse[apimodel.HarnessConfig](res)
	if err != nil {
		return console.Harness{}, err
	}
	return toTUIHarness(*cfg, ""), nil
}

func toTUIHarness(cfg apimodel.HarnessConfig, defaultID string) console.Harness {
	harness := console.Harness{
		Parent:   cfg.Extends.Or(""),
		ID:       cfg.ID,
		Name:     strings.TrimSpace(cfg.Name),
		Slug:     strings.TrimSpace(cfg.Slug),
		Default:  cfg.ID == defaultID,
		BuiltIn:  cfg.BuiltIn,
		Shell:    strings.TrimSpace(cfg.Slug) == harness.ShellSlug,
		Bundle:   cfg.Bundle.Or(""),
		Digest:   cfg.BundleDigest.Or(""),
		Run:      cfg.RunCommand,
		Relaunch: cfg.RelaunchCommand.Or(nil),
		Updated:  cfg.UpdatedAt,
	}
	for _, secret := range cfg.Secrets.Or(nil) {
		harness.Secrets = append(harness.Secrets, console.HarnessSecret{
			Name:     secret.Name,
			Required: secret.Required.Or(false),
			OneOf:    secret.OneOfGroup.Or(""),
			Declared: true,
		})
	}
	// Each path once, as a discobox is given it: a person's files lead, then
	// the saved ones, then the bundle's.
	for _, file := range harnessEffectiveFiles(&cfg) {
		harness.Files = append(harness.Files, console.HarnessFile{
			Path:       file.Path,
			Content:    file.Content,
			Source:     file.Source,
			Shadows:    file.Shadows,
			CreateOnly: file.CreateOnly.Or(false),
		})
	}
	return harness
}

// HarnessSecrets is what actually answers each environment variable the harness
// needs: the image's declarations resolved against the project's secret
// bindings, plus the bindings the image never declared, which are the ones
// somebody bound by hand.
func (d *apiDataSource) HarnessSecrets(ctx context.Context, server, harnessID string) ([]console.HarnessSecret, error) {
	d, err := d.on(ctx, server)
	if err != nil {
		return nil, err
	}
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

func harnessSecrets(cfg apimodel.HarnessConfig, bindings []apimodel.SecretBinding, secretsByID map[string]apimodel.Secret) []console.HarnessSecret {
	boundByEnv := make(map[string]string, len(bindings))
	for _, binding := range bindings {
		if env := binding.EnvVar.Or(""); env != "" {
			boundByEnv[env] = binding.SecretId
		}
	}

	out := make([]console.HarnessSecret, 0, len(bindings))
	declared := map[string]bool{}
	for _, secret := range cfg.Secrets.Or(nil) {
		declared[secret.Name] = true
		out = append(out, resolveHarnessSecret(console.HarnessSecret{
			Name:     secret.Name,
			Required: secret.Required.Or(false),
			OneOf:    secret.OneOfGroup.Or(""),
			Declared: true,
		}, boundByEnv[secret.Name], secretsByID))
	}
	// A binding for a variable the image never declared still answers one, so
	// it is listed too rather than silently dropped.
	for _, binding := range bindings {
		env := binding.EnvVar.Or("")
		if env == "" || declared[env] {
			continue
		}
		out = append(out, resolveHarnessSecret(console.HarnessSecret{Name: terminalSafe(env)}, binding.SecretId, secretsByID))
	}
	return out
}

func resolveHarnessSecret(out console.HarnessSecret, secretID string, secretsByID map[string]apimodel.Secret) console.HarnessSecret {
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
func (d *apiDataSource) DoHarness(ctx context.Context, server string, verb console.HarnessVerb, harnessID string) error {
	d, err := d.on(ctx, server)
	if err != nil {
		return err
	}
	switch verb {
	case console.HarnessSetDefault:
		return d.app.setDefaultHarnessConfig(ctx, d.client, d.projectID, harnessID)

	case console.HarnessReset, console.HarnessResetOverrides:
		res, err := d.client.ResetHarnessConfig(ctx, apiclientgen.ResetHarnessConfigParams{
			ProjectId: d.projectID, HarnessConfigId: harnessID,
			IncludeOverrides: apiclientgen.NewOptBool(verb == console.HarnessResetOverrides),
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

// EditHarnessFile opens one of the harness's files in the user's editor and
// saves what it wrote back. The config is re-read first, so the editor opens on
// what the file is now rather than on what the listing said it was.
func (d *apiDataSource) EditHarnessFile(ctx context.Context, server, harnessID, path string, stdin io.Reader, stdout, stderr io.Writer) (bool, error) {
	d, err := d.on(ctx, server)
	if err != nil {
		return false, err
	}
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
