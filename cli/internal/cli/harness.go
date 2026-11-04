package cli

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

type harnessCreateOptions struct {
	name           string
	slug           string
	bundle         string
	files          []string
	createOnlyFile []string
}

type harnessUpdateOptions struct {
	name           string
	bundle         string
	files          []string
	createOnlyFile []string
	removeFiles    []string
}

func (a *App) newHarnessCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "harness", Aliases: []string{"harnesses"}, Short: "Manage harness configs"}
	cmd.AddCommand(a.newHarnessListCommand())
	cmd.AddCommand(a.newHarnessChangesCommand(), a.newHarnessSaveCommand(), a.newHarnessDiscardCommand(), a.newHarnessReconsiderCommand())
	cmd.AddCommand(a.newHarnessGetCommand())
	cmd.AddCommand(a.newHarnessCreateCommand())
	cmd.AddCommand(a.newHarnessResetCommand())
	cmd.AddCommand(a.newHarnessRefreshImageCommand())
	cmd.AddCommand(a.newHarnessUpdateCommand())
	cmd.AddCommand(a.newHarnessEditCommand())
	cmd.AddCommand(a.newHarnessSetDefaultCommand())
	cmd.AddCommand(a.newHarnessUnsetDefaultCommand())
	cmd.AddCommand(a.newHarnessDeleteCommand())
	cmd.AddCommand(a.newHarnessSecretsCommand())
	return cmd
}

func (a *App) newHarnessSecretsCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "secret", Aliases: []string{"secrets"}, Short: "Show a harness config's declared secrets and what is bound to them", Long: `Show a harness config's declared secrets and what is bound to them.

A secret is bound to a harness config with ` + "`discobox secret bind SECRET --harness HARNESS --as VAR`" + `,
and taken back with ` + "`discobox secret unbind`" + `.`}
	cmd.AddCommand(a.newHarnessSecretsListCommand())
	return cmd
}

func (a *App) newHarnessSecretsListCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "ls HARNESS", Aliases: []string{"list"}, Short: "List a harness config's declared secrets and their bindings", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeHarnessConfigs, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, harnessID, client, err := a.harnessRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		harnessRes, err := client.GetHarnessConfig(cmd.Context(), apiclientgen.GetHarnessConfigParams{ProjectId: projectID, HarnessConfigId: harnessID})
		if err != nil {
			return err
		}
		harness, err := expectResponse[apimodel.HarnessConfig](harnessRes)
		if err != nil {
			return err
		}
		bindings, err := a.listHarnessSecretBindings(cmd.Context(), client, projectID, harnessID)
		if err != nil {
			return err
		}
		return a.writeHarnessSecretBindings(cmd, harness.Secrets.Or(nil), bindings)
	}}
	a.addQuietFlag(cmd)
	return cmd
}

// harnessSecretAssignments returns a harness's env→secret bindings along with
// the project's secrets by ID, so callers can show what is actually assigned
// to each declared environment variable.
func (a *App) harnessSecretAssignments(ctx context.Context, client *apiclientgen.Client, projectID, harnessID string) ([]apimodel.SecretBinding, map[string]apimodel.Secret, error) {
	bindings, err := a.listHarnessSecretBindings(ctx, client, projectID, harnessID)
	if err != nil {
		return nil, nil, err
	}
	if len(bindings) == 0 {
		return nil, nil, nil
	}
	res, err := client.ListSecrets(ctx, apiclientgen.ListSecretsParams{ProjectId: projectID})
	if err != nil {
		return nil, nil, err
	}
	body, err := expectResponse[apimodel.ListSecretsBody](res)
	if err != nil {
		return nil, nil, err
	}
	secretsByID := make(map[string]apimodel.Secret)
	for _, secret := range body.GetSecrets() {
		secretsByID[secret.ID] = secret
	}
	return bindings, secretsByID, nil
}

// listHarnessSecretBindings are the bindings on a harness config itself, which
// every discobox on it holds unless one of its own binds the same variable.
func (a *App) listHarnessSecretBindings(ctx context.Context, client *apiclientgen.Client, projectID, harnessID string) ([]apimodel.SecretBinding, error) {
	res, err := client.ListSecretBindings(ctx, apiclientgen.ListSecretBindingsParams{
		ProjectId:  projectID,
		TargetKind: apiclientgen.NewOptListSecretBindingsTargetKind(apiclientgen.ListSecretBindingsTargetKindHarnessConfig),
		Target:     apiclientgen.NewOptString(harnessID),
	})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListSecretBindingsBody](res)
	if err != nil {
		return nil, err
	}
	return body.GetSecretBindings(), nil
}

