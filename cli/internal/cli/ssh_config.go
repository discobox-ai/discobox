package cli

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

func (a *App) newSSHConfigCommand() *cobra.Command {
	var identityFile string
	var write bool
	cmd := &cobra.Command{
		Use:   "ssh-config",
		Short: "Emit an SSH client config for this project's discoboxes",
		Long: "Emit ssh_config(5) Host stanzas — one per discobox in the current project — plus\n" +
			"the server's known_hosts line, suitable for `discobox admin ssh-config >> ~/.ssh/config`\n" +
			"or an ssh_config Include directive.\n\n" +
			"With --write, the stanzas and the server's host key are written to files this\n" +
			"command owns and rewrites, and ~/.ssh/config gains a single Include line pointing\n" +
			"at them. Nothing else in ~/.ssh is edited.\n\n" +
			"--write covers every server: the primary and the ones `discobox servers` lists,\n" +
			"each into its own files. Without it, the stanzas printed are the primary's, since\n" +
			"what is printed is one block to paste; --server picks another.\n\n" +
			"On WSL that happens twice, once for each of the machine's two ssh installations:\n" +
			"this distribution's, and the Windows one that a Windows VS Code or JetBrains\n" +
			"Gateway drives. Without --write, the printed stanzas are this side's.\n\n" +
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
			// The user typed this command, so what it does on their behalf is
			// theirs to read: these notes are printed where the command's own
			// reporting goes.
			notes := printedNotes(cmd.ErrOrStderr())
			if write {
				return a.writeEverySSHConfig(cmd.Context(), identityFile, notes)
			}
			// The written files are named after the project and the host key
			// is verified under a name derived from it, so resolve what the
			// flag means either way: "default" is a server-side alias, and the
			// same project reached as "default" and by ID must not end up
			// owning two files, two Include lines, and two known_hosts names.
			resolvedProjectID, err := a.concreteProjectID(cmd.Context(), client, projectID)
			if err != nil {
				return err
			}

			hostKey, err := a.sshHostKey(cmd.Context(), client)
			if err != nil {
				return err
			}
			// Every ssh on this machine, which on WSL is two. A config that
			// cannot be written for the Windows side is worth saying so about
			// and no reason to withhold this side's.
			targets, err := machineSSHTargets(cmd.Context())
			if err != nil {
				return err
			}
			if targets.windowsErr != nil {
				notes(windowsSSHConfigSkipped, targets.windowsErr)
			}
			// Printed output is for pasting into a config by hand, and the
			// hand doing it is on this side.
			built, err := a.buildManagedSSHConfig(cmd.Context(), managedSSHConfigRequest{
				client:            client,
				projectID:         projectID,
				resolvedProjectID: resolvedProjectID,
				identityFile:      identityFile,
				hostKey:           hostKey,
				write:             write,
				notes:             notes,
			}, targets.all[:1])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprint(out, built[0].stanzas)
			fmt.Fprintf(out, "\n# add to your known_hosts:\n# %s %s\n", built[0].hostKeyAlias, built[0].hostKey)
			return nil
		},
	}
	cmd.Flags().StringVar(&identityFile, "identity-file", "", "Private key to use, generated and enrolled if absent (default: the CLI's own managed key)")
	cmd.Flags().BoolVarP(&write, "write", "w", false, "Write the config where ssh will find it, instead of printing it")
	return cmd
}

// writeEverySSHConfig syncs the stanzas of every server this client lists:
// each server's own project, into files named by that project, so one ssh
// reaches the discoboxes on all of them (ADR 0116 §4).
//
// The primary failing fails the command, as it did when it was the only server
// there was; a registered server that cannot be reached is a note, and every
// other server is still written. Which server a note is about is said where
// there is more than one: the paths it names are project IDs, which nobody
// reads as a server.
func (a *App) writeEverySSHConfig(ctx context.Context, identityFile string, notes noteFunc) error {
	set, err := a.servers()
	if err != nil {
		return err
	}
	for _, s := range set {
		about := notes
		if len(set) > 1 {
			about = func(format string, args ...any) {
				notes("%s: "+format, append([]any{s.name}, args...)...)
			}
		}
		if err := s.writeSSHConfig(ctx, identityFile, about); err != nil {
			if s.primary {
				return err
			}
			notes("could not sync the SSH config for %s: %v", s.name, err)
		}
	}
	return nil
}

