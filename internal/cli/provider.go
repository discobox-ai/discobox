package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/go-faster/jx"
	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/internal/apiclient/gen"
)

type providerCreateOptions struct {
	name            string
	providerType    string
	config          string
	doToken         string
	doTokenEnv      string
	controlPlaneURL string
	apiBaseURL      string
	region          string
	size            string
	image           string
	sshKeys         string
	tags            string
	poolSize        int
	minWorkers      int
	maxWorkers      int
	minHealthy      int
}

type providerUpdateOptions struct {
	providerCreateOptions
	disabled bool
}

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
	return &cobra.Command{Use: "catalog", Short: "List available provider types", RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		body, err := client.ListSandboxProviderCatalog(cmd.Context())
		if err != nil {
			return err
		}
		return a.writeProviderCatalog(cmd, body.GetProviders())
	}}
}

func (a *App) newProviderListCommand() *cobra.Command {
	return &cobra.Command{Use: "list", Short: "List provider instances", RunE: func(cmd *cobra.Command, _ []string) error {
		projectID, err := a.projectUUID()
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		body, err := client.ListSandboxProviderInstances(cmd.Context(), apiclientgen.ListSandboxProviderInstancesParams{ProjectId: projectID})
		if err != nil {
			return err
		}
		return a.writeProviders(cmd, body.GetProviders())
	}}
}

func (a *App) newProviderGetCommand() *cobra.Command {
	return &cobra.Command{Use: "get PROVIDER_ID", Short: "Get a provider instance", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := a.projectUUID()
		if err != nil {
			return err
		}
		providerID, err := parseUUIDArg(args[0], "provider ID")
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		provider, err := client.GetSandboxProviderInstance(cmd.Context(), apiclientgen.GetSandboxProviderInstanceParams{ProjectId: projectID, ProviderId: providerID})
		if err != nil {
			return err
		}
		return a.writeProvider(cmd, provider)
	}}
}

func (a *App) newProviderCreateCommand() *cobra.Command {
	helpFlag := &providerCreateHelpFlag{}
	cmd := &cobra.Command{
		Use:   "create --type PROVIDER --name NAME",
		Short: "Create a provider instance and bootstrap its warm worker pool",
		Long: `Create a provider instance and bootstrap its warm worker pool.

Provider-specific flags are loaded from the server catalog only when this
subcommand runs. Use --help=<provider> to show provider-specific options.

Examples:
  discobox provider create --help
  discobox provider create --help=digitalocean
  discobox provider create --type digitalocean --name do --control-plane-url https://example.com --token-env DIGITALOCEAN_ACCESS_TOKEN`,
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
	var opts providerUpdateOptions
	cmd := &cobra.Command{Use: "update PROVIDER_ID", Short: "Update a provider instance", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := a.projectUUID()
		if err != nil {
			return err
		}
		providerID, err := parseUUIDArg(args[0], "provider ID")
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		current, err := client.GetSandboxProviderInstance(cmd.Context(), apiclientgen.GetSandboxProviderInstanceParams{ProjectId: projectID, ProviderId: providerID})
		if err != nil {
			return err
		}
		body, err := providerUpdateBody(cmd, opts, current)
		if err != nil {
			return err
		}
		provider, err := client.UpdateSandboxProviderInstance(cmd.Context(), body, apiclientgen.UpdateSandboxProviderInstanceParams{ProjectId: projectID, ProviderId: providerID})
		if err != nil {
			return err
		}
		return a.writeProvider(cmd, provider)
	}}
	addProviderConfigFlags(cmd, &opts.providerCreateOptions)
	cmd.Flags().BoolVar(&opts.disabled, "disabled", false, "Disable or enable the provider instance")
	return cmd
}

func (a *App) newProviderDeleteCommand() *cobra.Command {
	return &cobra.Command{Use: "delete PROVIDER_ID", Short: "Delete a provider instance", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		projectID, err := a.projectUUID()
		if err != nil {
			return err
		}
		providerID, err := parseUUIDArg(args[0], "provider ID")
		if err != nil {
			return err
		}
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		if err := client.DeleteSandboxProviderInstance(cmd.Context(), apiclientgen.DeleteSandboxProviderInstanceParams{ProjectId: projectID, ProviderId: providerID}); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "deleted")
		return nil
	}}
}