func (a *App) newHarnessListCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "ls", Aliases: []string{"list"}, Short: "List harness configs", RunE: func(cmd *cobra.Command, _ []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		harnesses, err := a.listHarnessConfigs(cmd.Context(), client, projectID)
		if err != nil {
			return err
		}
		var defaultHarnessConfigID string
		if !a.quiet && a.output != "json" {
			defaultHarnessConfigID, err = a.defaultHarnessConfigID(cmd.Context(), client, projectID)
			if err != nil {
				return err
			}
		}
		return a.writeHarnesses(cmd, harnesses, defaultHarnessConfigID)
	}}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newHarnessGetCommand() *cobra.Command {
	return &cobra.Command{Use: "get HARNESS", Short: "Show a harness config", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeHarnessConfigs, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, harnessID, client, err := a.harnessRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		harnessRes, err := client.GetHarnessConfig(cmd.Context(), apiclientgen.GetHarnessConfigParams{ProjectId: projectID, HarnessConfigId: harnessID})
		if err != nil {
			return err
		}
		harness, err := expectResponse[apimodel.HarnessConfig](harnessRes)
		if err != nil {
			return err
		}
		return a.writeHarness(cmd, harness)
	}}
}

func (a *App) newHarnessCreateCommand() *cobra.Command {
	var opts harnessCreateOptions
	cmd := &cobra.Command{Use: "create", Short: "Create a harness config", RunE: func(cmd *cobra.Command, _ []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		body, err := createHarnessBody(opts)
		if err != nil {
			return err
		}
		harnessRes, err := client.CreateHarnessConfig(cmd.Context(), body, apiclientgen.CreateHarnessConfigParams{ProjectId: projectID})
		if err != nil {
			return err
		}
		harness, err := expectResponse[apimodel.HarnessConfig](harnessRes)
		if err != nil {
			return err
		}
		return a.writeHarness(cmd, harness)
	}}
	cmd.Flags().StringVar(&opts.name, "name", "", "Harness config name")
	cmd.Flags().StringVar(&opts.slug, "slug", "", "Stable, URL-safe identifier used to select this harness (e.g. codex); defaults to the definition ID or a slug derived from the name")
	cmd.Flags().StringVar(&opts.bundle, "bundle", "", "Harness bundle to register: a FROM scratch image of its features, built FROM nothing or another harness's bundle, whose labels are its manifest")
	cmd.Flags().StringArrayVar(&opts.files, "file", nil, "Add a file to the harness's home directory, or override the bundle's file at PATH, as PATH=CONTENT or PATH=@LOCALFILE (repeatable)")
	cmd.Flags().StringArrayVar(&opts.createOnlyFile, "create-only-file", nil, "File path that should only be created if it does not already exist. Can be repeated and must match a --file PATH.")
	return cmd
}

// newHarnessResetCommand returns a harness config to its bundle's defaults
// (ADR 26-11-02-152 §3): the files and logins saved from discoboxes' harness
// changes go, and the bundle's own baseline is what every box gets again.
func (a *App) newHarnessResetCommand() *cobra.Command {
	var includeOverrides bool
	cmd := &cobra.Command{
		Use:   "reset HARNESS",
		Short: "Forget a harness's saved settings and logins",
		Long: "Return the harness to its bundle's defaults: remove the settings files saved\n" +
			"from discoboxes' harness changes, and the logins those saves created along with\n" +
			"their bindings. Secrets you bound to the harness by hand are kept, and so are\n" +
			"the files you added or overrode yourself, unless --include-overrides is given.\n" +
			"New discoboxes on the harness start from the bundle's defaults, signed out; a\n" +
			"login made in one of them can be saved again.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeHarnessConfigs,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, harnessID, client, err := a.harnessRequest(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			res, err := client.ResetHarnessConfig(cmd.Context(), apiclientgen.ResetHarnessConfigParams{
				ProjectId: projectID, HarnessConfigId: harnessID,
				IncludeOverrides: apiclientgen.NewOptBool(includeOverrides),
			})
			if err != nil {
				return err
			}
			harness, err := expectResponse[apimodel.HarnessConfig](res)
			if err != nil {
				return err
			}
			return a.writeHarness(cmd, harness)
		},
	}
	cmd.Flags().BoolVar(&includeOverrides, "include-overrides", false, "Also remove the files you added or overrode, leaving nothing but the bundle's")
	return cmd
}

func (a *App) newHarnessRefreshImageCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "refresh HARNESS",
		Aliases: []string{"refresh-image"},
		Short:   "Re-snapshot a harness from its bundle",
		Long: "Re-inspect the harness's bundle and re-snapshot its run command, files,\n" +
			"secrets, environment, volumes, and digests.\n\n" +
			"Registration reads the bundle's labels once, so a harness pointing at a rebuilt\n" +
			"tag keeps describing a bundle that no longer exists under it — and its discoboxes\n" +
			"never report an available upgrade. Built-in harnesses refresh themselves on\n" +
			"server start; harnesses registered from your own bundle need this. A harness\n" +
			"registered before bundles needs one first: `update --bundle`. Saved settings\n" +
			"and logins are kept.",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: a.completeHarnessConfigs,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, harnessID, client, err := a.harnessRequest(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			res, err := client.RefreshHarnessConfigImage(cmd.Context(), apiclientgen.RefreshHarnessConfigImageParams{
				ProjectId: projectID, HarnessConfigId: harnessID,
			})
			if err != nil {
				return err
			}
			harness, err := expectResponse[apimodel.HarnessConfig](res)
			if err != nil {
				return err
			}
			return a.writeHarness(cmd, harness)
		},
	}
}

func (a *App) newHarnessUpdateCommand() *cobra.Command {
	var opts harnessUpdateOptions
	var egressOpts egressFlags
	cmd := &cobra.Command{Use: "update HARNESS", Short: "Update a harness config", Long: `Update a harness config.

--egress-allow, --egress-deny-by-default and --egress-passthrough set the
harness's egress level, which every discobox on the harness converges onto,
running ones included. The hosts the harness bundle declares (imageEgressHosts)
are part of this level already and need not be listed.

` + egressHelp, Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeHarnessConfigs, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, harnessID, client, err := a.harnessRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		currentRes, err := client.GetHarnessConfig(cmd.Context(), apiclientgen.GetHarnessConfigParams{ProjectId: projectID, HarnessConfigId: harnessID})
		if err != nil {
			return err
		}
		current, err := expectResponse[apimodel.HarnessConfig](currentRes)
		if err != nil {
			return err
		}
		body, err := updateHarnessBody(cmd, opts, current)
		if err != nil {
			return err
		}
		level, ok, err := egressOpts.update(cmd, func() (apimodel.EgressPolicy, error) {
			return current.Egress.Value, nil
		})
		if err != nil {
			return err
		}
		if ok {
			body.SetEgress(apiclientgen.NewOptEgressPolicy(level))
		}
		harnessRes, err := client.UpdateHarnessConfig(cmd.Context(), body, apiclientgen.UpdateHarnessConfigParams{ProjectId: projectID, HarnessConfigId: harnessID})
		if err != nil {
			return err
		}
		harness, err := expectResponse[apimodel.HarnessConfig](harnessRes)
		if err != nil {
			return err
		}
		return a.writeHarness(cmd, harness)
	}}
	cmd.Flags().StringVar(&opts.name, "name", "", "Harness config name")
	cmd.Flags().StringVar(&opts.bundle, "bundle", "", "Register the harness bundle its manifest is read from, as a harness registered before bundles needs to refresh")
	cmd.Flags().StringArrayVar(&opts.files, "file", nil, "Add a file to the harness's home directory, or override the harness's own file at PATH, as PATH=CONTENT or PATH=@LOCALFILE; the harness's other files are kept (repeatable)")
	cmd.Flags().StringArrayVar(&opts.createOnlyFile, "create-only-file", nil, "File path that should only be created if it does not already exist. Can be repeated and must match a --file PATH.")
	cmd.Flags().StringArrayVar(&opts.removeFiles, "remove-file", nil, "Remove the file you added or overrode at PATH, which brings back the harness's own file there, if it has one (repeatable)")
	addClearFlag(cmd, "file", "Remove every file you added or overrode; the harness's own files are kept")
	addEgressUpdateFlags(cmd, &egressOpts, "the harness")
	return cmd
}

func (a *App) newHarnessSetDefaultCommand() *cobra.Command {
	return &cobra.Command{Use: "set-default HARNESS", Short: "Set the project default harness config", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeHarnessConfigs, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, harnessID, client, err := a.harnessRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		res, err := client.SetDefaultHarnessConfig(cmd.Context(), apiclientgen.SetDefaultHarnessConfigParams{ProjectId: projectID, HarnessConfigId: harnessID})
		if err != nil {
			return err
		}
		project, err := expectResponse[apimodel.Project](res)
		if err != nil {
			return err
		}
		if a.output == "json" {
			return writeJSON(cmd.OutOrStdout(), project)
		}
		_, err = cmd.OutOrStdout().Write([]byte("default harness config set to " + harnessID + "\n"))
		return err
	}}
}