// sshSyncTimeout bounds syncing one registered server's stanzas. It is longer
// than a listing's bound because the work is longer: a key enrolled on that
// server, a listing, and on WSL two ssh installations written.
const sshSyncTimeout = time.Minute

// writeSSHConfig syncs one server's stanzas, in that server's own project.
func (s *server) writeSSHConfig(ctx context.Context, identityFile string, notes noteFunc) error {
	ctx, cancel := s.boundedBy(ctx, sshSyncTimeout)
	defer cancel()
	if _, err := s.app.resolveServerAddress(ctx); err != nil {
		return err
	}
	projectID, err := s.app.projectIDValue()
	if err != nil {
		return err
	}
	client, err := s.app.apiClient()
	if err != nil {
		return err
	}
	return s.app.writeProjectSSHConfig(ctx, client, projectID, identityFile, notes)
}

// writeProjectSSHConfig is the one operation behind both `admin ssh-config
// --write` and the automatic refresh after a prompt sandbox is created. On WSL
// machineSSHTargets returns this distribution and Windows, so every caller
// keeps both ssh installations in sync.
//
// It takes a context and a note sink rather than the command, so that what it
// says goes where the caller is showing things rather than onto whatever stream
// the command happens to hold. A create runs this on its way to a full-screen
// terminal — the launcher's window, or the attach `discobox run` ends in — and
// neither of those has a stream to spare.
func (a *App) writeProjectSSHConfig(ctx context.Context, client *apiclientgen.Client, projectID, identityFile string, notes noteFunc) error {
	resolvedProjectID, err := a.concreteProjectID(ctx, client, projectID)
	if err != nil {
		return err
	}
	hostKey, err := a.sshHostKey(ctx, client)
	if err != nil {
		return err
	}
	targets, err := machineSSHTargets(ctx)
	if err != nil {
		return err
	}
	if targets.windowsErr != nil {
		notes(windowsSSHConfigSkipped, targets.windowsErr)
	}
	_, err = a.writeManagedSSHConfigs(ctx, managedSSHConfigRequest{
		client:            client,
		projectID:         projectID,
		resolvedProjectID: resolvedProjectID,
		identityFile:      identityFile,
		hostKey:           hostKey,
		write:             true,
		notes:             notes,
	}, targets.all)
	return err
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
	// notes is where the key enrollment says what it did. It is required: this
	// work generates and enrolls keys on the caller's behalf, and no caller may
	// have that happen silently.
	notes noteFunc
}

// managedSSHConfig is a project's emitted ssh_config: the stanzas, the name the
// host key is verified under, the key itself, and the Host pattern each sandbox
// answers to.
//
// The aliases are what a caller handing a host to another program needs —
// `discobox tools vscode` builds a Remote-SSH target out of one, `tools zed` an
// `ssh://` URL — and they cannot be guessed from outside, since a contested
// pattern is dropped from every stanza that wanted it.
type managedSSHConfig struct {
	// target is the ssh installation this rendering is for: where its files
	// go, how their paths are spelled, and what its ProxyCommand runs.
	target       sshTarget
	stanzas      string
	hostKeyAlias string
	hostKey      string
	aliases      map[string]string
}