func addProviderConfigFlags(cmd *cobra.Command, opts *providerCreateOptions) {
	cmd.Flags().StringVar(&opts.providerType, "type", "digitalocean", "Provider type")
	cmd.Flags().StringVar(&opts.name, "name", "", "Provider instance name")
	cmd.Flags().StringVar(&opts.config, "config", "", "Provider config JSON or @path")
	cmd.Flags().StringVar(&opts.doToken, "do-token", "", "DigitalOcean API token (prefer --do-token-env)")
	cmd.Flags().StringVar(&opts.doTokenEnv, "do-token-env", "DIGITALOCEAN_ACCESS_TOKEN", "Environment variable containing the DigitalOcean token")
	cmd.Flags().StringVar(&opts.controlPlaneURL, "control-plane-url", "", "Public control plane URL for worker registration")
	cmd.Flags().StringVar(&opts.apiBaseURL, "do-api-base-url", "", "DigitalOcean API base URL (for tests/private endpoints)")
	cmd.Flags().StringVar(&opts.region, "do-region", "", "DigitalOcean region")
	cmd.Flags().StringVar(&opts.size, "do-size", "", "DigitalOcean droplet size")
	cmd.Flags().StringVar(&opts.image, "do-image", "", "DigitalOcean image slug")
	cmd.Flags().StringVar(&opts.sshKeys, "do-ssh-keys", "", "Comma-separated DigitalOcean SSH key IDs/fingerprints")
	cmd.Flags().StringVar(&opts.tags, "do-tags", "", "Comma-separated DigitalOcean droplet tags")
	cmd.Flags().IntVar(&opts.poolSize, "pool-size", 1, "Warm worker pool size")
	cmd.Flags().IntVar(&opts.minWorkers, "min-workers", 0, "Minimum active warm workers")
	cmd.Flags().IntVar(&opts.maxWorkers, "max-workers", 0, "Maximum active warm workers")
	cmd.Flags().IntVar(&opts.minHealthy, "min-healthy-workers", 0, "Minimum ready, schedulable, non-degraded warm workers")
}

func providerUpdateBody(cmd *cobra.Command, opts providerUpdateOptions, current *apiclientgen.SandboxProviderInstance) (*apiclientgen.UpdateSandboxProviderInstanceBody, error) {
	body := &apiclientgen.UpdateSandboxProviderInstanceBody{}
	if cmd.Flags().Changed("name") {
		body.SetName(apiclientgen.NewOptString(opts.name))
	}
	if cmd.Flags().Changed("disabled") {
		body.SetDisabled(apiclientgen.NewOptBool(opts.disabled))
	}
	if providerConfigChanged(cmd) {
		raw, err := providerUpdateConfig(cmd, opts, current)
		if err != nil {
			return nil, err
		}
		body.SetConfig(raw)
	}
	return body, nil
}

func providerUpdateConfig(cmd *cobra.Command, opts providerUpdateOptions, current *apiclientgen.SandboxProviderInstance) (jx.Raw, error) {
	if cmd.Flags().Changed("config") {
		return rawJSON(opts.config)
	}
	m := map[string]any{}
	if current != nil && len(current.GetConfig()) > 0 {
		_ = json.Unmarshal(current.GetConfig(), &m)
	}
	if cmd.Flags().Changed("do-token") {
		m["token"] = opts.doToken
	}
	if cmd.Flags().Changed("do-token-env") {
		m["tokenEnv"] = opts.doTokenEnv
	}
	if cmd.Flags().Changed("control-plane-url") {
		m["controlPlaneUrl"] = opts.controlPlaneURL
	}
	if cmd.Flags().Changed("do-api-base-url") {
		m["apiBaseUrl"] = opts.apiBaseURL
	}
	if cmd.Flags().Changed("do-region") {
		m["region"] = opts.region
	}
	if cmd.Flags().Changed("do-size") {
		m["size"] = opts.size
	}
	if cmd.Flags().Changed("do-image") {
		m["image"] = opts.image
	}
	if cmd.Flags().Changed("do-ssh-keys") {
		m["sshKeys"] = splitCSV(opts.sshKeys)
	}
	if cmd.Flags().Changed("do-tags") {
		m["tags"] = splitCSV(opts.tags)
	}
	if cmd.Flags().Changed("pool-size") {
		m["poolSize"] = opts.poolSize
	}
	if cmd.Flags().Changed("min-workers") {
		m["minWorkers"] = opts.minWorkers
	}
	if cmd.Flags().Changed("max-workers") {
		m["maxWorkers"] = opts.maxWorkers
	}
	if cmd.Flags().Changed("min-healthy-workers") {
		m["minHealthyWorkers"] = opts.minHealthy
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return jx.Raw(data), nil
}

func providerConfigChanged(cmd *cobra.Command) bool {
	for _, name := range []string{"config", "do-token", "do-token-env", "control-plane-url", "do-api-base-url", "do-region", "do-size", "do-image", "do-ssh-keys", "do-tags", "pool-size", "min-workers", "max-workers", "min-healthy-workers"} {
		if cmd.Flags().Changed(name) {
			return true
		}
	}
	return false
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