func (a *App) newHarnessUnsetDefaultCommand() *cobra.Command {
	return &cobra.Command{Use: "unset-default", Short: "Clear the project default harness config", Long: `Clear the project default harness config.

Leaves the project with no default, so new discoboxes created without an explicit
harness run agent-less. This is also how you release the default before
disabling that harness. With no default set, it changes nothing.`, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		project, err := a.getProject(cmd.Context(), client, projectID)
		if err != nil {
			return err
		}
		if harnessID := project.DefaultHarnessConfigId.Or(""); harnessID != "" {
			res, err := client.UnsetDefaultHarnessConfig(cmd.Context(), apiclientgen.UnsetDefaultHarnessConfigParams{ProjectId: projectID, HarnessConfigId: harnessID})
			if err != nil {
				return err
			}
			if project, err = expectResponse[apimodel.Project](res); err != nil {
				return err
			}
		}
		if a.output == "json" {
			return writeJSON(cmd.OutOrStdout(), project)
		}
		_, err = cmd.OutOrStdout().Write([]byte("default harness config cleared\n"))
		return err
	}}
}

func (a *App) newHarnessDeleteCommand() *cobra.Command {
	return &cobra.Command{Use: "rm HARNESS...", Aliases: []string{"delete", "remove"}, Short: "Delete harness configs", Args: cobra.MinimumNArgs(1), ValidArgsFunction: a.completeHarnessConfigs, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		return runActionMany(cmd, args, "harness config", "delete", "deleted", func(arg string) (string, error) {
			harnessID, err := a.resolveHarnessConfigID(cmd.Context(), client, projectID, arg)
			if err != nil {
				return "", err
			}
			res, err := client.DeleteHarnessConfig(cmd.Context(), apiclientgen.DeleteHarnessConfigParams{ProjectId: projectID, HarnessConfigId: harnessID})
			if err != nil {
				return "", err
			}
			if err := expectNoContent[apiclientgen.DeleteHarnessConfigNoContent](res); err != nil {
				return "", err
			}
			return harnessID, nil
		})
	}}
}

func (a *App) harnessRequest(ctx context.Context, harnessArg string) (projectID string, harnessID string, client *apiclientgen.Client, err error) {
	projectID, err = a.projectIDValue()
	if err != nil {
		return projectID, harnessID, nil, err
	}
	client, err = a.apiClient()
	if err != nil {
		return projectID, harnessID, nil, err
	}
	harnessID, err = a.resolveHarnessConfigID(ctx, client, projectID, harnessArg)
	return projectID, harnessID, client, err
}

// harnessDisplayName is what a harness is called in a message: its name, the
// slug it is run by, or failing both the id, which is the one thing it always
// has.
func harnessDisplayName(cfg apimodel.HarnessConfig) string {
	if name := strings.TrimSpace(cfg.Name); name != "" {
		return name
	}
	if slug := strings.TrimSpace(cfg.Slug); slug != "" {
		return slug
	}
	return cfg.ID
}

