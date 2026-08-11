package cli

import (
	"fmt"
	"net"
	"strconv"

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
			"or an ssh_config Include directive.\n\n" +
			"The address comes from the server, which knows where its SSH ingress is reachable;\n" +
			"--host and --port override it for cases the server cannot know about, such as a\n" +
			"local port forward.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}

			ingressRes, err := client.GetSSHIngress(cmd.Context())
			if err != nil {
				return err
			}
			ingress, err := expectResponse[apimodel.SSHIngress](ingressRes)
			if err != nil {
				return err
			}
			if !ingress.Enabled {
				return fmt.Errorf("this server has no SSH ingress: set DISCOBOX_SSH_LISTEN to enable it")
			}
			ingressHost, ingressPort, err := net.SplitHostPort(ingress.Address.Or(""))
			if err != nil {
				return fmt.Errorf("server advertised an unusable SSH address %q: %w", ingress.Address.Or(""), err)
			}
			// Flags are overrides, never defaults: an unset flag means "use
			// what the server advertises", which is the whole point of asking.
			if !cmd.Flags().Changed("host") {
				host = ingressHost
			}
			if !cmd.Flags().Changed("port") {
				if port, err = strconv.Atoi(ingressPort); err != nil {
					return fmt.Errorf("server advertised an unusable SSH port %q: %w", ingressPort, err)
				}
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
			fmt.Fprintf(out, "# add to your known_hosts:\n# %s %s\n",
				knownHostsHost(host, port), ingress.HostKey.Or(""))
			return nil
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "Override the address ssh clients should dial (default: whatever the server advertises)")
	cmd.Flags().IntVar(&port, "port", 0, "Override the SSH port (default: whatever the server advertises)")
	return cmd
}

// knownHostsHost renders a known_hosts(5) host field. A non-default port takes
// the bracketed "[host]:port" form — and only a non-default one: ssh looks up
// a port-22 host under its bare name, so bracketing it would produce an entry
// that never matches.
func knownHostsHost(host string, port int) string {
	if port == 22 {
		return host
	}
	return fmt.Sprintf("[%s]:%d", host, port)
}
