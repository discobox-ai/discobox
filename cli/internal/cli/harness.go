package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
	name            string
	slug            string
	definitionID    string
	installCommand  []string
	runCommand      []string
	relaunchCommand []string
	files           []string
	createOnlyFile  []string
	requiredSecrets []string
	optionalSecrets []string
}

type harnessUpdateOptions struct {
	name            string
	installCommand  []string
	runCommand      []string
	relaunchCommand []string
	files           []string
	createOnlyFile  []string
	requiredSecrets []string
	optionalSecrets []string
}

func (a *App) newHarnessCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "harnesses", Aliases: []string{"harness"}, Short: "Manage harness configs"}
	cmd.AddCommand(a.newHarnessDefinitionsCommand())
	cmd.AddCommand(a.newHarnessListCommand())
	cmd.AddCommand(a.newHarnessGetCommand())
	cmd.AddCommand(a.newHarnessCreateCommand())
	cmd.AddCommand(a.newHarnessEnableCommand())
	cmd.AddCommand(a.newHarnessDisableCommand())
	cmd.AddCommand(a.newHarnessUpdateCommand())
	cmd.AddCommand(a.newHarnessSetDefaultCommand())
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
	return &cobra.Command{Use: "list HARNESS_CONFIG_ID", Short: "List a harness config's declared secrets and their bindings", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeHarnessConfigs, RunE: func(cmd *cobra.Command, args []string) error {
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

func (a *App) newHarnessDefinitionsCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "definitions", Aliases: []string{"definition", "defs"}, Short: "List harness config definitions", ValidArgsFunction: a.completeHarnessDefinitions, RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		if len(args) > 0 {
			definitionID, err := a.resolveHarnessDefinitionID(cmd.Context(), client, args[0])
			if err != nil {
				return err
			}
			definitionRes, err := client.GetHarnessDefinition(cmd.Context(), apiclientgen.GetHarnessDefinitionParams{DefinitionId: definitionID})
			if err != nil {
				return err
			}
			definition, err := expectResponse[apimodel.HarnessDefinition](definitionRes)
			if err != nil {
				return err
			}
			return a.writeHarnessDefinition(cmd, definition)
		}
		bodyRes, err := client.ListHarnessDefinitions(cmd.Context())
		if err != nil {
			return err
		}
		body, err := expectResponse[apimodel.ListHarnessDefinitionsBody](bodyRes)
		if err != nil {
			return err
		}
		return a.writeHarnessDefinitions(cmd, body.GetHarnessDefinitions())
	}}
	cmd.Args = cobra.MaximumNArgs(1)
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newHarnessListCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "list", Short: "List harness configs", RunE: func(cmd *cobra.Command, _ []string) error {
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
		if opts.definitionID != "" {
			opts.definitionID, err = a.resolveHarnessDefinitionID(cmd.Context(), client, opts.definitionID)
			if err != nil {
				return err
			}
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
	cmd.Flags().StringVar(&opts.definitionID, "definition", "", "Built-in harness definition to extend; unset fields are inherited and pick up definition upgrades")
	cmd.Flags().StringArrayVar(&opts.installCommand, "install-command", nil, "Argv element used to install the harness (repeatable, e.g. --install-command npm --install-command install). Not run through a shell; pass sh -c yourself for shell semantics.")
	cmd.Flags().StringArrayVar(&opts.runCommand, "run-command", nil, "Argv element used to run the harness (repeatable, e.g. --run-command claude --run-command --dangerously-skip-permissions). Not run through a shell; pass sh -c yourself for shell semantics.")
	cmd.Flags().StringArrayVar(&opts.relaunchCommand, "relaunch-command", nil, "Argv element used to resume the previous harness session on subsequent sandbox starts (repeatable, e.g. --relaunch-command claude --relaunch-command --continue). Replaces the run command for non-first launches. Not run through a shell.")
	cmd.Flags().StringArrayVar(&opts.files, "file", nil, "File to write into the harness's home directory, as PATH=CONTENT or PATH=@LOCALFILE (repeatable)")
	cmd.Flags().StringArrayVar(&opts.createOnlyFile, "create-only-file", nil, "File path that should only be created if it does not already exist. Can be repeated and must match a --file PATH.")
	cmd.Flags().StringArrayVar(&opts.requiredSecrets, "required-secret", nil, "Environment variable name of a required secret the harness expects (repeatable, e.g. --required-secret ANTHROPIC_API_KEY)")
	cmd.Flags().StringArrayVar(&opts.optionalSecrets, "optional-secret", nil, "Environment variable name of an optional secret the harness uses when present (repeatable)")
	_ = cmd.RegisterFlagCompletionFunc("definition", a.completeHarnessDefinitions)
	return cmd
}

func (a *App) newHarnessEnableCommand() *cobra.Command {
	var setDefault bool
	var noConfigure bool
	cmd := &cobra.Command{Use: "enable DEFINITION_NAME", Aliases: []string{"enabled"}, Short: "Enable a harness config definition", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeHarnessDefinitions, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		definition, err := a.resolveHarnessDefinition(cmd.Context(), client, args[0])
		if err != nil {
			return err
		}
		harnesses, err := a.listHarnessConfigs(cmd.Context(), client, projectID)
		if err != nil {
			return err
		}
		existing := harnessConfigBySlug(harnesses, definition.ID)
		if existing != nil {
			if setDefault {
				if err := a.setDefaultHarnessConfig(cmd.Context(), client, projectID, existing.ID); err != nil {
					return err
				}
			}
			return a.writeHarness(cmd, existing)
		}
		var configured *harnessConfigureOutput
		if configureSpec, ok := definition.Configure.Get(); ok && !noConfigure {
			configured, err = a.runHarnessConfigure(cmd, client, projectID, configureSpec)
			if err != nil {
				return fmt.Errorf("harness configure: %w", err)
			}
		}
		body := &apimodel.CreateHarnessConfigBody{}
		body.SetDefinitionId(apiclientgen.NewOptString(definition.ID))
		if configured != nil && len(configured.Files) > 0 {
			body.SetFiles(apiclientgen.NewOptNilHarnessConfigFileArray(configured.Files))
		}
		if configured != nil {
			declarations := make([]apimodel.HarnessConfigSecret, 0, len(configured.Secrets))
			for _, secret := range configured.Secrets {
				if strings.TrimSpace(secret.EnvName) == "" {
					return fmt.Errorf("harness configure: configure secret is missing envName")
				}
				declarations = append(declarations, apimodel.HarnessConfigSecret{
					Name:     strings.TrimSpace(secret.EnvName),
					Required: apiclientgen.NewOptBool(true),
				})
			}
			body.SetSecrets(apiclientgen.NewOptNilHarnessConfigSecretArray(declarations))
		}
		harnessRes, err := client.CreateHarnessConfig(cmd.Context(), body, apiclientgen.CreateHarnessConfigParams{ProjectId: projectID})
		if err != nil {
			return err
		}
		harness, err := expectResponse[apimodel.HarnessConfig](harnessRes)
		if err != nil {
			return err
		}
		if configured != nil {
			if err := a.applyHarnessConfigureSecrets(cmd.Context(), client, projectID, harness.ID, configured.Secrets); err != nil {
				if res, deleteErr := client.DeleteHarnessConfig(cmd.Context(), apiclientgen.DeleteHarnessConfigParams{ProjectId: projectID, HarnessConfigId: harness.ID}); deleteErr == nil {
					_ = expectNoContent[apiclientgen.DeleteHarnessConfigNoContent](res)
				}
				return fmt.Errorf("harness configure: apply secrets: %w", err)
			}
		}
		if setDefault || len(harnesses) == 0 {
			if err := a.setDefaultHarnessConfig(cmd.Context(), client, projectID, harness.ID); err != nil {
				return err
			}
		}
		return a.writeHarness(cmd, harness)
	}}
	cmd.Flags().BoolVarP(&setDefault, "default", "d", false, "Set the project default harness config")
	cmd.Flags().BoolVar(&noConfigure, "no-configure", false, "Skip the definition's interactive configure step even if one is defined")
	return cmd
}

