package cli

import (
	"context"
	"errors"
	"strings"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/id"
	"github.com/spf13/cobra"
)

type runOptions struct {
	source string
	ref    string
	prompt []string
	env    []string
}

func (a *App) newRunCommand() *cobra.Command {
	var opts runOptions
	cmd := &cobra.Command{
		Use:   "run [flags] SOURCE[@REF] [PROMPT...]",
		Short: "Run a prompt against a local directory or Git repository",
		Long: `Run a prompt against a local directory or Git repository.

The run command is intentionally shaped like docker run: command flags come first,
then the source directory or Git repository, followed by the prompt. Use -- when
the source or prompt needs to be separated from command flags explicitly.`,
		Example: `  discobox run . fix the failing tests
  discobox run https://github.com/obot-platform/discobox.git@main summarize the CLI package
  discobox run -e GITHUB_TOKEN -e MODE=test . fix the failing tests
  discobox run -- . prompt starting with --flag-like text`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			parsedOpts, err := parseRunArgs(args)
			if err != nil {
				return err
			}
			parsedOpts.env = append(parsedOpts.env, opts.env...)
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			body, err := createRunSandboxBody(cmd.Context(), parsedOpts)
			if err != nil {
				return err
			}
			sandboxRes, err := client.CreateSandbox(cmd.Context(), body, apiclientgen.CreateSandboxParams{ProjectId: projectID})
			if err != nil {
				return err
			}
			sandbox, err := expectResponse[apimodel.Sandbox](sandboxRes)
			if err != nil {
				return err
			}
			return a.writeSandbox(cmd, sandbox)
		},
	}
	cmd.Flags().StringArrayVarP(&opts.env, "env", "e", nil, "Environment variable as KEY=VALUE or KEY from the local environment; repeat for multiple variables")
	return cmd
}

func parseRunArgs(args []string) (runOptions, error) {
	if len(args) == 0 {
		return runOptions{}, errors.New("source directory or Git repository is required")
	}
	opts := runOptions{
		source: args[0],
		prompt: append([]string(nil), args[1:]...),
	}
	if opts.source == "" {
		return runOptions{}, errors.New("source directory or Git repository is required")
	}
	if source, ref, ok := splitRunSourceRef(opts.source); ok {
		opts.source = source
		opts.ref = ref
	}
	return opts, nil
}

func splitRunSourceRef(value string) (string, string, bool) {
	at := strings.LastIndex(value, "@")
	if at <= 0 || at == len(value)-1 {
		return value, "", false
	}
	if !strings.Contains(value[:at], "@") && strings.Contains(value[at+1:], ":") {
		return value, "", false
	}
	return value[:at], value[at+1:], true
}

func createRunSandboxBody(ctx context.Context, opts runOptions) (*apimodel.CreateSandboxBody, error) {
	runID, err := id.New()
	if err != nil {
		return nil, err
	}
	body := &apimodel.CreateSandboxBody{Config: apimodel.SandboxCreateConfig{Name: "run-" + shortID(runID)}}
	body.Config.SetPrompt(optString(strings.Join(opts.prompt, " ")))
	env, err := keyValueMapFromShell(opts.env)
	if err != nil {
		return nil, err
	}
	if len(env) > 0 {
		body.Config.SetEnv(apiclientgen.NewOptSandboxCreateConfigEnv(apiclientgen.SandboxCreateConfigEnv(env)))
	}
	userIdentity, _, err := resolveRunUserIdentity()
	if err != nil {
		return nil, err
	}
	sourceArg := opts.source
	if opts.ref != "" {
		sourceArg += "@" + opts.ref
	}
	source, err := resolveRunSource(ctx, sourceArg)
	if err != nil {
		return nil, err
	}
	apiSource, err := source.apiGitSource()
	if err != nil {
		return nil, err
	}
	body.Config.SetSource(apiclientgen.NewOptGitSource(*apiSource))
	userIdentity.setCreateSandboxUser(body)
	return body, nil
}
