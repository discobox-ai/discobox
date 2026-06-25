package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	"github.com/obot-platform/discobox/controlplane"
	"github.com/obot-platform/discobox/localipc"
	discoboxserver "github.com/obot-platform/discobox/server"
)

const defaultProjectAlias = "default"

type App struct {
	serverURL string
	projectID string
	token     string
	output    string
	quiet     bool
	debug     bool
	noStart   bool
	errOut    io.Writer
}

func NewRootCommand() *cobra.Command {
	app := &App{}
	cmd := &cobra.Command{
		Use:           "discobox",
		Short:         "Discobox command line client",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			app.errOut = cmd.ErrOrStderr()
			return app.validate()
		},
	}
	cmd.PersistentFlags().StringVar(&app.serverURL, "server", envOrDefault("DISCOBOX_SERVER", localipc.DefaultEndpoint()), "Discobox API server endpoint")
	cmd.PersistentFlags().StringVarP(&app.projectID, "project", "p", envOrDefault("DISCOBOX_PROJECT", defaultProjectAlias), "Project ID for this invocation; use default for the user's default project")
	cmd.PersistentFlags().StringVar(&app.token, "token", os.Getenv("DISCOBOX_TOKEN"), "Bearer token for API requests")
	cmd.PersistentFlags().StringVarP(&app.output, "output", "o", "table", "Output format: table or json")
	cmd.PersistentFlags().BoolVar(&app.debug, "debug", false, "Print HTTP requests made by the API client")
	cmd.PersistentFlags().BoolVar(&app.noStart, "no-start", false, "Do not start a local server when the endpoint is unavailable")

	cmd.AddCommand(app.newSandboxCommand())
	cmd.AddCommand(app.newRunCommand())
	cmd.AddCommand(app.newAgentCommand())
	cmd.AddCommand(app.newProviderCommand())
	cmd.AddCommand(app.newWorkerCommand())
	cmd.AddCommand(app.newEventsCommand())
	cmd.AddCommand(app.newJobCommand())
	cmd.AddCommand(app.newCompletionCommand())
	cmd.AddCommand(app.newServerCommand())
	return cmd
}

func (a *App) addQuietFlag(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&a.quiet, "quiet", "q", false, "Only display resource IDs")
}

func (a *App) validate() error {
	switch a.output {
	case "table", "json":
		return nil
	default:
		return fmt.Errorf("unsupported output format %q; expected table or json", a.output)
	}
}

func (a *App) projectIDValue() (string, error) {
	projectID := strings.TrimSpace(a.projectID)
	if projectID == "" {
		return "", errMissingProject
	}
	return projectID, nil
}

func (a *App) apiClient() (*apiclientgen.Client, error) {
	baseURL, client, err := a.httpClient()
	if err != nil {
		return nil, err
	}
	return apiclientgen.NewClient(baseURL, apiclientgen.WithClient(client))
}

func (a *App) httpClient() (string, *http.Client, error) {
	return a.httpClientWithAutoStart(!a.noStart)
}

func (a *App) httpClientWithAutoStart(autoStart bool) (string, *http.Client, error) {
	transport := http.DefaultTransport
	baseURL := a.serverURL
	if isLocalEndpoint(a.serverURL) {
		if autoStart {
			if err := a.ensureLocalServer(context.Background()); err != nil {
				return "", nil, err
			}
		}
		localBaseURL, client, err := localipc.HTTPClient(a.serverURL, transport)
		if err != nil {
			return "", nil, err
		}
		baseURL = localBaseURL
		transport = client.Transport
	}
	if a.debug {
		transport = debugTransport{
			out:  a.errOut,
			base: transport,
		}
	}
	if strings.TrimSpace(a.token) != "" {
		transport = requestHeaderTransport{
			token: strings.TrimSpace(a.token),
			base:  transport,
		}
	}
	if transport == http.DefaultTransport {
		return baseURL, http.DefaultClient, nil
	}
	return baseURL, &http.Client{
		Transport: transport,
	}, nil
}

func isLocalEndpoint(endpoint string) bool {
	endpoint = strings.TrimSpace(strings.ToLower(endpoint))
	return strings.HasPrefix(endpoint, "unix://") || strings.HasPrefix(endpoint, "npipe://")
}