const harnessConfigureOutputPath = "/run/discobox/harness-configure.json"

type harnessConfigureOutput struct {
	Secrets []harnessConfigureSecret     `json:"secrets"`
	Files   []apimodel.HarnessConfigFile `json:"files"`
}

type harnessConfigureSecret struct {
	EnvName string                            `json:"envName"`
	Name    string                            `json:"name"`
	Type    apiclientgen.CreateSecretBodyType `json:"type"`
	Host    string                            `json:"host,omitempty"`
	Value   apimodel.SecretValue              `json:"value"`
}

func (a *App) runHarnessConfigure(cmd *cobra.Command, client *apiclientgen.Client, projectID string, configureSpec apimodel.SandboxCreateConfig) (*harnessConfigureOutput, error) {
	ctx := cmd.Context()
	stderr := cmd.ErrOrStderr()
	runID, err := id.New(id.PrefixSandbox)
	if err != nil {
		return nil, err
	}
	configureSpec.Name = "configure-" + id.RandomPart(runID)[:8]
	sandboxRes, err := client.CreateSandbox(ctx, &apimodel.CreateSandboxBody{Config: configureSpec}, apiclientgen.CreateSandboxParams{ProjectId: projectID})
	if err != nil {
		return nil, fmt.Errorf("create configure sandbox: %w", err)
	}
	sandbox, err := expectResponse[apimodel.Sandbox](sandboxRes)
	if err != nil {
		return nil, fmt.Errorf("create configure sandbox: %w", err)
	}
	defer a.deleteConfigureSandboxQuietly(context.WithoutCancel(ctx), client, projectID, sandbox.ID, stderr)

	fmt.Fprintf(stderr, "Running configure sandbox %s, waiting for it to start...\n", id.RandomPart(sandbox.ID))
	if _, err := a.waitForSandbox(cmd, client, projectID, sandbox.ID, 2*time.Minute); err != nil {
		return nil, fmt.Errorf("configure sandbox failed to start: %w", err)
	}
	terminal, err := a.waitForPrimaryTerminal(ctx, stderr, projectID, sandbox.ID, 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("configure sandbox failed to launch: %w", err)
	}
	fmt.Fprintf(stderr, "Attaching to configure terminal %s (answer any prompts; Ctrl-P Ctrl-Q to detach)\n", id.RandomPart(terminal.ID))
	if err := a.attachSandboxTerminal(ctx, projectID, sandbox.ID, terminal.ID, cmd.InOrStdin(), cmd.OutOrStdout(), stderr); err != nil {
		return nil, fmt.Errorf("attach configure terminal: %w", err)
	}
	finished, err := a.getSandboxExec(ctx, projectID, sandbox.ID, terminal.ID)
	if err != nil {
		return nil, fmt.Errorf("check configure terminal status: %w", err)
	}
	switch finished.Status {
	case apiclientgen.SandboxExecStatusExited:
		if code, ok := finished.ExitCode.Get(); !ok || code != 0 {
			return nil, fmt.Errorf("configure sandbox exited with code %d", code)
		}
	case apiclientgen.SandboxExecStatusFailed, apiclientgen.SandboxExecStatusLost:
		return nil, fmt.Errorf("configure sandbox %s: %s", finished.Status, finished.Error.Or(""))
	default:
		return nil, fmt.Errorf("detached before the configure sandbox finished; re-run to retry")
	}

	var buf bytes.Buffer
	catBody := &apimodel.CreateSandboxExecRequest{}
	catBody.SetCommand([]string{"cat", harnessConfigureOutputPath})
	catExec, err := a.createSandboxExec(ctx, projectID, sandbox.ID, catBody)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", harnessConfigureOutputPath, err)
	}
	if err := a.attachSandboxExec(ctx, projectID, sandbox.ID, catExec.ID, false, false, bytes.NewReader(nil), &buf, stderr); err != nil {
		return nil, fmt.Errorf("read %s: %w", harnessConfigureOutputPath, err)
	}
	if err := a.returnSandboxExecStatus(ctx, projectID, sandbox.ID, catExec.ID); err != nil {
		return nil, fmt.Errorf("read %s: %w", harnessConfigureOutputPath, err)
	}
	var out harnessConfigureOutput
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("%s is invalid: %w", harnessConfigureOutputPath, err)
	}
	return &out, nil
}

