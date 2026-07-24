package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/id"
)

type harnessCreateOptions struct {
	name           string
	slug           string
	image          string
	files          []string
	createOnlyFile []string
}

type harnessUpdateOptions struct {
	name           string
	files          []string
	createOnlyFile []string
}

func (a *App) newHarnessCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "harnesses", Aliases: []string{"harness"}, Short: "Manage harness configs"}
	cmd.AddCommand(a.newHarnessListCommand())
	cmd.AddCommand(a.newHarnessGetCommand())
	cmd.AddCommand(a.newHarnessCreateCommand())
	cmd.AddCommand(a.newHarnessConfigureCommand())
	cmd.AddCommand(a.newHarnessDeconfigureCommand())
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
	cmd := &cobra.Command{Use: "secrets", Aliases: []string{"secret"}, Short: "Manage a harness config's secret bindings"}
	cmd.AddCommand(a.newHarnessSecretsListCommand())
	cmd.AddCommand(a.newHarnessSecretsBindCommand())
	cmd.AddCommand(a.newHarnessSecretsUnbindCommand())
	return cmd
}

func (a *App) newHarnessSecretsListCommand() *cobra.Command {
	return &cobra.Command{Use: "ls HARNESS_CONFIG_ID", Aliases: []string{"list"}, Short: "List a harness config's declared secrets and their bindings", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeHarnessConfigs, RunE: func(cmd *cobra.Command, args []string) error {
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
}

func (a *App) newHarnessSecretsBindCommand() *cobra.Command {
	return &cobra.Command{Use: "bind HARNESS_CONFIG_ID ENV_NAME SECRET_ID", Short: "Bind a harness config environment variable to a secret", Args: cobra.ExactArgs(3), ValidArgsFunction: a.completeHarnessConfigs, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, harnessID, client, err := a.harnessRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		secretID, err := a.resolveSecretID(cmd.Context(), client, projectID, args[2])
		if err != nil {
			return err
		}
		body := &apimodel.SetHarnessConfigSecretBindingBody{SecretId: secretID}
		res, err := client.SetHarnessConfigSecretBinding(cmd.Context(), body, apiclientgen.SetHarnessConfigSecretBindingParams{ProjectId: projectID, HarnessConfigId: harnessID, EnvName: args[1]})
		if err != nil {
			return err
		}
		binding, err := expectResponse[apimodel.HarnessConfigSecretBinding](res)
		if err != nil {
			return err
		}
		if a.output == "json" {
			return writeJSON(cmd.OutOrStdout(), binding)
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "bound %s to secret %s\n", binding.EnvName, binding.SecretId)
		return err
	}}
}

func (a *App) newHarnessSecretsUnbindCommand() *cobra.Command {
	return &cobra.Command{Use: "unbind HARNESS_CONFIG_ID ENV_NAME", Short: "Remove a harness config secret binding", Args: cobra.ExactArgs(2), ValidArgsFunction: a.completeHarnessConfigs, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, harnessID, client, err := a.harnessRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		res, err := client.DeleteHarnessConfigSecretBinding(cmd.Context(), apiclientgen.DeleteHarnessConfigSecretBindingParams{ProjectId: projectID, HarnessConfigId: harnessID, EnvName: args[1]})
		if err != nil {
			return err
		}
		if err := expectNoContent[apiclientgen.DeleteHarnessConfigSecretBindingNoContent](res); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s unbound\n", args[1])
		return err
	}}
}

