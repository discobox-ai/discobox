package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/go-faster/jx"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

type dynamicProviderCreateOptions struct {
	Name         string
	ProviderType string
	Config       string
	Secrets      []string
	Stdin        io.Reader
	Flags        *pflag.FlagSet
}

type dynamicProviderUpdateOptions struct {
	ProviderID string
	Name       string
	Config     string
	Secrets    []string
	Disabled   bool
	Stdin      io.Reader
	Flags      *pflag.FlagSet
}

// secretFlagUsage describes --secret, the one way a secret field's value is
// given: from a file or stdin, never on the command line, where ps shows it
// (ADR 26-10-31-694 §2).
const secretFlagUsage = "Secret field from a file or stdin, as KEY=@path or KEY=- (repeatable); never given on the command line" //nolint:gosec // Flag help text, not a credential.

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
	// The root's hook settled the primary already, so what is left here is
	// either that address or a --server given after the command name.
	if err := a.resolveServerName(true); err != nil {
		return err
	}
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
	catalogRes, err := client.ListSandboxProviderCatalog(cmd.Context())
	if err != nil {
		return err
	}
	catalog, err := expectResponse[apimodel.ListSandboxProviderCatalogBody](catalogRes)
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
	createdRes, err := client.CreateSandboxProviderInstance(cmd.Context(), body, apiclientgen.CreateSandboxProviderInstanceParams{ProjectId: projectID})
	if err != nil {
		return err
	}
	created, err := expectResponse[apimodel.SandboxProviderInstance](createdRes)
	if err != nil {
		return err
	}
	return a.writeProvider(cmd, created)
}