func (a *App) applyHarnessConfigureSecrets(ctx context.Context, client *apiclientgen.Client, projectID, harnessConfigID string, secrets []harnessConfigureSecret) error {
	for _, secret := range secrets {
		envName := strings.TrimSpace(secret.EnvName)
		if envName == "" {
			return fmt.Errorf("configure secret is missing envName")
		}
		name := strings.TrimSpace(secret.Name)
		if name == "" {
			name = envName
		}
		body := &apimodel.CreateSecretBody{Name: name, Type: secret.Type, Value: secret.Value}
		if secret.Host != "" {
			body.SetHost(apiclientgen.NewOptString(secret.Host))
		}
		secretRes, err := client.CreateSecret(ctx, body, apiclientgen.CreateSecretParams{ProjectId: projectID})
		if err != nil {
			return err
		}
		created, err := expectResponse[apimodel.Secret](secretRes)
		if err != nil {
			return err
		}
		bindBody := &apimodel.SetHarnessConfigSecretBindingBody{SecretId: created.ID}
		if _, err := client.SetHarnessConfigSecretBinding(ctx, bindBody, apiclientgen.SetHarnessConfigSecretBindingParams{ProjectId: projectID, HarnessConfigId: harnessConfigID, EnvName: envName}); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) deleteConfigureSandboxQuietly(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string, stderr io.Writer) {
	if _, err := client.DeleteSandbox(ctx, apiclientgen.DeleteSandboxParams{ProjectId: projectID, SandboxId: sandboxID}); err != nil {
		fmt.Fprintf(stderr, "warning: failed to delete configure sandbox %s: %v\n", id.RandomPart(sandboxID), err)
	}
}

func (a *App) newHarnessDisableCommand() *cobra.Command {
	return &cobra.Command{Use: "disable DEFINITION_NAME", Short: "Disable a harness config definition", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeHarnessDefinitions, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		definition, err := a.resolveHarnessDefinition(cmd.Context(), client, args[0])
		if err != nil {
			return err
		}
		existing, err := a.harnessConfigBySlug(cmd.Context(), client, projectID, definition.ID)
		if err != nil {
			return err
		}
		if existing == nil {
			return nil
		}
		res, err := client.DeleteHarnessConfig(cmd.Context(), apiclientgen.DeleteHarnessConfigParams{ProjectId: projectID, HarnessConfigId: existing.ID})
		if err != nil {
			return err
		}
		if err := expectNoContent[apiclientgen.DeleteHarnessConfigNoContent](res); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s deleted\n", existing.ID)
		return err
	}}
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
	cmd.Flags().StringArrayVar(&opts.installCommand, "install-command", nil, "Argv element used to install the harness (repeatable, e.g. --install-command npm --install-command install). Not run through a shell; pass sh -c yourself for shell semantics.")
	cmd.Flags().StringArrayVar(&opts.runCommand, "run-command", nil, "Argv element used to run the harness (repeatable, e.g. --run-command claude --run-command --dangerously-skip-permissions). Not run through a shell; pass sh -c yourself for shell semantics.")
	cmd.Flags().StringArrayVar(&opts.relaunchCommand, "relaunch-command", nil, "Argv element used to resume the previous harness session on subsequent sandbox starts (repeatable, e.g. --relaunch-command claude --relaunch-command --continue). Replaces the run command for non-first launches. Not run through a shell.")
	cmd.Flags().StringArrayVar(&opts.files, "file", nil, "File to write into the harness's home directory, as PATH=CONTENT or PATH=@LOCALFILE (repeatable)")
	cmd.Flags().StringArrayVar(&opts.createOnlyFile, "create-only-file", nil, "File path that should only be created if it does not already exist. Can be repeated and must match a --file PATH.")
	cmd.Flags().StringArrayVar(&opts.requiredSecrets, "required-secret", nil, "Environment variable name of a required secret the harness expects (repeatable). Replaces the existing secret set together with --optional-secret.")
	cmd.Flags().StringArrayVar(&opts.optionalSecrets, "optional-secret", nil, "Environment variable name of an optional secret the harness uses when present (repeatable). Replaces the existing secret set together with --required-secret.")
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

func (a *App) resolveHarnessDefinition(ctx context.Context, client *apiclientgen.Client, value string) (*apimodel.HarnessDefinition, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("harness definition name is required")
	}
	res, err := client.ListHarnessDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListHarnessDefinitionsBody](res)
	if err != nil {
		return nil, err
	}
	var nameMatches []apimodel.HarnessDefinition
	var ids []string
	definitions := body.GetHarnessDefinitions()
	for _, definition := range definitions {
		ids = append(ids, definition.ID)
		if definition.Name == value {
			matched := definition
			return &matched, nil
		}
		if strings.EqualFold(definition.Name, value) {
			nameMatches = append(nameMatches, definition)
		}
	}
	if len(nameMatches) == 1 {
		return &nameMatches[0], nil
	}
	if len(nameMatches) > 1 {
		return nil, fmt.Errorf("harness definition name %q is ambiguous", value)
	}
	definitionID, err := resolveShortID(value, "harness definition ID", ids)
	if err != nil {
		return nil, err
	}
	for _, definition := range definitions {
		if definition.ID == definitionID {
			matched := definition
			return &matched, nil
		}
	}
	return nil, fmt.Errorf("harness definition %q not found", value)
}