// harnessSecretAssignments returns a harness's env→secret bindings along with
// the project's secrets by ID, so callers can show what is actually assigned
// to each declared environment variable.
func (a *App) harnessSecretAssignments(ctx context.Context, client *apiclientgen.Client, projectID, harnessID string) ([]apimodel.HarnessConfigSecretBinding, map[string]apimodel.Secret, error) {
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

func (a *App) listHarnessSecretBindings(ctx context.Context, client *apiclientgen.Client, projectID, harnessID string) ([]apimodel.HarnessConfigSecretBinding, error) {
	res, err := client.ListHarnessConfigSecretBindings(ctx, apiclientgen.ListHarnessConfigSecretBindingsParams{ProjectId: projectID, HarnessConfigId: harnessID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListHarnessConfigSecretBindingsBody](res)
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
	return &cobra.Command{Use: "get HARNESS_CONFIG_ID", Short: "Get a harness config", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeHarnessConfigs, RunE: func(cmd *cobra.Command, args []string) error {
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
	cmd.Flags().StringVar(&opts.image, "image", "", "Harness Docker image to register; the server reads and validates its io.discobox.harness.v1 metadata label")
	cmd.Flags().StringArrayVar(&opts.files, "file", nil, "File to write into the harness's home directory, as PATH=CONTENT or PATH=@LOCALFILE (repeatable)")
	cmd.Flags().StringArrayVar(&opts.createOnlyFile, "create-only-file", nil, "File path that should only be created if it does not already exist. Can be repeated and must match a --file PATH.")
	return cmd
}

// newHarnessConfigureCommand runs a harness's configure flow. The server owns
// applying the result: this drives the sequence and hands the terminal to the
// user in between.
//
//	configure        create the ephemeral configure sandbox
//	configure/attach seed the previous configuration into it
//	attach "primary" launch the configure command and let the user drive it
//	configure/commit verify it exited 0, apply what it wrote, delete the sandbox
func (a *App) newHarnessConfigureCommand() *cobra.Command {
	var setDefault bool
	cmd := &cobra.Command{
		Use:               "configure HARNESS",
		Short:             "Run a harness's interactive configure flow",
		Long:              "Run a harness's interactive configure flow. Re-running reconfigures it, and the previous configuration is offered to the harness so it can pre-fill.",
		Args:              cobra.ExactArgs(1),
		Aliases:           []string{"enable", "reconfigure"},
		ValidArgsFunction: a.completeHarnessConfigs,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, harnessID, client, err := a.harnessRequest(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			harness, err := a.runHarnessConfigure(cmd.Context(), client, projectID, harnessID,
				cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			if setDefault {
				if err := a.setDefaultHarnessConfig(cmd.Context(), client, projectID, harness.ID); err != nil {
					return err
				}
			}
			return a.writeHarness(cmd, harness)
		},
	}
	cmd.Flags().BoolVar(&setDefault, "default", false, "Make this the project's default harness once configured")
	return cmd
}

// runHarnessConfigure drives the configure sequence to completion. It takes the
// streams rather than a command so the TUI can hand it the real terminal it
// restores for the duration.
func (a *App) runHarnessConfigure(ctx context.Context, client *apiclientgen.Client, projectID, harnessID string,
	stdin io.Reader, stdout, stderr io.Writer,
) (*apimodel.HarnessConfig, error) {
	sandboxRes, err := client.ConfigureHarnessConfig(ctx, apiclientgen.ConfigureHarnessConfigParams{
		ProjectId: projectID, HarnessConfigId: harnessID,
	})
	if err != nil {
		return nil, fmt.Errorf("start configure: %w", err)
	}
	sandbox, err := expectResponse[apimodel.Sandbox](sandboxRes)
	if err != nil {
		return nil, fmt.Errorf("start configure: %w", err)
	}

	fmt.Fprintf(stderr, "Running configure sandbox %s, waiting for it to start...\n", id.RandomPart(sandbox.ID))
	if _, err := a.waitForSandboxCtx(ctx, client, projectID, sandbox.ID, 2*time.Minute); err != nil {
		return nil, fmt.Errorf("configure sandbox failed to start: %w", err)
	}
	// Seeding must land before the configure command runs. It does: in config mode
	// the sandbox-agent holds the primary terminal until it is attached, so nothing
	// has started yet.
	attachRes, err := client.AttachHarnessConfigConfigure(ctx, apiclientgen.AttachHarnessConfigConfigureParams{
		ProjectId: projectID, HarnessConfigId: harnessID,
	})
	if err != nil {
		return nil, fmt.Errorf("prepare configure: %w", err)
	}
	if err := expectNoContent[apiclientgen.AttachHarnessConfigConfigureNoContent](attachRes); err != nil {
		return nil, fmt.Errorf("prepare configure: %w", err)
	}

	// Attaching the virtual primary exec is what launches the configure command,
	// so there is no terminal to wait for first.
	fmt.Fprintf(stderr, "Attaching to configure terminal (answer any prompts)\n")
	if err := a.attachSandboxTerminal(ctx, projectID, sandbox.ID, primaryExecID, stdin, stdout, stderr); err != nil {
		return nil, fmt.Errorf("attach configure terminal: %w", err)
	}

	// The server checks the real exit status; detaching early surfaces as a
	// commit error rather than a silent success.
	committedRes, err := client.CommitHarnessConfigConfigure(ctx, apiclientgen.CommitHarnessConfigConfigureParams{
		ProjectId: projectID, HarnessConfigId: harnessID,
	})
	if err != nil {
		return nil, fmt.Errorf("configure: %w", err)
	}
	committed, err := expectResponse[apimodel.HarnessConfig](committedRes)
	if err != nil {
		return nil, fmt.Errorf("configure: %w", err)
	}
	return committed, nil
}

func (a *App) newHarnessDeconfigureCommand() *cobra.Command {
	return &cobra.Command{
		Use:               "deconfigure HARNESS",
		Short:             "Undo a harness's configure flow",
		Long:              "Remove the secrets and files the configure flow created and mark the harness unconfigured. The harness itself is kept and can be configured again.",
		Args:              cobra.ExactArgs(1),
		Aliases:           []string{"disable"},
		ValidArgsFunction: a.completeHarnessConfigs,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, harnessID, client, err := a.harnessRequest(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			res, err := client.DeconfigureHarnessConfig(cmd.Context(), apiclientgen.DeconfigureHarnessConfigParams{
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

func (a *App) newHarnessRefreshImageCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh-image HARNESS",
		Short: "Re-snapshot a harness from its image",
		Long: "Re-inspect the harness's image and re-snapshot its run command, files, secrets,\n" +
			"environment, volumes, and digest.\n\n" +
			"Registration reads the image's label once, so a harness pointing at a rebuilt\n" +
			"tag keeps describing an image that no longer exists under it — and its sandboxes\n" +
			"never report an available upgrade. Built-in harnesses refresh themselves on\n" +
			"server start; harnesses registered from your own image need this. Configured\n" +
			"harnesses stay configured.",
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
	cmd := &cobra.Command{Use: "update HARNESS_CONFIG_ID", Short: "Update a harness config", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeHarnessConfigs, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, harnessID, client, err := a.harnessRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		body, err := updateHarnessBody(cmd, opts)
		if err != nil {
			return err
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
	cmd.Flags().StringArrayVar(&opts.files, "file", nil, "File to write into the harness's home directory, as PATH=CONTENT or PATH=@LOCALFILE (repeatable)")
	cmd.Flags().StringArrayVar(&opts.createOnlyFile, "create-only-file", nil, "File path that should only be created if it does not already exist. Can be repeated and must match a --file PATH.")
	return cmd
}

func (a *App) newHarnessSetDefaultCommand() *cobra.Command {
	return &cobra.Command{Use: "set-default HARNESS_CONFIG_ID", Short: "Set the project default harness config", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeHarnessConfigs, RunE: func(cmd *cobra.Command, args []string) error {
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
	return &cobra.Command{Use: "unset-default HARNESS_CONFIG_ID", Short: "Clear the project default harness config", Long: `Clear the project default harness config.

Leaves the project with no default, so new sandboxes created without an explicit
harness run agent-less. HARNESS_CONFIG_ID must be the current default; this is
also how you release the default before disabling that harness.`, Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeHarnessConfigs, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, harnessID, client, err := a.harnessRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		res, err := client.UnsetDefaultHarnessConfig(cmd.Context(), apiclientgen.UnsetDefaultHarnessConfigParams{ProjectId: projectID, HarnessConfigId: harnessID})
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
		_, err = cmd.OutOrStdout().Write([]byte("default harness config cleared\n"))
		return err
	}}
}

func (a *App) newHarnessDeleteCommand() *cobra.Command {
	return &cobra.Command{Use: "delete HARNESS_CONFIG_ID...", Short: "Delete harness configs", Args: cobra.MinimumNArgs(1), ValidArgsFunction: a.completeHarnessConfigs, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		return runDeleteMany(cmd, args, "harness config", func(arg string) (string, error) {
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
	body.Image = strings.TrimSpace(opts.image)
	if len(opts.files) > 0 {
		files, err := parseHarnessFileFlags(opts.files, opts.createOnlyFile)
		if err != nil {
			return nil, err
		}
		body.SetFiles(apiclientgen.NewOptNilHarnessConfigFileArray(files))
	}
	return body, nil
}

func updateHarnessBody(cmd *cobra.Command, opts harnessUpdateOptions) (*apimodel.UpdateHarnessConfigBody, error) {
	body := &apimodel.UpdateHarnessConfigBody{}
	if cmd.Flags().Changed("name") {
		body.SetName(apiclientgen.NewOptString(opts.name))
	}
	if cmd.Flags().Changed("file") {
		files, err := parseHarnessFileFlags(opts.files, opts.createOnlyFile)
		if err != nil {
			return nil, err
		}
		body.SetFiles(apiclientgen.NewOptNilHarnessConfigFileArray(files))
	}
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