// buildManagedSSHConfig resolves the key, lists the project's sandboxes, and
// renders the stanzas once per target. It does not decide what to do with them:
// `ssh-config` prints or writes them, and the editor commands write them and
// open an editor on one.
//
// The project is listed and the identity resolved once for all of them: what
// differs between one ssh installation and the next is how the stanzas spell
// what they name, not which sandboxes exist or which key authenticates.
func (a *App) buildManagedSSHConfig(ctx context.Context, req managedSSHConfigRequest, targets []sshTarget) ([]managedSSHConfig, error) {
	identityFile, err := a.resolveSSHIdentity(ctx, req.client, req.projectID, req.identityFile, req.notes)
	if err != nil {
		return nil, err
	}
	sandboxesRes, err := req.client.ListSandboxes(ctx, apiclientgen.ListSandboxesParams{ProjectId: req.projectID})
	if err != nil {
		return nil, err
	}
	sandboxesBody, err := expectResponse[apimodel.ListSandboxesBody](sandboxesRes)
	if err != nil {
		return nil, err
	}
	sandboxes := sandboxesBody.GetSandboxes()

	// The first surviving pattern is the alias to hand out: they are emitted
	// friendliest-first, so this is the sandbox's name where the name is
	// unambiguous and its ID where it is not. It is the same alias whichever
	// ssh reads the stanza.
	patterns := sshConfigHostPatterns(sandboxes)
	aliases := make(map[string]string, len(sandboxes))
	for i, sandbox := range sandboxes {
		if len(patterns[i]) > 0 {
			aliases[sandbox.ID] = patterns[i][0]
		}
	}

	buildTarget := func(target sshTarget) (managedSSHConfig, error) {
		proxyCommand, err := target.proxyCommandLine(a.serverURL)
		if err != nil {
			return managedSSHConfig{}, err
		}
		// The key is generated and enrolled on this side whichever ssh reads
		// the config; only where it has to be readable from changes.
		identity, err := target.mirrorSSHIdentity(ctx, identityFile)
		if err != nil {
			return managedSSHConfig{}, err
		}
		// Only the written config can name a known_hosts file, because only it
		// owns one: printed output would be naming a file this run never wrote.
		knownHostsFile := ""
		if req.write {
			knownHostsFile = target.knownHostsPath(req.resolvedProjectID).client
		}
		render := sshConfigRender{
			sandboxes:      sandboxes,
			proxyCommand:   proxyCommand,
			hostKeyAlias:   sshHostKeyAlias(req.resolvedProjectID),
			identityFile:   identity.client,
			knownHostsFile: knownHostsFile,
		}
		return managedSSHConfig{
			target:       target,
			stanzas:      renderSSHConfig(render),
			hostKeyAlias: render.hostKeyAlias,
			hostKey:      req.hostKey,
			aliases:      aliases,
		}, nil
	}

	built := make([]managedSSHConfig, 0, len(targets))
	for _, target := range targets {
		config, err := buildTarget(target)
		if err != nil {
			if !target.optional {
				return nil, err
			}
			req.notes(windowsSSHConfigSkipped, err)
			continue
		}
		built = append(built, config)
	}
	return built, nil
}

// windowsSSHConfigSkipped is what a target that may fail says when it does. It
// is one sentence in one wording wherever the failure happens — resolving the
// target, mirroring the key, writing the files — because it is one thing the
// reader has to know: this machine's other ssh installation did not get the
// stanzas, and here is why. See sshTarget.optional.
const windowsSSHConfigSkipped = "not writing the Windows ssh_config: %v"

// writeManagedSSHConfigs renders a project's stanzas for every target and puts
// each where that ssh will read them. It is what every caller that writes does,
// as against `admin ssh-config` with no --write, which renders one target and
// prints it.
//
// A target that may fail and does is reported and dropped rather than failing
// the caller: what comes back is the targets that were written, this machine's
// own first. See sshTarget.optional.
func (a *App) writeManagedSSHConfigs(ctx context.Context, req managedSSHConfigRequest, targets []sshTarget) ([]managedSSHConfig, error) {
	built, err := a.buildManagedSSHConfig(ctx, req, targets)
	if err != nil {
		return nil, err
	}
	written := make([]managedSSHConfig, 0, len(built))
	for _, config := range built {
		if err := writeManagedSSHConfig(ctx, config, req.resolvedProjectID, req.notes); err != nil {
			if !config.target.optional {
				return nil, err
			}
			req.notes(windowsSSHConfigSkipped, err)
			continue
		}
		written = append(written, config)
	}
	return written, nil
}