func (a *App) harnessConfigBySlug(ctx context.Context, client *apiclientgen.Client, projectID, slug string) (*apimodel.HarnessConfig, error) {
	harnesses, err := a.listHarnessConfigs(ctx, client, projectID)
	if err != nil {
		return nil, err
	}
	return harnessConfigBySlug(harnesses, slug), nil
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

func harnessConfigBySlug(harnesses []apimodel.HarnessConfig, slug string) *apimodel.HarnessConfig {
	for _, harness := range harnesses {
		if harness.Slug == slug {
			matched := harness
			return &matched
		}
	}
	return nil
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
	body.SetDefinitionId(optString(opts.definitionID))
	if len(opts.installCommand) > 0 {
		body.SetInstallCommand(apiclientgen.NewOptNilStringArray(opts.installCommand))
	}
	if len(opts.runCommand) > 0 {
		body.SetRunCommand(apiclientgen.NewOptNilStringArray(opts.runCommand))
	}
	if len(opts.relaunchCommand) > 0 {
		body.SetRelaunchCommand(apiclientgen.NewOptNilStringArray(opts.relaunchCommand))
	}
	if len(opts.files) > 0 {
		files, err := parseHarnessFileFlags(opts.files, opts.createOnlyFile)
		if err != nil {
			return nil, err
		}
		body.SetFiles(apiclientgen.NewOptNilHarnessConfigFileArray(files))
	}
	if len(opts.requiredSecrets) > 0 || len(opts.optionalSecrets) > 0 {
		body.SetSecrets(apiclientgen.NewOptNilHarnessConfigSecretArray(parseHarnessSecretFlags(opts.requiredSecrets, opts.optionalSecrets)))
	}
	return body, nil
}

func updateHarnessBody(cmd *cobra.Command, opts harnessUpdateOptions) (*apimodel.UpdateHarnessConfigBody, error) {
	body := &apimodel.UpdateHarnessConfigBody{}
	if cmd.Flags().Changed("name") {
		body.SetName(apiclientgen.NewOptString(opts.name))
	}
	if cmd.Flags().Changed("install-command") {
		body.SetInstallCommand(apiclientgen.NewOptNilStringArray(opts.installCommand))
	}
	if cmd.Flags().Changed("run-command") {
		body.SetRunCommand(apiclientgen.NewOptNilStringArray(opts.runCommand))
	}
	if cmd.Flags().Changed("relaunch-command") {
		body.SetRelaunchCommand(apiclientgen.NewOptNilStringArray(opts.relaunchCommand))
	}
	if cmd.Flags().Changed("file") {
		files, err := parseHarnessFileFlags(opts.files, opts.createOnlyFile)
		if err != nil {
			return nil, err
		}
		body.SetFiles(apiclientgen.NewOptNilHarnessConfigFileArray(files))
	}
	if cmd.Flags().Changed("required-secret") || cmd.Flags().Changed("optional-secret") {
		body.SetSecrets(apiclientgen.NewOptNilHarnessConfigSecretArray(parseHarnessSecretFlags(opts.requiredSecrets, opts.optionalSecrets)))
	}
	return body, nil
}

func parseHarnessSecretFlags(required, optional []string) []apimodel.HarnessConfigSecret {
	secrets := make([]apimodel.HarnessConfigSecret, 0, len(required)+len(optional))
	for _, name := range required {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		secrets = append(secrets, apimodel.HarnessConfigSecret{Name: name, Required: apiclientgen.NewOptBool(true)})
	}
	for _, name := range optional {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		secrets = append(secrets, apimodel.HarnessConfigSecret{Name: name})
	}
	return secrets
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
