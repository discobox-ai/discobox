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
	var write bool
	cmd := &cobra.Command{
		Use:   "ssh-config",
		Short: "Emit an SSH client config for this project's sandboxes",
		Long: "Emit ssh_config(5) Host stanzas — one per sandbox in the current project — plus\n" +
			"the server's known_hosts line, suitable for `disco box ssh-config >> ~/.ssh/config`\n" +
			"or an ssh_config Include directive.\n\n" +
			"With --write, the stanzas and the server's host key are written to files this\n" +
			"command owns and rewrites, and ~/.ssh/config gains a single Include line pointing\n" +
			"at them. Nothing else in ~/.ssh is edited.\n\n" +
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
			// The written files are named after the project, so resolve what
			// the flag means: "default" is a server-side alias, and the same
			// project reached as "default" and by ID must not end up owning two
			// files and two Include lines.
			resolvedProjectID := projectID
			if write {
				if resolvedProjectID, err = a.resolveProjectID(cmd, client, projectID); err != nil {
					return err
				}
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
				// --write mirrors the server into local files, and "no SSH
				// here" is a state to mirror: an empty config replaces stanzas
				// that would otherwise keep pointing at a port nothing answers
				// on. Printing has nothing to mirror and nothing to emit, so it
				// still says why rather than producing empty output to paste.
				if write {
					fmt.Fprintf(cmd.ErrOrStderr(), "this server has no SSH ingress; wrote an empty config\n")
					return writeManagedSSHConfig(cmd, resolvedProjectID, "", "", "")
				}
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

			identityFile, err = a.resolveSSHIdentity(cmd, client, projectID, identityFile)
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

			stanzas := renderSSHConfig(sshConfigRender{
				sandboxes:    sandboxesBody.GetSandboxes(),
				host:         host,
				port:         port,
				identityFile: identityFile,
				// Only the written config can point at a known_hosts file,
				// because only it owns one.
				knownHostsFile: knownHostsFileFor(write, resolvedProjectID),
			})
			if write {
				return writeManagedSSHConfig(cmd, resolvedProjectID, stanzas, knownHostsHost(host, port), ingress.HostKey.Or(""))
			}
			out := cmd.OutOrStdout()
			fmt.Fprint(out, stanzas)
			fmt.Fprintf(out, "\n# add to your known_hosts:\n# %s %s\n",
				knownHostsHost(host, port), ingress.HostKey.Or(""))
			return nil
		},
	}
	cmd.Flags().StringVar(&host, "host", "", "Override the address ssh clients should dial (default: whatever the server advertises)")
	cmd.Flags().IntVar(&port, "port", 0, "Override the SSH port (default: whatever the server advertises)")
	cmd.Flags().StringVar(&identityFile, "identity-file", "", "Private key to use, generated and enrolled if absent (default: the CLI's own managed key)")
	cmd.Flags().BoolVarP(&write, "write", "w", false, "Write the config where ssh will find it, instead of printing it")
	return cmd
}

// hostAliasSuffix qualifies each pattern into an obviously discobox-owned
// namespace. Every sandbox gets both the bare form and the qualified one: the
// bare name is what anyone actually types, and the qualified alias is the
// unambiguous spelling to fall back on when a bare name collides with a real
// host elsewhere in the user's ssh_config — which is the cost of the bare form
// and the reason the qualified one is still emitted.
const hostAliasSuffix = ".discobox.internal"

