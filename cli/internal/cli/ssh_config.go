package cli

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

func (a *App) newSSHConfigCommand() *cobra.Command {
	var host string
	var port int
	var identityFile string
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

			if strings.TrimSpace(identityFile) == "" {
				identityFile = defaultSSHIdentityPath()
			}
			if err := a.ensureSSHIdentityEnrolled(cmd, client, projectID, identityFile); err != nil {
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

			sandboxes := sandboxesBody.GetSandboxes()
			out := cmd.OutOrStdout()
			for i, sandbox := range sandboxes {
				fmt.Fprintf(out, "Host %s\n", strings.Join(sshConfigHostPatterns(sandbox, sandboxes), " "))
				fmt.Fprintf(out, "    HostName %s\n", host)
				fmt.Fprintf(out, "    Port %d\n", port)
				fmt.Fprintf(out, "    User %s\n", sandbox.ID)
				fmt.Fprintf(out, "    IdentityFile %s\n", identityFile)
				// Without IdentitiesOnly, ssh offers every agent key before
				// this one and can exhaust MaxAuthTries before reaching it.
				fmt.Fprintf(out, "    IdentitiesOnly yes\n")
				if i < len(sandboxes)-1 {
					fmt.Fprintln(out)
				}
			}
			fmt.Fprintf(out, "\n# add to your known_hosts:\n# %s %s\n",
				knownHostsHost(host, port), ingress.HostKey.Or(""))
			return nil
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "Override the address ssh clients should dial (default: whatever the server advertises)")
	cmd.Flags().IntVar(&port, "port", 0, "Override the SSH port (default: whatever the server advertises)")
	cmd.Flags().StringVar(&identityFile, "identity-file", "", "Private key to use, generated and enrolled if absent (default: the CLI's own managed key)")
	return cmd
}

// hostAliasSuffix keeps every generated Host pattern inside one obviously
// discobox-owned namespace, so a pattern can never collide with a real host in
// the user's ssh_config.
const hostAliasSuffix = ".discobox.internal"

// sshConfigHostPatterns returns the Host patterns for one sandbox: its name
// first, when that name can safely and unambiguously stand for it, then always
// its ID.
//
// The name is only an alias — `User` carries the sandbox ID, which is what
// actually routes (server/internal/sshd's ResolveUsername), and `HostName` is
// what ssh resolves — so the pattern is free to be friendly. It is not free to
// be ambiguous: sandbox names have no unique index and are settable at create,
// so two can share one, and ssh silently applies the *first* matching block,
// which would quietly send you to the wrong sandbox. A duplicated name is
// therefore dropped rather than emitted for both. The ID pattern is always
// present, so every sandbox stays addressable however its name behaves.
func sshConfigHostPatterns(sandbox apimodel.Sandbox, all []apimodel.Sandbox) []string {
	idPattern := sandbox.ID + hostAliasSuffix
	name := sandbox.Config.Name
	if !safeHostAlias(name) {
		return []string{idPattern}
	}
	for _, other := range all {
		if other.ID != sandbox.ID && other.Config.Name == name {
			return []string{idPattern}
		}
	}
	return []string{name + hostAliasSuffix, idPattern}
}

// safeHostAlias reports whether a sandbox name can be used as an ssh_config
// Host pattern. Names are free text up to 200 characters, and a Host line is
// whitespace-separated patterns with glob metacharacters: a name containing a
// space would silently become two patterns, and one containing `*` or `?`
// would match hosts it has no business matching — `Host *.discobox.internal`
// from a sandbox literally named `*` would capture every other sandbox.
func safeHostAlias(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case (r == '_' || r == '-' || r == '.') && i > 0:
		default:
			return false
		}
	}
	return true
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

// ensureSSHIdentityEnrolled makes the emitted config usable on its own: it
// generates the key when absent and enrolls it in the project when the project
// does not already have it, so `disco box ssh-config >> ~/.ssh/config` is the
// only step between a fresh checkout and `ssh <sandbox>`.
//
// Enrollment is keyed on the fingerprint, not on having just generated the
// key, so running this against a second project — or after someone revoked the
// key — enrolls the existing key rather than creating a duplicate or leaving a
// config that cannot authenticate.
func (a *App) ensureSSHIdentityEnrolled(cmd *cobra.Command, client *apiclientgen.Client, projectID, identityFile string) error {
	publicKeyLine, created, err := loadOrCreateSSHIdentity(identityFile)
	if err != nil {
		return err
	}
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicKeyLine))
	if err != nil {
		return fmt.Errorf("parse SSH identity public key: %w", err)
	}
	fingerprint := ssh.FingerprintSHA256(parsed)
	if created {
		fmt.Fprintf(cmd.ErrOrStderr(), "generated a new SSH key at %s (%s)\n", identityFile, fingerprint)
	}

	res, err := client.ListSSHKeys(cmd.Context(), apiclientgen.ListSSHKeysParams{ProjectId: projectID})
	if err != nil {
		return err
	}
	body, err := expectResponse[apimodel.ListSSHKeysBody](res)
	if err != nil {
		return err
	}
	for _, key := range body.GetSshKeys() {
		if key.Fingerprint == fingerprint {
			return nil
		}
	}

	createBody := &apimodel.CreateSSHKeyBody{PublicKey: publicKeyLine}
	createBody.SetName(apiclientgen.NewOptString(sshIdentityComment()))
	createRes, err := client.CreateSSHKey(cmd.Context(), createBody, apiclientgen.CreateSSHKeyParams{ProjectId: projectID})
	if err != nil {
		return err
	}
	if _, err := expectResponse[apimodel.SSHKey](createRes); err != nil {
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "enrolled SSH key %s in this project\n", fingerprint)
	return nil
}