// sshHostKey is the server's host public key, which every emitted stanza pins
// and nothing else in the document needs.
//
// `GET /ssh` does not say whether the server serves SSH, or at what address: it
// serves SSH over the transport the API already answers on, and that is the
// only way in (ADR 0057).
func (a *App) sshHostKey(ctx context.Context, client *apiclientgen.Client) (string, error) {
	res, err := client.GetSSHIngress(ctx)
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
// (idx_sandbox_project_name), so name-versus-name collisions do not happen;
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
func (a *App) resolveSSHIdentity(ctx context.Context, client *apiclientgen.Client, projectID, explicitPath string, notes noteFunc) (string, error) {
	enrolled, err := a.listEnrolledFingerprints(ctx, client, projectID)
	if err != nil {
		return "", err
	}

	if strings.TrimSpace(explicitPath) != "" {
		return explicitPath, a.enrollSSHIdentity(ctx, client, projectID, explicitPath, enrolled, notes)
	}
	managed := defaultSSHIdentityPath()
	if fileExists(managed) {
		return managed, a.enrollSSHIdentity(ctx, client, projectID, managed, enrolled, notes)
	}
	if reusable := enrolledLocalKey(enrolled); reusable != "" {
		notes("using %s, already enrolled in this project", reusable)
		return reusable, nil
	}
	return managed, a.enrollSSHIdentity(ctx, client, projectID, managed, enrolled, notes)
}

func (a *App) listEnrolledFingerprints(ctx context.Context, client *apiclientgen.Client, projectID string) (map[string]bool, error) {
	res, err := client.ListSSHKeys(ctx, apiclientgen.ListSSHKeysParams{ProjectId: projectID})
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
func (a *App) enrollSSHIdentity(ctx context.Context, client *apiclientgen.Client, projectID, path string, enrolled map[string]bool, notes noteFunc) error {
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
		notes("generated a new SSH key at %s (%s)", path, fingerprint)
	}
	if enrolled[fingerprint] {
		return nil
	}

	createBody := &apimodel.CreateSSHKeyBody{PublicKey: publicKeyLine}
	createBody.SetName(apiclientgen.NewOptString(sshIdentityComment()))
	createRes, err := client.CreateSSHKey(ctx, createBody, apiclientgen.CreateSSHKeyParams{ProjectId: projectID})
	if err != nil {
		return err
	}
	if _, err := expectResponse[apimodel.SSHKey](createRes); err != nil {
		return err
	}
	notes("enrolled SSH key %s in this project", fingerprint)
	return nil
}

// sandboxSSHRemote is one discobox as the rest of the world reaches it: the
// ssh host that gets there, and the directory in it worth pointing at.
//
// It refreshes the project's managed ssh_config on the way, because the host
// only exists in that file: nothing here is an address, and every program that
// speaks ssh — VS Code's Remote-SSH, `git clone`, `ssh` itself — finds a host
// by reading ssh_config and nothing else. Writing the whole project's stanzas
// rather than this sandbox's is what `ssh-config --write` already means by that
// file — it is rewritten wholesale on every run — and it leaves every other
// sandbox reachable too.
type sandboxSSHRemote struct {
	host   string
	folder string
}

func (t sandboxSSHRemote) describe() string {
	if t.folder == "" {
		return t.host
	}
	return t.host + ":" + t.folder
}

// gitURL is the remote's working tree as git takes it: an ssh URL whose
// authority is the ssh_config host, so git hands the whole connection —
// ProxyCommand, identity, host key — back to the ssh that already knows how to
// reach this discobox.
//
// Built rather than pasted together so a workdir with a space or a percent sign
// in it survives being one, the same reason vscodeFolderURI is. Empty when
// there is no directory to name, since a URL to the run user's home is a clone
// of nothing.
func (t sandboxSSHRemote) gitURL() string {
	if t.folder == "" {
		return ""
	}
	uri := url.URL{Scheme: "ssh", Host: t.host, Path: t.folder}
	return uri.String()
}

// zedURL is the remote's working tree as Zed takes it: an `ssh://` URL whose
// authority is the ssh_config host, so Zed — which shells out to the ssh on
// PATH — hands the whole connection back to the ssh that already knows how to
// reach this discobox. No user and no port, because the stanza carries both.
//
// Built with net/url like gitURL, and for a second reason here: Zed parses the
// URL and percent-decodes the path, so a workdir with a space in it has to
// arrive encoded rather than raw.
//
// A URL rather than a path argument on every platform, for the reason
// vscodeFolderURI is a URI (ADR 0074 §4): Zed's launcher passes anything
// beginning with a known scheme through untouched, while a bare path it
// resolves against the machine it is running on. Launched from WSL, Zed is
// normally the Windows build, and /home/agent/repo there is a Windows path
// that does not exist — unlike VS Code it does not translate one into the
// distribution, it simply opens nothing.
//
// The discobox's root when there is no known working tree. Zed's URL has
// nowhere to say "connected, nothing open" the way VS Code's --remote does, and
// a window on / is a connected window whose tree is the box.
func (t sandboxSSHRemote) zedURL() string {
	folder := t.folder
	if folder == "" {
		folder = "/"
	}
	uri := url.URL{Scheme: "ssh", Host: t.host, Path: folder}
	return uri.String()
}

// sandboxSSHRemote refreshes the project's managed ssh_config for every target
// and works out how this sandbox is reached in it. It is what `tools vscode`
// and `tools zed` point an editor at, and what the launcher's tools picker
// prints for copying:
// the same three questions — which config, which alias, which directory — and
// the same write behind them.
func (a *App) sandboxSSHRemote(ctx context.Context, targets []sshTarget, client *apiclientgen.Client, projectID, sandboxID, sourceSlug string, notes noteFunc) (sandboxSSHRemote, error) {
	resolvedProjectID, err := a.concreteProjectID(ctx, client, projectID)
	if err != nil {
		return sandboxSSHRemote{}, err
	}
	hostKey, err := a.sshHostKey(ctx, client)
	if err != nil {
		return sandboxSSHRemote{}, err
	}
	built, err := a.writeManagedSSHConfigs(ctx, managedSSHConfigRequest{
		client:            client,
		projectID:         projectID,
		resolvedProjectID: resolvedProjectID,
		hostKey:           hostKey,
		write:             true,
		notes:             notes,
	}, targets)
	if err != nil {
		return sandboxSSHRemote{}, err
	}

	host, ok := built[0].aliases[sandboxID]
	if !ok {
		// Every spelling of this sandbox was claimed by another one, so the
		// config carries no stanza it could be reached by. See
		// sshConfigHostPatterns.
		return sandboxSSHRemote{}, fmt.Errorf("discobox %s has no unambiguous SSH host alias; rename it or the discobox whose name spells its ID", sandboxID)
	}
	folder, err := a.sandboxSSHFolder(ctx, client, projectID, sandboxID, sourceSlug)
	if err != nil {
		return sandboxSSHRemote{}, err
	}
	return sandboxSSHRemote{host: host, folder: folder}, nil
}

// sandboxSSHFolder is the directory in the sandbox an ssh-driven program is
// pointed at.
//
// An SSH session lands in the run user's home directory rather than the
// sandbox's exec default, so unlike `discobox tools git` this cannot leave the
// directory unsaid: without one, VS Code would open a window on the home
// directory and the working tree would be somewhere else, and a git URL would
// name a directory that is not a repository. Empty is still possible — a
// sandbox may not have told us where its source landed — and then VS Code opens
// on the host with no folder, which is its own way of saying "connected,
// nothing open", and there is no git URL to print at all.
func (a *App) sandboxSSHFolder(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID, sourceSlug string) (string, error) {
	if sourceSlug != "" {
		return a.toolSourceWorkdir(ctx, client, projectID, sandboxID, sourceSlug)
	}
	res, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return "", err
	}
	sandbox, err := expectResponse[apimodel.Sandbox](res)
	if err != nil {
		return "", err
	}
	sources := applySources(sandbox)
	if len(sources) == 0 {
		return "", nil
	}
	return sourceWorkdir(sources[0].source), nil
}