func (a *App) listHarnessConfigs(ctx context.Context, client *apiclientgen.Client, projectID string) ([]apimodel.HarnessConfig, error) {
	res, err := client.ListHarnessConfigs(ctx, apiclientgen.ListHarnessConfigsParams{ProjectId: projectID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListHarnessConfigsBody](res)
	if err != nil {
		return nil, err
	}
	return body.GetHarnessConfigs(), nil
}

func (a *App) defaultHarnessConfigID(ctx context.Context, client *apiclientgen.Client, projectID string) (string, error) {
	res, err := client.GetProject(ctx, apiclientgen.GetProjectParams{ProjectId: projectID})
	if err != nil {
		return "", err
	}
	project, err := expectResponse[apimodel.Project](res)
	if err != nil {
		return "", err
	}
	return project.DefaultHarnessConfigId.Or(""), nil
}
func (a *App) setDefaultHarnessConfig(ctx context.Context, client *apiclientgen.Client, projectID, harnessID string) error {
	res, err := client.SetDefaultHarnessConfig(ctx, apiclientgen.SetDefaultHarnessConfigParams{ProjectId: projectID, HarnessConfigId: harnessID})
	if err != nil {
		return err
	}
	_, err = expectResponse[apimodel.Project](res)
	return err
}

func createHarnessBody(opts harnessCreateOptions) (*apimodel.CreateHarnessConfigBody, error) {
	body := &apimodel.CreateHarnessConfigBody{}
	body.SetName(optString(opts.name))
	body.SetSlug(optString(opts.slug))
	body.SetBundle(optString(opts.bundle))
	if len(opts.files) > 0 {
		files, err := parseHarnessFileFlags(opts.files, opts.createOnlyFile)
		if err != nil {
			return nil, err
		}
		body.SetFileOverrides(apiclientgen.NewOptNilHarnessConfigFileArray(files))
	}
	return body, nil
}

// updateHarnessBody builds the update of current. A person's files are one set
// the server replaces whole (ADR 26-11-04-028 §4), so --file and
// --remove-file change the paths they name in the set current holds and send
// the rest back as it was, under the revision read with it.
func updateHarnessBody(cmd *cobra.Command, opts harnessUpdateOptions, current *apimodel.HarnessConfig) (*apimodel.UpdateHarnessConfigBody, error) {
	body := &apimodel.UpdateHarnessConfigBody{Revision: current.Revision.Or(0)}
	if cmd.Flags().Changed("name") {
		body.SetName(apiclientgen.NewOptString(opts.name))
	}
	if cmd.Flags().Changed("bundle") {
		body.SetBundle(apiclientgen.NewOptString(opts.bundle))
	}
	if listCleared(cmd.Flags(), "file") {
		if len(opts.removeFiles) > 0 {
			return nil, fmt.Errorf("--clear-file and --remove-file cannot be used together")
		}
		body.SetFileOverrides(apiclientgen.NewOptNilHarnessConfigFileArray([]apimodel.HarnessConfigFile{}))
		return body, nil
	}
	if !cmd.Flags().Changed("file") && len(opts.removeFiles) == 0 {
		return body, nil
	}
	added, err := parseHarnessFileFlags(opts.files, opts.createOnlyFile)
	if err != nil {
		return nil, err
	}
	overrides := append([]apimodel.HarnessConfigFile{}, current.FileOverrides.Or(nil)...)
	for _, path := range opts.removeFiles {
		n := slices.IndexFunc(overrides, func(f apimodel.HarnessConfigFile) bool { return f.Path == path })
		if n < 0 {
			return nil, fmt.Errorf("--remove-file %q: %s has no file you added or overrode there", path, harnessDisplayName(*current))
		}
		overrides = slices.Delete(overrides, n, n+1)
	}
	for _, file := range added {
		if slices.Contains(opts.removeFiles, file.Path) {
			return nil, fmt.Errorf("--file and --remove-file both name %q", file.Path)
		}
		if n := slices.IndexFunc(overrides, func(f apimodel.HarnessConfigFile) bool { return f.Path == file.Path }); n >= 0 {
			overrides[n] = file
		} else {
			overrides = append(overrides, file)
		}
	}
	body.SetFileOverrides(apiclientgen.NewOptNilHarnessConfigFileArray(overrides))
	return body, nil
}

func parseHarnessFileFlags(values []string, createOnlyFiles []string) ([]apimodel.HarnessConfigFile, error) {
	createOnly := map[string]struct{}{}
	for _, path := range createOnlyFiles {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		createOnly[path] = struct{}{}
	}
	files := make([]apimodel.HarnessConfigFile, 0, len(values))
	for _, value := range values {
		path, content, ok := strings.Cut(value, "=")
		path = strings.TrimSpace(path)
		if !ok || path == "" {
			return nil, fmt.Errorf("--file must be PATH=CONTENT or PATH=@LOCALFILE, got %q", value)
		}
		if localPath, isLocalFile := strings.CutPrefix(content, "@"); isLocalFile {
			data, err := os.ReadFile(localPath)
			if err != nil {
				return nil, fmt.Errorf("read --file content %q: %w", localPath, err)
			}
			content = string(data)
		}
		files = append(files, apimodel.HarnessConfigFile{Path: path, Content: content})
	}
	for filePath := range createOnly {
		found := false
		for _, file := range files {
			if file.Path == filePath {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("--create-only-file path %q has no matching --file entry", filePath)
		}
	}
	for i := range files {
		_, isCreateOnly := createOnly[files[i].Path]
		if isCreateOnly {
			files[i].CreateOnly = apiclientgen.NewOptBool(true)
		}
	}
	return files, nil
}