func (a *App) runProviderUpdate(cmd *cobra.Command, args []string) error {
	args = a.consumeProviderCreateGlobalFlags(args)
	// The root's hook settled the primary already, so what is left here is
	// either that address or a --server given after the command name.
	if err := a.resolveServerName(true); err != nil {
		return err
	}
	if helpProvider, ok := providerCreateHelpRequest(nil, args); ok {
		if helpProvider == "" {
			writeProviderUpdateStaticHelp(cmd)
			return nil
		}
		return a.writeProviderUpdateHelpForProvider(cmd, helpProvider)
	}
	providerIDArg, ok := firstProviderUpdateIDArg(args)
	if !ok {
		return fmt.Errorf("provider ID is required")
	}
	projectID, err := a.projectIDValue()
	if err != nil {
		return err
	}
	client, err := a.apiClient()
	if err != nil {
		return err
	}
	providerID, err := a.resolveProviderID(cmd.Context(), client, projectID, providerIDArg)
	if err != nil {
		return err
	}
	currentRes, err := client.GetSandboxProviderInstance(cmd.Context(), apiclientgen.GetSandboxProviderInstanceParams{ProjectId: projectID, ProviderId: providerID})
	if err != nil {
		return err
	}
	current, err := expectResponse[apimodel.SandboxProviderInstance](currentRes)
	if err != nil {
		return err
	}
	catalogRes, err := client.ListSandboxProviderCatalog(cmd.Context())
	if err != nil {
		return err
	}
	catalog, err := expectResponse[apimodel.ListSandboxProviderCatalogBody](catalogRes)
	if err != nil {
		return err
	}
	provider, ok := findProviderCatalogItem(catalog.GetProviders(), current.Type)
	if !ok {
		return fmt.Errorf("provider type %q not found", current.Type)
	}
	opts, err := parseDynamicProviderUpdateArgs(cmd, args, provider)
	if err != nil {
		return err
	}
	body, err := dynamicProviderUpdateBody(opts, provider, current)
	if err != nil {
		return err
	}
	providerRes, err := client.UpdateSandboxProviderInstance(cmd.Context(), body, apiclientgen.UpdateSandboxProviderInstanceParams{ProjectId: projectID, ProviderId: providerID})
	if err != nil {
		return err
	}
	updated, err := expectResponse[apimodel.SandboxProviderInstance](providerRes)
	if err != nil {
		return err
	}
	return a.writeProvider(cmd, updated)
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
		case arg == "--project":
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

func firstProviderUpdateIDArg(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--help" || arg == "-h" || strings.HasPrefix(arg, "--help=") || strings.HasPrefix(arg, "--help-provider=") {
			return "", false
		}
		if strings.HasPrefix(arg, "-") {
			if !strings.Contains(arg, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			continue
		}
		return strings.TrimSpace(arg), strings.TrimSpace(arg) != ""
	}
	return "", false
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

func parseDynamicProviderCreateArgs(cmd *cobra.Command, args []string, provider apimodel.SandboxProviderCatalogItem) (dynamicProviderCreateOptions, error) {
	opts := dynamicProviderCreateOptions{ProviderType: provider.ID, Stdin: cmd.InOrStdin()}
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

func parseDynamicProviderUpdateArgs(cmd *cobra.Command, args []string, provider apimodel.SandboxProviderCatalogItem) (dynamicProviderUpdateOptions, error) {
	opts := dynamicProviderUpdateOptions{Stdin: cmd.InOrStdin()}
	flags := providerUpdateFlagSet(cmd, provider, &opts)
	opts.Flags = flags
	if err := flags.Parse(args); err != nil {
		return opts, err
	}
	if flags.NArg() != 1 {
		return opts, fmt.Errorf("expected exactly one provider ID")
	}
	opts.ProviderID = strings.TrimSpace(flags.Arg(0))
	if opts.ProviderID == "" {
		return opts, fmt.Errorf("provider ID is required")
	}
	return opts, nil
}

func providerCreateFlagSet(cmd *cobra.Command, provider apimodel.SandboxProviderCatalogItem, opts *dynamicProviderCreateOptions) *pflag.FlagSet {
	flags := pflag.NewFlagSet(cmd.Name(), pflag.ContinueOnError)
	flags.SetOutput(cmd.ErrOrStderr())
	flags.StringVar(&opts.ProviderType, "type", provider.ID, "Provider type")
	flags.StringVar(&opts.Name, "name", "", "Provider instance name")
	flags.StringVar(&opts.Config, "config", "", "Provider config JSON or @path; cannot be combined with provider-specific flags")
	flags.StringArrayVar(&opts.Secrets, "secret", nil, secretFlagUsage)
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

func providerUpdateFlagSet(cmd *cobra.Command, provider apimodel.SandboxProviderCatalogItem, opts *dynamicProviderUpdateOptions) *pflag.FlagSet {
	flags := pflag.NewFlagSet(cmd.Name(), pflag.ContinueOnError)
	flags.SetOutput(cmd.ErrOrStderr())
	flags.StringVar(&opts.Name, "name", "", "Provider instance name")
	flags.StringVar(&opts.Config, "config", "", "Provider config JSON or @path; cannot be combined with provider-specific flags")
	flags.BoolVar(&opts.Disabled, "disabled", false, "Disable or enable the provider instance")
	flags.StringArrayVar(&opts.Secrets, "secret", nil, secretFlagUsage)
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

func dynamicProviderCreateBody(opts dynamicProviderCreateOptions, provider apimodel.SandboxProviderCatalogItem) (*apimodel.CreateSandboxProviderInstanceBody, error) {
	body := &apimodel.CreateSandboxProviderInstanceBody{Name: opts.Name, Type: opts.ProviderType}
	if opts.Flags == nil {
		return body, nil
	}
	secrets, err := providerSecretValues(opts.Secrets, provider, opts.Stdin)
	if err != nil {
		return nil, err
	}
	var raw jx.Raw
	if config := strings.TrimSpace(opts.Config); config != "" {
		for _, field := range sortedProviderConfigFields(provider) {
			if opts.Flags.Changed(providerFieldFlagName(field.Key)) {
				return nil, fmt.Errorf("--config cannot be combined with provider-specific flags")
			}
		}
		if raw, err = rawJSON(config); err != nil {
			return nil, err
		}
	} else if raw, err = dynamicProviderCreateConfig(opts.Flags, provider); err != nil {
		return nil, err
	}
	if raw, err = withProviderSecrets(raw, secrets); err != nil {
		return nil, err
	}
	if err := requireProviderSecrets(raw, provider); err != nil {
		return nil, err
	}
	body.SetConfig(raw)
	return body, nil
}

// providerSecretValues reads each --secret KEY=@path or KEY=- into the value
// of the provider's secret field KEY. A value is never taken from the command
// line itself, and stdin is read for at most one.
func providerSecretValues(specs []string, provider apimodel.SandboxProviderCatalogItem, stdin io.Reader) (map[string]string, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	secretKeys := map[string]bool{}
	var names []string
	for _, field := range provider.ConfigFields.Or(nil) {
		if isSecretProviderField(field) {
			secretKeys[field.Key] = true
			names = append(names, field.Key)
		}
	}
	values := make(map[string]string, len(specs))
	readStdin := false
	for _, spec := range specs {
		key, source, ok := strings.Cut(spec, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("--secret %q: give it as KEY=@path or KEY=-", spec)
		}
		if !secretKeys[key] {
			if len(names) == 0 {
				return nil, fmt.Errorf("--secret %s: a %s provider has no secret fields", key, provider.ID)
			}
			return nil, fmt.Errorf("--secret %s: not a secret field of a %s provider (its secret fields: %s)", key, provider.ID, strings.Join(names, ", "))
		}
		if _, dup := values[key]; dup {
			return nil, fmt.Errorf("--secret %s is given twice", key)
		}
		var data []byte
		var err error
		switch {
		case source == "-":
			if readStdin {
				return nil, fmt.Errorf("--secret %s: only one secret may be read from stdin", key)
			}
			readStdin = true
			if stdin == nil {
				return nil, fmt.Errorf("--secret %s: no stdin to read", key)
			}
			data, err = io.ReadAll(stdin)
		case strings.HasPrefix(source, "@"):
			data, err = os.ReadFile(strings.TrimPrefix(source, "@"))
		default:
			return nil, fmt.Errorf("--secret %s: read it from a file (%s=@path) or stdin (%s=-), never from the command line", key, key, key)
		}
		if err != nil {
			return nil, fmt.Errorf("--secret %s: %w", key, err)
		}
		values[key] = string(data)
	}
	return values, nil
}

// withProviderSecrets sets the secret values in a config object.
func withProviderSecrets(raw jx.Raw, secrets map[string]string) (jx.Raw, error) {
	if len(secrets) == 0 {
		return raw, nil
	}
	config := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &config); err != nil {
			return nil, fmt.Errorf("config must be a JSON object: %w", err)
		}
	}
	for key, value := range secrets {
		config[key] = value
	}
	data, err := json.Marshal(config)
	if err != nil {
		return nil, err
	}
	return jx.Raw(data), nil
}

// requireProviderSecrets refuses a create that leaves out a required secret
// field, naming the flag that gives it.
func requireProviderSecrets(raw jx.Raw, provider apimodel.SandboxProviderCatalogItem) error {
	config := map[string]json.RawMessage{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &config)
	}
	for _, field := range provider.ConfigFields.Or(nil) {
		if !isSecretProviderField(field) || !field.Required.Or(false) {
			continue
		}
		if _, ok := config[field.Key]; !ok {
			return fmt.Errorf("required secret %s not set: give it with --secret %s=@path or --secret %s=-", field.Key, field.Key, field.Key)
		}
	}
	return nil
}

func isSecretProviderField(field apimodel.ProviderConfigField) bool {
	return strings.EqualFold(field.Type, "secret")
}

func dynamicProviderCreateConfig(flags *pflag.FlagSet, provider apimodel.SandboxProviderCatalogItem) (jx.Raw, error) {
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

func dynamicProviderUpdateBody(opts dynamicProviderUpdateOptions, provider apimodel.SandboxProviderCatalogItem, current *apimodel.SandboxProviderInstance) (*apimodel.UpdateSandboxProviderInstanceBody, error) {
	body := &apimodel.UpdateSandboxProviderInstanceBody{}
	if opts.Flags == nil {
		return body, nil
	}
	if opts.Flags.Changed("name") {
		body.SetName(apiclientgen.NewOptString(opts.Name))
	}
	if opts.Flags.Changed("disabled") {
		body.SetDisabled(apiclientgen.NewOptBool(opts.Disabled))
	}
	if dynamicProviderUpdateConfigChanged(opts.Flags, provider) {
		raw, err := dynamicProviderUpdateConfig(opts, provider, current)
		if err != nil {
			return nil, err
		}
		body.SetConfig(raw)
	}
	return body, nil
}

// dynamicProviderUpdateConfig is the config an update sends, which replaces
// the stored one. The current config it starts from holds no secret field,
// and a secret field an update leaves out keeps its value, so only a --secret
// given here changes one.
func dynamicProviderUpdateConfig(opts dynamicProviderUpdateOptions, provider apimodel.SandboxProviderCatalogItem, current *apimodel.SandboxProviderInstance) (jx.Raw, error) {
	secrets, err := providerSecretValues(opts.Secrets, provider, opts.Stdin)
	if err != nil {
		return nil, err
	}
	if opts.Flags.Changed("config") {
		for _, field := range sortedProviderConfigFields(provider) {
			if opts.Flags.Changed(providerFieldFlagName(field.Key)) {
				return nil, fmt.Errorf("--config cannot be combined with provider-specific flags")
			}
		}
		raw, err := rawJSON(opts.Config)
		if err != nil {
			return nil, err
		}
		return withProviderSecrets(raw, secrets)
	}
	m := map[string]any{}
	if current != nil && len(current.GetConfig()) > 0 {
		_ = json.Unmarshal(current.GetConfig(), &m)
	}
	for _, field := range sortedProviderConfigFields(provider) {
		flagName := providerFieldFlagName(field.Key)
		if !opts.Flags.Changed(flagName) {
			continue
		}
		value, err := opts.Flags.GetString(flagName)
		if err != nil {
			return nil, err
		}
		converted, err := providerFieldValue(field, value)
		if err != nil {
			return nil, fmt.Errorf("--%s: %w", flagName, err)
		}
		m[field.Key] = converted
	}
	for key, value := range secrets {
		m[key] = value
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return jx.Raw(data), nil
}

func dynamicProviderUpdateConfigChanged(flags *pflag.FlagSet, provider apimodel.SandboxProviderCatalogItem) bool {
	if flags.Changed("config") || flags.Changed("secret") {
		return true
	}
	for _, field := range sortedProviderConfigFields(provider) {
		if flags.Changed(providerFieldFlagName(field.Key)) {
			return true
		}
	}
	return false
}

func (a *App) writeProviderCreateHelpForProvider(cmd *cobra.Command, providerType string) error {
	client, err := a.apiClient()
	if err != nil {
		return err
	}
	catalogRes, err := client.ListSandboxProviderCatalog(cmd.Context())
	if err != nil {
		return err
	}
	catalog, err := expectResponse[apimodel.ListSandboxProviderCatalogBody](catalogRes)
	if err != nil {
		return err
	}
	provider, ok := findProviderCatalogItem(catalog.GetProviders(), providerType)
	if !ok {
		return fmt.Errorf("provider type %q not found", providerType)
	}
	return writeProviderCreateHelp(cmd.OutOrStdout(), provider)
}

func (a *App) writeProviderUpdateHelpForProvider(cmd *cobra.Command, providerType string) error {
	client, err := a.apiClient()
	if err != nil {
		return err
	}
	catalogRes, err := client.ListSandboxProviderCatalog(cmd.Context())
	if err != nil {
		return err
	}
	catalog, err := expectResponse[apimodel.ListSandboxProviderCatalogBody](catalogRes)
	if err != nil {
		return err
	}
	provider, ok := findProviderCatalogItem(catalog.GetProviders(), providerType)
	if !ok {
		return fmt.Errorf("provider type %q not found", providerType)
	}
	return writeProviderUpdateHelp(cmd.OutOrStdout(), provider)
}

func writeProviderCreateStaticHelp(cmd *cobra.Command) {
	fmt.Fprintln(cmd.OutOrStdout(), cmd.Long)
	fmt.Fprintln(cmd.OutOrStdout())
	fmt.Fprintln(cmd.OutOrStdout(), "Provider discovery:")
	fmt.Fprintln(cmd.OutOrStdout(), "      discobox admin provider catalog          List available provider types")
	fmt.Fprintln(cmd.OutOrStdout(), "      discobox admin provider create --help=PROVIDER")
	fmt.Fprintln(cmd.OutOrStdout(), "                                      Show provider-specific create flags")
	fmt.Fprintln(cmd.OutOrStdout())
	fmt.Fprintln(cmd.OutOrStdout(), "Common Flags:")
	fmt.Fprintln(cmd.OutOrStdout(), "      --name string       Provider instance name")
	fmt.Fprintln(cmd.OutOrStdout(), "      --type string       Provider type (default \"digitalocean\")")
	fmt.Fprintln(cmd.OutOrStdout(), "      --config string     Provider config JSON or @path")
	fmt.Fprintln(cmd.OutOrStdout(), "      --secret KEY=@path  Secret field from a file, or KEY=- from stdin (repeatable)")
	fmt.Fprintln(cmd.OutOrStdout(), "      --help              Show this help without contacting the API server")
	fmt.Fprintln(cmd.OutOrStdout(), "      --help=PROVIDER     Load catalog and show provider-specific flags")
}

func writeProviderUpdateStaticHelp(cmd *cobra.Command) {
	fmt.Fprintln(cmd.OutOrStdout(), cmd.Long)
	fmt.Fprintln(cmd.OutOrStdout())
	fmt.Fprintln(cmd.OutOrStdout(), "Provider discovery:")
	fmt.Fprintln(cmd.OutOrStdout(), "      discobox admin provider catalog          List available provider types")
	fmt.Fprintln(cmd.OutOrStdout(), "      discobox admin provider update --help=PROVIDER")
	fmt.Fprintln(cmd.OutOrStdout(), "                                      Show provider-specific update flags")
	fmt.Fprintln(cmd.OutOrStdout())
	fmt.Fprintln(cmd.OutOrStdout(), "Common Flags:")
	fmt.Fprintln(cmd.OutOrStdout(), "      --name string       Provider instance name")
	fmt.Fprintln(cmd.OutOrStdout(), "      --disabled          Disable or enable the provider instance")
	fmt.Fprintln(cmd.OutOrStdout(), "      --config string     Provider config JSON or @path")
	fmt.Fprintln(cmd.OutOrStdout(), "      --secret KEY=@path  Secret field from a file, or KEY=- from stdin (repeatable)")
	fmt.Fprintln(cmd.OutOrStdout(), "      --help              Show this help without contacting the API server")
	fmt.Fprintln(cmd.OutOrStdout(), "      --help=PROVIDER     Load catalog and show provider-specific flags")
}

func writeProviderCreateHelp(w io.Writer, provider apimodel.SandboxProviderCatalogItem) error {
	fmt.Fprintf(w, "Create a %s provider instance\n\n", provider.Name)
	if description, ok := provider.Description.Get(); ok && strings.TrimSpace(description) != "" {
		fmt.Fprintf(w, "%s\n\n", description)
	}
	fmt.Fprintf(w, "Usage:\n  discobox admin provider create --type %s [provider flags]\n\n", provider.ID)
	fmt.Fprintln(w, "Common Flags:")
	fmt.Fprintln(w, "      --name string     Provider instance name")
	fmt.Fprintf(w, "      --type string     Provider type (default %q)\n", provider.ID)
	fmt.Fprintln(w, "      --config string   Provider config JSON or @path; cannot be combined with provider-specific flags")
	fields := sortedProviderConfigFields(provider)
	defer writeProviderSecretHelp(w, provider, true)
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
			if field.Immutable.Or(false) {
				advanced += " [fixed while it has pools]"
			}
			fmt.Fprintf(w, "      --%-22s %s%s%s%s\n", providerFieldFlagName(field.Key)+" string", description, required, placeholder, advanced)
		}
	}
	return nil
}

func writeProviderUpdateHelp(w io.Writer, provider apimodel.SandboxProviderCatalogItem) error {
	fmt.Fprintf(w, "Update a %s provider instance\n\n", provider.Name)
	if description, ok := provider.Description.Get(); ok && strings.TrimSpace(description) != "" {
		fmt.Fprintf(w, "%s\n\n", description)
	}
	fmt.Fprintf(w, "Usage:\n  discobox admin provider update PROVIDER [provider flags]\n\n")
	fmt.Fprintln(w, "Common Flags:")
	fmt.Fprintln(w, "      --name string       Provider instance name")
	fmt.Fprintln(w, "      --disabled          Disable or enable the provider instance")
	fmt.Fprintln(w, "      --config string     Provider config JSON or @path; cannot be combined with provider-specific flags")
	fields := sortedProviderConfigFields(provider)
	defer writeProviderSecretHelp(w, provider, false)
	if len(fields) > 0 {
		fmt.Fprintln(w, "\nProvider Flags:")
		for _, field := range fields {
			placeholder := ""
			if value, ok := field.Placeholder.Get(); ok && strings.TrimSpace(value) != "" {
				placeholder = fmt.Sprintf(" default/example: %s", value)
			}
			description := field.Description.Or(field.Label)
			advanced := ""
			if field.Advanced.Or(false) {
				advanced = " [advanced]"
			}
			if field.Immutable.Or(false) {
				advanced += " [fixed while it has pools]"
			}
			fmt.Fprintf(w, "      --%-22s %s%s%s\n", providerFieldFlagName(field.Key)+" string", description, placeholder, advanced)
		}
	}
	return nil
}

// writeProviderSecretHelp lists the provider's secret fields, which are given
// with --secret rather than a flag of their own.
func writeProviderSecretHelp(w io.Writer, provider apimodel.SandboxProviderCatalogItem, create bool) {
	var fields []apimodel.ProviderConfigField
	for _, field := range provider.ConfigFields.Or(nil) {
		if isSecretProviderField(field) {
			fields = append(fields, field)
		}
	}
	if len(fields) == 0 {
		return
	}
	fmt.Fprintln(w, "\nSecret Fields (--secret KEY=@path or --secret KEY=-; sealed by the server and never shown again):")
	for _, field := range fields {
		required := ""
		if create && field.Required.Or(false) {
			required = " (required)"
		}
		fmt.Fprintf(w, "      %-24s %s%s\n", field.Key, field.Description.Or(field.Label), required)
	}
	if !create {
		fmt.Fprintln(w, "      A secret field an update leaves out keeps its value.")
	}
}

func findProviderCatalogItem(providers []apimodel.SandboxProviderCatalogItem, providerType string) (apimodel.SandboxProviderCatalogItem, bool) {
	for _, provider := range providers {
		if provider.ID == providerType {
			return provider, true
		}
	}
	return apimodel.SandboxProviderCatalogItem{}, false
}

// sortedProviderConfigFields lists the fields that get a flag of their own:
// every field but a secret one, which is given with --secret instead.
func sortedProviderConfigFields(provider apimodel.SandboxProviderCatalogItem) []apimodel.ProviderConfigField {
	var fields []apimodel.ProviderConfigField
	for _, field := range provider.ConfigFields.Or(nil) {
		if !isSecretProviderField(field) {
			fields = append(fields, field)
		}
	}
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

func providerFieldDescription(field apimodel.ProviderConfigField) string {
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

func providerFieldValue(field apimodel.ProviderConfigField, value string) (any, error) {
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