func (a *App) ensureLocalServer(ctx context.Context) error {
	command, err := os.Executable()
	if err != nil {
		return err
	}
	return localipc.EnsureRunning(ctx, localipc.LaunchOptions{
		Endpoint: a.serverURL,
		Command:  command,
		Args:     []string{"server"},
		Env:      localServerEnv(a.serverURL),
	})
}

func localServerEnv(endpoint string) []string {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = fmt.Sprint(controlplane.DefaultPort)
	}
	env := []string{
		"DISCOBOX_SERVER_LISTEN=" + strings.Join([]string{endpoint, "http://0.0.0.0:" + port}, ","),
		"DISCOBOX_SERVER=" + endpoint,
		"DISCOBOX_SERVER_IDLE_TIMEOUT=5m",
	}
	for _, key := range []string{
		"DATABASE_DSN",
		"DATABASE_READ_DSN",
		"DISCOBOX_CACHE_DIR",
		"DISCOBOX_CONFIG_DIR",
		"DISCOBOX_DATA_DIR",
		"DISCOBOX_DEFAULT_SANDBOX_IMAGE",
		"DISCOBOX_ENCRYPTION_KEY",
		"DISCOBOX_STATE_DIR",
		"OTEL_METRICS_EXPORTER",
		"PATH",
		"PORT",
	} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func (a *App) newServerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the Discobox API server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return discoboxserver.Run(cmd.Context())
		},
	}
	cmd.AddCommand(a.newServerShutdownCommand())
	return cmd
}

func (a *App) newServerShutdownCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "shutdown",
		Short: "Ask the Discobox API server to stop",
		RunE: func(cmd *cobra.Command, _ []string) error {
			baseURL, httpClient, err := a.httpClientWithAutoStart(false)
			if err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(cmd.Context(), http.MethodPost, baseURL+"/shutdown", nil)
			if err != nil {
				return err
			}
			resp, err := httpClient.Do(req)
			if err != nil {
				return fmt.Errorf("shutdown server: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("shutdown server: %s: %s", resp.Status, strings.TrimSpace(string(body)))
			}
			if a.output == "json" {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"shutdown": true})
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "shutdown requested")
			return err
		},
	}
}

type requestHeaderTransport struct {
	token string
	base  http.RoundTripper
}

func (t requestHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	if t.token != "" {
		cloned.Header.Set("Authorization", "Bearer "+t.token)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(cloned)
}

type debugTransport struct {
	out  io.Writer
	base http.RoundTripper
}

func (t debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	out := t.out
	if out == nil {
		out = os.Stderr
	}
	fmt.Fprintf(out, "> %s %s\n", req.Method, req.URL.Redacted())
	for name, values := range req.Header {
		for _, value := range values {
			if strings.EqualFold(name, "Authorization") {
				value = "[REDACTED]"
			}
			fmt.Fprintf(out, "> %s: %s\n", name, value)
		}
	}
	req, err := logRequestBody(out, req)
	if err != nil {
		fmt.Fprintf(out, "> body error: %v\n", err)
		return nil, err
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil {
		fmt.Fprintf(out, "< error: %v\n", err)
		return nil, err
	}
	fmt.Fprintf(out, "< %s\n", resp.Status)
	if resp.Body != nil && resp.Body != http.NoBody {
		fmt.Fprintln(out, "< body:")
		resp.Body = debugBody{out: out, base: resp.Body}
	}
	return resp, nil
}

func logRequestBody(out io.Writer, req *http.Request) (*http.Request, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return req, nil
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return req, err
	}
	if err := req.Body.Close(); err != nil {
		return req, err
	}
	cloned := req.Clone(req.Context())
	cloned.Body = io.NopCloser(bytes.NewReader(body))
	cloned.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	cloned.ContentLength = int64(len(body))
	if len(body) > 0 {
		fmt.Fprintln(out, "> body:")
		_, _ = out.Write(body)
		if body[len(body)-1] != '\n' {
			fmt.Fprintln(out)
		}
	}
	return cloned, nil
}

type debugBody struct {
	out  io.Writer
	base io.ReadCloser
}

func (b debugBody) Read(p []byte) (int, error) {
	n, err := b.base.Read(p)
	if n > 0 {
		_, _ = b.out.Write(p[:n])
	}
	return n, err
}

func (b debugBody) Close() error {
	return b.base.Close()
}

func envOrDefault(key, defaultValue string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return defaultValue
}
