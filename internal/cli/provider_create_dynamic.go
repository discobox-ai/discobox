package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/go-faster/jx"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	apiclientgen "github.com/obot-platform/discobox/internal/apiclient/gen"
)

type dynamicProviderCreateOptions struct {
	Name         string
	ProviderType string
	Config       string
	Flags        *pflag.FlagSet
}

type providerCreateHelpFlag struct {
	provider string
	set      bool
}

func (f *providerCreateHelpFlag) Set(value string) error {
	f.set = true
	if value == "true" {
		f.provider = ""
	} else {
		f.provider = strings.TrimSpace(value)
	}
	return nil
}

func (f *providerCreateHelpFlag) String() string {
	return strconv.FormatBool(f.set)
}

func (f *providerCreateHelpFlag) Type() string { return "bool" }

func (f *providerCreateHelpFlag) IsBoolFlag() bool { return true }

func (f *providerCreateHelpFlag) Provider() string { return f.provider }

func (a *App) runProviderCreate(cmd *cobra.Command, args []string) error {
	args = a.consumeProviderCreateGlobalFlags(args)
	args = providerCreateArgs(cmd, args)
	if helpProvider, ok := providerCreateHelpRequest(nil, args); ok {
		if helpProvider == "" {
			writeProviderCreateStaticHelp(cmd)
			return nil
		}
		return a.writeProviderCreateHelpForProvider(cmd, helpProvider)
	}
	providerType := providerTypeFromArgs(args)
	client, err := a.apiClient()
	if err != nil {
		return err
	}
	catalog, err := client.ListSandboxProviderCatalog(cmd.Context())
	if err != nil {
		return err
	}
	provider, ok := findProviderCatalogItem(catalog.GetProviders(), providerType)
	if !ok {
		return fmt.Errorf("provider type %q not found", providerType)
	}
	opts, err := parseDynamicProviderCreateArgs(cmd, args, provider)
	if err != nil {
		return err
	}
	projectID, err := a.projectIDValue()
	if err != nil {
		return err
	}
	body, err := dynamicProviderCreateBody(opts, provider)
	if err != nil {
		return err
	}
	created, err := client.CreateSandboxProviderInstance(cmd.Context(), body, apiclientgen.CreateSandboxProviderInstanceParams{ProjectId: projectID})
	if err != nil {
		return err
	}
	return a.writeProvider(cmd, created)
}

func (a *App) consumeProviderCreateGlobalFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--server" && i+1 < len(args):
			a.serverURL = args[i+1]
			i++
		case strings.HasPrefix(arg, "--server="):
			a.serverURL = strings.TrimPrefix(arg, "--server=")
		case arg == "--project" || arg == "-p":
			if i+1 < len(args) {
				a.projectID = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--project="):
			a.projectID = strings.TrimPrefix(arg, "--project=")
		case arg == "--token" && i+1 < len(args):
			a.token = args[i+1]
			i++
		case strings.HasPrefix(arg, "--token="):
			a.token = strings.TrimPrefix(arg, "--token=")
		case arg == "--output" || arg == "-o":
			if i+1 < len(args) {
				a.output = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--output="):
			a.output = strings.TrimPrefix(arg, "--output=")
		case arg == "--debug":
			a.debug = true
		case strings.HasPrefix(arg, "--debug="):
			value, err := strconv.ParseBool(strings.TrimPrefix(arg, "--debug="))
			if err == nil {
				a.debug = value
			}
		default:
			out = append(out, arg)
		}
	}
	return out
}

func providerCreateArgs(cmd *cobra.Command, args []string) []string {
	out := append([]string(nil), args...)
	if cmd.Flags().Changed("type") {
		value, _ := cmd.Flags().GetString("type")
		out = append([]string{"--type", value}, out...)
	}
	if cmd.Flags().Changed("name") {
		value, _ := cmd.Flags().GetString("name")
		out = append([]string{"--name", value}, out...)
	}
	if cmd.Flags().Changed("config") {
		value, _ := cmd.Flags().GetString("config")
		out = append([]string{"--config", value}, out...)
	}
	return out
}

func providerCreateHelpRequest(cmd *cobra.Command, args []string) (string, bool) {
	if cmd != nil && cmd.Flags().Changed("help") {
		value, _ := cmd.Flags().GetString("help")
		if value == "true" || value == "" {
			return "", true
		}
		return strings.TrimSpace(value), true
	}
	for i, arg := range args {
		switch {
		case arg == "--help" || arg == "-h":
			return "", true
		case strings.HasPrefix(arg, "--help="):
			return strings.TrimSpace(strings.TrimPrefix(arg, "--help=")), true
		case arg == "--help-provider" && i+1 < len(args):
			return strings.TrimSpace(args[i+1]), true
		case strings.HasPrefix(arg, "--help-provider="):
			return strings.TrimSpace(strings.TrimPrefix(arg, "--help-provider=")), true
		}
	}
	return "", false
}

