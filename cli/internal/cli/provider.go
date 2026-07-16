package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/go-faster/jx"
	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

func (a *App) newProviderCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "provider", Aliases: []string{"providers"}, Short: "Manage sandbox provider instances"}
	cmd.AddCommand(a.newProviderCatalogCommand())
	cmd.AddCommand(a.newProviderListCommand())
	cmd.AddCommand(a.newProviderGetCommand())
	cmd.AddCommand(a.newProviderCreateCommand())
	cmd.AddCommand(a.newProviderUpdateCommand())
	cmd.AddCommand(a.newProviderDeleteCommand())
	return cmd
}

func (a *App) newProviderCatalogCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "catalog", Short: "List available provider types", RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		bodyRes, err := client.ListSandboxProviderCatalog(cmd.Context())
		if err != nil {
			return err
		}
		body, err := expectResponse[apimodel.ListSandboxProviderCatalogBody](bodyRes)
		if err != nil {
			return err
		}
		return a.writeProviderCatalog(cmd, body.GetProviders())
	}}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newProviderListCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "ls", Aliases: []string{"list"}, Short: "List provider instances", RunE: func(cmd *cobra.Command, _ []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		bodyRes, err := client.ListSandboxProviderInstances(cmd.Context(), apiclientgen.ListSandboxProviderInstancesParams{ProjectId: projectID})
		if err != nil {
			return err
		}
		body, err := expectResponse[apimodel.ListSandboxProviderInstancesBody](bodyRes)
		if err != nil {
			return err
		}
		return a.writeProviders(cmd, body.GetProviders())
	}}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newProviderGetCommand() *cobra.Command {
	return &cobra.Command{Use: "get PROVIDER_ID", Short: "Get a provider instance", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeProviders, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		providerID, err := a.resolveProviderID(cmd.Context(), client, projectID, args[0])
		if err != nil {
			return err
		}
		providerRes, err := client.GetSandboxProviderInstance(cmd.Context(), apiclientgen.GetSandboxProviderInstanceParams{ProjectId: projectID, ProviderId: providerID})
		if err != nil {
			return err
		}
		provider, err := expectResponse[apimodel.SandboxProviderInstance](providerRes)
		if err != nil {
			return err
		}
		return a.writeProvider(cmd, provider)
	}}
}

func (a *App) newProviderCreateCommand() *cobra.Command {
	helpFlag := &providerCreateHelpFlag{}
	cmd := &cobra.Command{
		Use:   "create --type PROVIDER",
		Short: "Create a provider instance and bootstrap its warm worker pool",
		Long: `Create a provider instance and bootstrap its warm worker pool.

Provider-specific flags are loaded from the server catalog only when this
subcommand runs. Use --help=<provider> to show provider-specific options.

Examples:
  disco box provider create --help
  disco box provider create --help=digitalocean
  disco box provider create --type digitalocean --control-plane-url https://example.com --token-env DIGITALOCEAN_ACCESS_TOKEN`,
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
		DisableFlagParsing: true,
		RunE:               a.runProviderCreate,
	}
	cmd.Flags().Var(helpFlag, "help", "Show create help; use --help=PROVIDER for provider-specific flags")
	cmd.Flags().Lookup("help").NoOptDefVal = "true"
	cmd.Flags().String("type", "digitalocean", "Provider type")
	cmd.Flags().String("name", "", "Provider instance name")
	cmd.Flags().String("config", "", "Provider config JSON or @path")
	cmd.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		if provider := strings.TrimSpace(helpFlag.Provider()); provider != "" {
			if err := a.writeProviderCreateHelpForProvider(cmd, provider); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), err)
			}
			return
		}
		writeProviderCreateStaticHelp(cmd)
	})
	return cmd
}

func (a *App) newProviderUpdateCommand() *cobra.Command {
	helpFlag := &providerCreateHelpFlag{}
	cmd := &cobra.Command{
		Use:   "update PROVIDER_ID",
		Short: "Update a provider instance",
		Long: `Update a provider instance.

Provider-specific flags are loaded from the server catalog after the current
provider instance is loaded. Use --help=<provider> to show provider-specific
options.

Examples:
  disco box provider update --help
  disco box provider update --help=docker
  disco box provider update my-provider --min-workers 1 --max-workers 2`,
		FParseErrWhitelist: cobra.FParseErrWhitelist{UnknownFlags: true},
		DisableFlagParsing: true,
		ValidArgsFunction:  a.completeProviders,
		RunE:               a.runProviderUpdate,
	}
	cmd.Flags().Var(helpFlag, "help", "Show update help; use --help=PROVIDER for provider-specific flags")
	cmd.Flags().Lookup("help").NoOptDefVal = "true"
	cmd.Flags().String("name", "", "Provider instance name")
	cmd.Flags().String("config", "", "Provider config JSON or @path")
	cmd.Flags().Bool("disabled", false, "Disable or enable the provider instance")
	cmd.SetHelpFunc(func(cmd *cobra.Command, _ []string) {
		if provider := strings.TrimSpace(helpFlag.Provider()); provider != "" {
			if err := a.writeProviderUpdateHelpForProvider(cmd, provider); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), err)
			}
			return
		}
		writeProviderUpdateStaticHelp(cmd)
	})
	return cmd
}

func (a *App) newProviderDeleteCommand() *cobra.Command {
	return &cobra.Command{Use: "delete PROVIDER_ID...", Short: "Delete provider instances", Args: cobra.MinimumNArgs(1), ValidArgsFunction: a.completeProviders, RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		return runDeleteMany(cmd, args, "provider", func(arg string) (string, error) {
			providerID, err := a.resolveProviderID(cmd.Context(), client, projectID, arg)
			if err != nil {
				return "", err
			}
			res, err := client.DeleteSandboxProviderInstance(cmd.Context(), apiclientgen.DeleteSandboxProviderInstanceParams{ProjectId: projectID, ProviderId: providerID})
			if err != nil {
				return "", err
			}
			if err := expectNoContent[apiclientgen.DeleteSandboxProviderInstanceNoContent](res); err != nil {
				return "", err
			}
			return providerID, nil
		})
	}}
}

func rawJSON(value string) (jx.Raw, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}
	if strings.HasPrefix(value, "@") {
		data, err := os.ReadFile(strings.TrimPrefix(value, "@"))
		if err != nil {
			return nil, err
		}
		value = string(data)
	}
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return nil, fmt.Errorf("config must be valid JSON: %w", err)
	}
	return jx.Raw(raw), nil
}

func splitCSV(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