// sshConfigHostPatterns returns each sandbox's Host patterns, aligned with
// sandboxes: its name and ID, each bare and suffixed.
//
// The name is only an alias — `User` carries the sandbox ID, which is what
// actually routes (server/internal/sshd's ResolveUsername), and `HostName` is
// what ssh resolves — so the pattern is free to be friendly. It is not free to
// be ambiguous: ssh silently applies the *first* matching Host block, so a
// pattern claimed by two sandboxes would quietly send you to the wrong one.
// Patterns are therefore counted across the whole emitted config and any that
// is not unique is dropped from every stanza that wanted it.
//
// The server enforces unique names within a project
// (idx_sandbox_project_name), so name-versus-name collisions no longer happen;
// what this still catches is a name that spells another sandbox's pattern, such
// as one named exactly "<other id>.discobox.internal".
func sshConfigHostPatterns(sandboxes []apimodel.Sandbox) [][]string {
	candidates := make([][]string, len(sandboxes))
	claims := map[string]int{}
	for i, sandbox := range sandboxes {
		var patterns []string
		if name := sandbox.Config.Name; safeHostAlias(name) {
			patterns = append(patterns, name, name+hostAliasSuffix)
		}
		patterns = append(patterns, sandbox.ID, sandbox.ID+hostAliasSuffix)
		candidates[i] = patterns
		for _, pattern := range patterns {
			claims[pattern]++
		}
	}

	unique := make([][]string, len(sandboxes))
	for i, patterns := range candidates {
		for _, pattern := range patterns {
			if claims[pattern] == 1 {
				unique[i] = append(unique[i], pattern)
			}
		}
	}
	return unique
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

// resolveSSHIdentity returns the private key path the emitted config should
// name, enrolling it if the project does not already have it.
//
// A new key is generated only as a last resort. An enrolled key the caller can
// actually use is preferred in every case, because generating one would leave
// the project accumulating keys that all authenticate the same person from the
// same machine, and would revoke nothing when the old one is removed. In order:
//
//  1. --identity-file, which is an explicit instruction, not a preference.
//  2. The key this command manages, if it exists.
//  3. Any ~/.ssh key already enrolled in this project whose private half is
//     present — the case where the user enrolled their own key by hand.
//  4. Only then, a freshly generated managed key.
//
// Agent-only keys cannot win step 3: `IdentityFile` names a file, and an agent
// identity has no path to name.
func (a *App) resolveSSHIdentity(cmd *cobra.Command, client *apiclientgen.Client, projectID, explicitPath string) (string, error) {
	enrolled, err := a.listEnrolledFingerprints(cmd, client, projectID)
	if err != nil {
		return "", err
	}

	if strings.TrimSpace(explicitPath) != "" {
		return explicitPath, a.enrollSSHIdentity(cmd, client, projectID, explicitPath, enrolled)
	}
	managed := defaultSSHIdentityPath()
	if fileExists(managed) {
		return managed, a.enrollSSHIdentity(cmd, client, projectID, managed, enrolled)
	}
	if reusable := enrolledLocalKey(enrolled); reusable != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "using %s, already enrolled in this project\n", reusable)
		return reusable, nil
	}
	return managed, a.enrollSSHIdentity(cmd, client, projectID, managed, enrolled)
}

func (a *App) listEnrolledFingerprints(cmd *cobra.Command, client *apiclientgen.Client, projectID string) (map[string]bool, error) {
	res, err := client.ListSSHKeys(cmd.Context(), apiclientgen.ListSSHKeysParams{ProjectId: projectID})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListSSHKeysBody](res)
	if err != nil {
		return nil, err
	}
	fingerprints := map[string]bool{}
	for _, key := range body.GetSshKeys() {
		fingerprints[key.Fingerprint] = true
	}
	return fingerprints, nil
}

// enrolledLocalKey returns the path of a ~/.ssh private key whose public half
// is already enrolled, or "" when there is none. Discovery failures are not
// errors: this is an optimization over generating a key, so a missing or
// unreadable ~/.ssh simply means there is nothing to reuse.
func enrolledLocalKey(enrolled map[string]bool) string {
	candidates, err := discoverSSHDirPublicKeys()
	if err != nil {
		return ""
	}
	for _, candidate := range candidates {
		if candidate.privateKeyPath != "" && enrolled[candidate.fingerprint] {
			return candidate.privateKeyPath
		}
	}
	return ""
}

// enrollSSHIdentity generates the key at path if absent and enrolls it when the
// project does not already list its fingerprint, so the emitted config can
// authenticate on its own.
//
// Enrollment is keyed on the fingerprint rather than on having just generated
// the key, so running this against a second project — or after someone revoked
// the key — enrolls the existing key instead of creating a duplicate or leaving
// a config that cannot authenticate.
func (a *App) enrollSSHIdentity(cmd *cobra.Command, client *apiclientgen.Client, projectID, path string, enrolled map[string]bool) error {
	publicKeyLine, created, err := loadOrCreateSSHIdentity(path)
	if err != nil {
		return err
	}
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(publicKeyLine))
	if err != nil {
		return fmt.Errorf("parse SSH identity public key: %w", err)
	}
	fingerprint := ssh.FingerprintSHA256(parsed)
	if created {
		fmt.Fprintf(cmd.ErrOrStderr(), "generated a new SSH key at %s (%s)\n", path, fingerprint)
	}
	if enrolled[fingerprint] {
		return nil
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