func providerTypeFromArgs(args []string) string {
	for i, arg := range args {
		if arg == "--type" && i+1 < len(args) {
			return strings.TrimSpace(args[i+1])
		}
		if strings.HasPrefix(arg, "--type=") {
			return strings.TrimSpace(strings.TrimPrefix(arg, "--type="))
		}
	}
	return "digitalocean"
}

func parseDynamicProviderCreateArgs(cmd *cobra.Command, args []string, provider apiclientgen.SandboxProviderCatalogItem) (dynamicProviderCreateOptions, error) {
	opts := dynamicProviderCreateOptions{ProviderType: provider.ID}
	flags := providerCreateFlagSet(cmd, provider, &opts)
	opts.Flags = flags
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if strings.TrimSpace(opts.ProviderType) == "" {
		opts.ProviderType = provider.ID
	}
	if opts.ProviderType != provider.ID {
		return opts, fmt.Errorf("provider type changed during parsing: %q != %q", opts.ProviderType, provider.ID)
	}
	return opts, nil
}

func providerCreateFlagSet(cmd *cobra.Command, provider apiclientgen.SandboxProviderCatalogItem, opts *dynamicProviderCreateOptions) *pflag.FlagSet {
	flags := pflag.NewFlagSet(cmd.Name(), pflag.ContinueOnError)
	flags.SetOutput(cmd.ErrOrStderr())
	flags.StringVar(&opts.ProviderType, "type", provider.ID, "Provider type")
	flags.StringVar(&opts.Name, "name", "", "Provider instance name")
	flags.StringVar(&opts.Config, "config", "", "Provider config JSON or @path; cannot be combined with provider-specific flags")
	for _, field := range sortedProviderConfigFields(provider) {
		flagName := providerFieldFlagName(field.Key)
		description := providerFieldDescription(field)
		flags.String(flagName, "", description)
		if strings.EqualFold(field.Type, "boolean") || strings.EqualFold(field.Type, "bool") {
			flags.Lookup(flagName).NoOptDefVal = "true"
		}
	}
	return flags
}

func dynamicProviderCreateBody(opts dynamicProviderCreateOptions, provider apiclientgen.SandboxProviderCatalogItem) (*apiclientgen.CreateSandboxProviderInstanceBody, error) {
	body := &apiclientgen.CreateSandboxProviderInstanceBody{Name: opts.Name, Type: opts.ProviderType}
	if opts.Flags == nil {
		return body, nil
	}
	config := strings.TrimSpace(opts.Config)
	if config != "" {
		for _, field := range sortedProviderConfigFields(provider) {
			if opts.Flags.Changed(providerFieldFlagName(field.Key)) {
				return nil, fmt.Errorf("--config cannot be combined with provider-specific flags")
			}
		}
		raw, err := rawJSON(config)
		if err != nil {
			return nil, err
		}
		body.SetConfig(raw)
		return body, nil
	}
	raw, err := dynamicProviderCreateConfig(opts.Flags, provider)
	if err != nil {
		return nil, err
	}
	body.SetConfig(raw)
	return body, nil
}

func dynamicProviderCreateConfig(flags *pflag.FlagSet, provider apiclientgen.SandboxProviderCatalogItem) (jx.Raw, error) {
	m := map[string]any{}
	for _, field := range sortedProviderConfigFields(provider) {
		flagName := providerFieldFlagName(field.Key)
		if !flags.Changed(flagName) {
			if field.Required.Or(false) {
				return nil, fmt.Errorf("required flag --%s not set", flagName)
			}
			continue
		}
		value, err := flags.GetString(flagName)
		if err != nil {
			return nil, err
		}
		converted, err := providerFieldValue(field, value)
		if err != nil {
			return nil, fmt.Errorf("--%s: %w", flagName, err)
		}
		m[field.Key] = converted
	}
	if len(m) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return jx.Raw(data), nil
}

func (a *App) writeProviderCreateHelpForProvider(cmd *cobra.Command, providerType string) error {
	client, err := a.apiClient()
	if err != nil {
		return err
	}
	catalog, err := client.ListSandboxProviderCatalog(cmd.Context())
	if err != nil {
		return err
	}
	provider, ok := findProviderCatalogItem(catalog.GetProviders(), providerType)
	if !ok {
		return fmt.Errorf("provider type %q not found", providerType)
	}
	return writeProviderCreateHelp(cmd.OutOrStdout(), provider)
}

