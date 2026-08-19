package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

func (a *App) newSSHConfigCommand() *cobra.Command {
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
			"The stanzas name no address. They carry a ProxyCommand that reaches the server's\n" +
			"SSH ingress over the same endpoint every other request uses, which is the only way\n" +
			"in: the server binds no SSH port of its own. So ssh — and anything built on it,\n" +
			"such as VS Code Remote-SSH — connects wherever this CLI does.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			// The written files are named after the project and the host key
			// is verified under a name derived from it, so resolve what the
			// flag means either way: "default" is a server-side alias, and the
			// same project reached as "default" and by ID must not end up
			// owning two files, two Include lines, and two known_hosts names.
			resolvedProjectID, err := a.concreteProjectID(cmd, client, projectID)
			if err != nil {
				return err
			}

			hostKey, err := a.sshHostKey(cmd, client)
			if err != nil {
				return err
			}
			built, err := a.buildManagedSSHConfig(cmd, managedSSHConfigRequest{
				client:            client,
				projectID:         projectID,
				resolvedProjectID: resolvedProjectID,
				identityFile:      identityFile,
				hostKey:           hostKey,
				write:             write,
			})
			if err != nil {
				return err
			}
			if write {
				return writeManagedSSHConfig(cmd, resolvedProjectID, built.stanzas, built.hostKeyAlias, built.hostKey)
			}
			out := cmd.OutOrStdout()
			fmt.Fprint(out, built.stanzas)
			fmt.Fprintf(out, "\n# add to your known_hosts:\n# %s %s\n", built.hostKeyAlias, built.hostKey)
			return nil
		},
	}
	cmd.Flags().StringVar(&identityFile, "identity-file", "", "Private key to use, generated and enrolled if absent (default: the CLI's own managed key)")
	cmd.Flags().BoolVarP(&write, "write", "w", false, "Write the config where ssh will find it, instead of printing it")
	return cmd
}

// managedSSHConfigRequest is what rendering a project's stanzas needs that the
// renderer cannot work out for itself.
type managedSSHConfigRequest struct {
	client *apiclientgen.Client
	// projectID is what was asked for, which the API takes; resolvedProjectID
	// is what it turned out to be, which names the files and the host key.
	projectID         string
	resolvedProjectID string
	// identityFile is the caller's --identity-file, empty to let
	// resolveSSHIdentity choose and enroll one.
	identityFile string
	hostKey      string
	// write decides whether the stanzas may name a known_hosts file, since only
	// a written config owns one.
	write bool
}

// managedSSHConfig is a project's emitted ssh_config: the stanzas, the name the
// host key is verified under, the key itself, and the Host pattern each sandbox
// answers to.
//
// The aliases are what a caller handing a host to another program needs — `disco
// tools vscode` builds a Remote-SSH target out of one — and they cannot be
// guessed from outside, since a contested pattern is dropped from every stanza
// that wanted it.
type managedSSHConfig struct {
	stanzas      string
	hostKeyAlias string
	hostKey      string
	aliases      map[string]string
}

// buildManagedSSHConfig resolves the key, lists the project's sandboxes, and
// renders the stanzas. It does not decide what to do with them: `ssh-config`
// prints or writes them, and `tools vscode` writes them and opens an editor on
// one.
func (a *App) buildManagedSSHConfig(cmd *cobra.Command, req managedSSHConfigRequest) (managedSSHConfig, error) {
	proxyCommand, err := sshProxyCommandLine(a.serverURL)
	if err != nil {
		return managedSSHConfig{}, err
	}
	identityFile, err := a.resolveSSHIdentity(cmd, req.client, req.projectID, req.identityFile)
	if err != nil {
		return managedSSHConfig{}, err
	}
	sandboxesRes, err := req.client.ListSandboxes(cmd.Context(), apiclientgen.ListSandboxesParams{ProjectId: req.projectID})
	if err != nil {
		return managedSSHConfig{}, err
	}
	sandboxesBody, err := expectResponse[apimodel.ListSandboxesBody](sandboxesRes)
	if err != nil {
		return managedSSHConfig{}, err
	}
	sandboxes := sandboxesBody.GetSandboxes()

	render := sshConfigRender{
		sandboxes:    sandboxes,
		proxyCommand: proxyCommand,
		hostKeyAlias: sshHostKeyAlias(req.resolvedProjectID),
		identityFile: identityFile,
		// Only the written config can point at a known_hosts file, because
		// only it owns one.
		knownHostsFile: knownHostsFileFor(req.write, req.resolvedProjectID),
	}

	// The first surviving pattern is the alias to hand out: they are emitted
	// friendliest-first, so this is the sandbox's name where the name is
	// unambiguous and its ID where it is not.
	patterns := sshConfigHostPatterns(sandboxes)
	aliases := make(map[string]string, len(sandboxes))
	for i, sandbox := range sandboxes {
		if len(patterns[i]) > 0 {
			aliases[sandbox.ID] = patterns[i][0]
		}
	}
	return managedSSHConfig{
		stanzas:      renderSSHConfig(render),
		hostKeyAlias: render.hostKeyAlias,
		hostKey:      req.hostKey,
		aliases:      aliases,
	}, nil
}

// sshHostKey is the server's host public key, which every emitted stanza pins
// and nothing else in the document needs.
//
// `GET /ssh` used to answer two more questions — whether the server serves SSH,
// and at what address — and answers neither now: it serves SSH over the
// transport the API already answers on, and that is the only way in (ADR 0057).
func (a *App) sshHostKey(cmd *cobra.Command, client *apiclientgen.Client) (string, error) {
	res, err := client.GetSSHIngress(cmd.Context())
	if err != nil {
		return "", err
	}
	ingress, err := expectResponse[apimodel.SSHIngress](res)
	if err != nil {
		return "", err
	}
	hostKey := strings.TrimSpace(ingress.HostKey)
	if hostKey == "" {
		return "", fmt.Errorf("server advertised no SSH host key to verify against")
	}
	return hostKey, nil
}

// sshHostKeyAlias is the name every one of a project's stanzas verifies the
// server's host key under, and so the host field of its known_hosts line.
//
// One name per project rather than per address: a proxied stanza has no address
// for ssh to derive a name from, and a direct one would tie the entry to the
// address of the day. It is qualified into the same discobox-owned namespace
// the host patterns are, so it cannot be mistaken for a real hostname.
func sshHostKeyAlias(projectID string) string {
	return projectID + hostAliasSuffix
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
