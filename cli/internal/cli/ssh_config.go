package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

func (a *App) newSSHConfigCommand() *cobra.Command {
	var host string
	var port int
	cmd := &cobra.Command{
		Use:   "ssh-config",
		Short: "Emit an SSH client config for this project's sandboxes",
		Long: "Emit ssh_config(5) Host stanzas — one per sandbox in the current project — plus\n" +
			"the server's known_hosts line, suitable for `disco box ssh-config >> ~/.ssh/config`\n" +
			"or an ssh_config Include directive.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			sandboxesRes, err := client.ListSandboxes(cmd.Context(), apiclientgen.ListSandboxesParams{ProjectId: projectID})
			if err != nil {
				return err
			}
			sandboxesBody, err := expectResponse[apimodel.ListSandboxesBody](sandboxesRes)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			for _, sandbox := range sandboxesBody.GetSandboxes() {
				fmt.Fprintf(out, "Host %s.discobox.internal\n", sandbox.ID)
				fmt.Fprintf(out, "    HostName %s\n", host)
				fmt.Fprintf(out, "    Port %d\n", port)
				fmt.Fprintf(out, "    User %s\n\n", sandbox.ID)
			}

			knownHostsLine, err := a.fetchSSHHostKeyLine(cmd.Context())
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not fetch the server's SSH host key: %v\n", err)
				return nil
			}
			fmt.Fprintf(out, "# add to your known_hosts:\n# [%s]:%d %s\n", host, port, knownHostsLine)
			return nil
		},
	}
	cmd.Flags().StringVar(&host, "host", "127.0.0.1", "Address ssh clients should dial")
	cmd.Flags().IntVar(&port, "port", 3222, "discobox-server's SSH port (DISCOBOX_SSH_LISTEN)")
	return cmd
}

// fetchSSHHostKeyLine reads GET /ssh/host-key, a small unauthenticated route
// (ADR 0024) served alongside the normal control-plane API: the SSH host
// *public* key is not a credential, and disco ssh-config must be able to read
// it before any other credential exists.
func (a *App) fetchSSHHostKeyLine(ctx context.Context) (string, error) {
	baseURL, httpClient, err := a.httpClient()
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/ssh/host-key", nil)
	if err != nil {
		return "", err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET /ssh/host-key: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}