func writeProviderCreateStaticHelp(cmd *cobra.Command) {
	fmt.Fprintln(cmd.OutOrStdout(), cmd.Long)
	fmt.Fprintln(cmd.OutOrStdout())
	fmt.Fprintln(cmd.OutOrStdout(), "Provider discovery:")
	fmt.Fprintln(cmd.OutOrStdout(), "      discobox provider catalog          List available provider types")
	fmt.Fprintln(cmd.OutOrStdout(), "      discobox provider create --help=PROVIDER")
	fmt.Fprintln(cmd.OutOrStdout(), "                                      Show provider-specific create flags")
	fmt.Fprintln(cmd.OutOrStdout())
	fmt.Fprintln(cmd.OutOrStdout(), "Common Flags:")
	fmt.Fprintln(cmd.OutOrStdout(), "      --name string       Provider instance name")
	fmt.Fprintln(cmd.OutOrStdout(), "      --type string       Provider type (default \"digitalocean\")")
	fmt.Fprintln(cmd.OutOrStdout(), "      --config string     Provider config JSON or @path")
	fmt.Fprintln(cmd.OutOrStdout(), "      --help              Show this help without contacting the API server")
	fmt.Fprintln(cmd.OutOrStdout(), "      --help=PROVIDER     Load catalog and show provider-specific flags")
}

func writeProviderCreateHelp(w io.Writer, provider apiclientgen.SandboxProviderCatalogItem) error {
	fmt.Fprintf(w, "Create a %s provider instance\n\n", provider.Name)
	if description, ok := provider.Description.Get(); ok && strings.TrimSpace(description) != "" {
		fmt.Fprintf(w, "%s\n\n", description)
	}
	fmt.Fprintf(w, "Usage:\n  discobox provider create --type %s [provider flags]\n\n", provider.ID)
	fmt.Fprintln(w, "Common Flags:")
	fmt.Fprintln(w, "      --name string     Provider instance name")
	fmt.Fprintf(w, "      --type string     Provider type (default %q)\n", provider.ID)
	fmt.Fprintln(w, "      --config string   Provider config JSON or @path; cannot be combined with provider-specific flags")
	fields := sortedProviderConfigFields(provider)
	if len(fields) > 0 {
		fmt.Fprintln(w, "\nProvider Flags:")
		for _, field := range fields {
			required := ""
			if field.Required.Or(false) {
				required = " (required)"
			}
			placeholder := ""
			if value, ok := field.Placeholder.Get(); ok && strings.TrimSpace(value) != "" {
				placeholder = fmt.Sprintf(" default/example: %s", value)
			}
			description := field.Description.Or(field.Label)
			advanced := ""
			if field.Advanced.Or(false) {
				advanced = " [advanced]"
			}
			fmt.Fprintf(w, "      --%-22s %s%s%s%s\n", providerFieldFlagName(field.Key)+" string", description, required, placeholder, advanced)
		}
	}
	return nil
}

func findProviderCatalogItem(providers []apiclientgen.SandboxProviderCatalogItem, providerType string) (apiclientgen.SandboxProviderCatalogItem, bool) {
	for _, provider := range providers {
		if provider.ID == providerType {
			return provider, true
		}
	}
	return apiclientgen.SandboxProviderCatalogItem{}, false
}

func sortedProviderConfigFields(provider apiclientgen.SandboxProviderCatalogItem) []apiclientgen.ProviderConfigField {
	fields := append([]apiclientgen.ProviderConfigField(nil), provider.ConfigFields.Or(nil)...)
	sort.SliceStable(fields, func(i, j int) bool {
		if fields[i].Required.Or(false) != fields[j].Required.Or(false) {
			return fields[i].Required.Or(false)
		}
		if fields[i].Advanced.Or(false) != fields[j].Advanced.Or(false) {
			return !fields[i].Advanced.Or(false)
		}
		return fields[i].Key < fields[j].Key
	})
	return fields
}

func providerFieldFlagName(key string) string {
	var out strings.Builder
	for i, r := range key {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				out.WriteByte('-')
			}
			out.WriteRune(r + ('a' - 'A'))
			continue
		}
		if r == '_' || r == ' ' {
			out.WriteByte('-')
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

func providerFieldDescription(field apiclientgen.ProviderConfigField) string {
	description := field.Description.Or(field.Label)
	parts := []string{description}
	if field.Required.Or(false) {
		parts = append(parts, "required")
	}
	if placeholder, ok := field.Placeholder.Get(); ok && strings.TrimSpace(placeholder) != "" {
		parts = append(parts, "default/example: "+placeholder)
	}
	if field.Advanced.Or(false) {
		parts = append(parts, "advanced")
	}
	return strings.Join(parts, "; ")
}

func providerFieldValue(field apiclientgen.ProviderConfigField, value string) (any, error) {
	switch strings.ToLower(field.Type) {
	case "boolean", "bool":
		return strconv.ParseBool(value)
	case "number", "integer", "int":
		if strings.ContainsAny(value, ".eE") {
			return strconv.ParseFloat(value, 64)
		}
		return strconv.Atoi(value)
	case "array", "list", "strings":
		return splitCSV(value), nil
	default:
		return value, nil
	}
}
