package cli

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/obot-platform/disco2/internal/apiclient"
	apiclientgen "github.com/obot-platform/disco2/internal/apiclient/gen"
)

const defaultServerURL = "http://localhost:8080"
const defaultProjectAlias = "local"
const defaultLocalProjectID = "00000000-0000-0000-0000-000000000002"

type App struct {
	serverURL string
	projectID string
	tenantID  string
	token     string
	output    string
}

func NewRootCommand() *cobra.Command {
	app := &App{}
	cmd := &cobra.Command{
		Use:           "disco2",
		Short:         "Disco2 command line client",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			return app.validate()
		},
	}
	cmd.PersistentFlags().StringVar(&app.serverURL, "server", envOrDefault("DISCO2_SERVER", defaultServerURL), "Disco2 API server URL")
	cmd.PersistentFlags().StringVarP(&app.projectID, "project", "p", envOrDefault("DISCO2_PROJECT", defaultProjectAlias), "Project ID for this invocation; use local for the built-in local project")
	cmd.PersistentFlags().StringVar(&app.tenantID, "tenant", envOrDefault("DISCO2_TENANT_ID", ""), "Tenant ID for API requests")
	cmd.PersistentFlags().StringVar(&app.token, "token", os.Getenv("DISCO2_TOKEN"), "Bearer token for API requests")
	cmd.PersistentFlags().StringVarP(&app.output, "output", "o", "table", "Output format: table or json")

	cmd.AddCommand(app.newSandboxCommand())
	cmd.AddCommand(app.newProviderCommand())
	cmd.AddCommand(app.newEventsCommand())
	cmd.AddCommand(app.newCompletionCommand())
	return cmd
}

func (a *App) validate() error {
	switch a.output {
	case "table", "json":
		return nil
	default:
		return fmt.Errorf("unsupported output format %q; expected table or json", a.output)
	}
}

func (a *App) projectUUID() (uuid.UUID, error) {
	if strings.TrimSpace(a.projectID) == "" {
		return uuid.Nil, errMissingProject
	}
	if strings.EqualFold(strings.TrimSpace(a.projectID), defaultProjectAlias) {
		return uuid.Parse(defaultLocalProjectID)
	}
	id, err := uuid.Parse(a.projectID)
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

func (a *App) apiClient() (*apiclientgen.Client, error) {
	return apiclientgen.NewClient(a.serverURL, apiclientgen.WithClient(a.httpClient()))
}

func (a *App) eventClient() (*apiclient.EventClient, error) {
	return apiclient.NewEventClient(a.serverURL, apiclient.WithHTTPClient(a.httpClient()))
}

func (a *App) httpClient() *http.Client {
	if strings.TrimSpace(a.token) == "" && strings.TrimSpace(a.tenantID) == "" {
		return http.DefaultClient
	}
	return &http.Client{
		Transport: requestHeaderTransport{
			token:    strings.TrimSpace(a.token),
			tenantID: strings.TrimSpace(a.tenantID),
			base:     http.DefaultTransport,
		},
	}
}

type requestHeaderTransport struct {
	token    string
	tenantID string
	base     http.RoundTripper
}

func (t requestHeaderTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	if t.token != "" {
		cloned.Header.Set("Authorization", "Bearer "+t.token)
	}
	if t.tenantID != "" {
		cloned.Header.Set("X-Disco2-Tenant-ID", t.tenantID)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(cloned)
}

func envOrDefault(key, defaultValue string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return defaultValue
}
