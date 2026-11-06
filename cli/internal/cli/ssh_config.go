package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/internal/filelock"
	idpkg "github.com/discobox-ai/x/id"
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
			"--write covers every server: the primary and the ones `discobox admin remote`\n" +
			"lists, each into its own files. Without it, the stanzas printed are the\n" +
			"primary's, since what is printed is one block to paste; --server picks another.\n\n" +
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
	err = a.writeManagedSSHConfigs(ctx, managedSSHConfigRequest{
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

// syncSSHConfigAfterCreate is the sync a create ends with, so that a person can
// `ssh` to the discobox they just made. A caller the server refuses it (403) —
// a discobox creating another, which may not enroll a key that would reach
// every discobox in the project — has no use for it, and the create stands
// (ADR 26-09-24-630 §4). `admin ssh-config --write` calls writeProjectSSHConfig
// directly: there the sync is what was asked for, and a refusal is its answer.
func (a *App) syncSSHConfigAfterCreate(ctx context.Context, client *apiclientgen.Client, projectID string, notes noteFunc) error {
	err := a.writeProjectSSHConfig(ctx, client, projectID, "", notes)
	var refused *plainStatusError
	if errors.As(err, &refused) && refused.code == http.StatusForbidden {
		notes("SSH config not synced: the server refused it (%s)", refused.body)
		return nil
	}
	if err != nil {
		return fmt.Errorf("sync SSH config: %w", err)
	}
	return nil
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
// host key is verified under, and the key itself. The host each sandbox answers
// to needs no lookup in it: it is the sandbox's ID (sshConfigHostPatterns).
type managedSSHConfig struct {
	// target is the ssh installation this rendering is for: where its files
	// go, how their paths are spelled, and what its ProxyCommand runs.
	projectID    string
	namespace    string
	target       sshTarget
	stanzas      string
	proxyCommand string
	hostKeyAlias string
	hostKey      string
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
			knownHostsFile = target.knownHostsPath(sshConfigNamespace(a.serverURL, req.resolvedProjectID)).client
		}
		render := sshConfigRender{
			sandboxes:      sandboxes,
			namespace:      sshConfigNamespace(a.serverURL, req.resolvedProjectID),
			proxyCommand:   proxyCommand,
			hostKeyAlias:   sshHostKeyAlias(sshConfigNamespace(a.serverURL, req.resolvedProjectID)),
			identityFile:   identity.client,
			knownHostsFile: knownHostsFile,
		}
		return managedSSHConfig{
			target:       target,
			projectID:    req.resolvedProjectID,
			namespace:    sshConfigNamespace(a.serverURL, req.resolvedProjectID),
			stanzas:      renderSSHConfig(render),
			proxyCommand: proxyCommand,
			hostKeyAlias: render.hostKeyAlias,
			hostKey:      req.hostKey,
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
// A target that may fail and does is reported and passed over rather than
// failing the caller. See sshTarget.optional.
//
// The writes are made under sshConfigLockName. Each write also re-keys every
// other project's file (rekeyManagedSSHConfigs), a read-modify-write of a file
// another command may be writing that moment: without the lock a re-key that
// read the old file could put it back over the stanzas that command just
// wrote, a discobox it created among them.
func (a *App) writeManagedSSHConfigs(ctx context.Context, req managedSSHConfigRequest, targets []sshTarget) error {
	built, err := a.buildManagedSSHConfig(ctx, req, targets)
	if err != nil {
		return err
	}
	dir := filepath.Join(cliStateDir(), "ssh")
	if err := ensureStateDir(dir); err != nil {
		return err
	}
	lock, err := acquireFileLock(filepath.Join(dir, sshConfigLockName), sshConfigLockWait)
	if errors.Is(err, filelock.ErrBusy) {
		return errors.New("another discobox command is still writing the SSH config; try again")
	}
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	for _, config := range built {
		if err := writeManagedSSHConfig(ctx, config, req.notes); err != nil {
			if !config.target.optional {
				return err
			}
			req.notes(windowsSSHConfigSkipped, err)
		}
	}
	return nil
}

// sshConfigLockName is held across writing the managed ssh_config files, in
// this machine's own state directory whichever ssh reads them: every command
// writing them, for either side of a WSL machine, runs here.
const sshConfigLockName = "config.lock"

// sshConfigLockWait is how long one write waits for another. A write across
// the WSL boundary restricts its files through Windows tools, which takes
// seconds rather than milliseconds.
const sshConfigLockWait = 30 * time.Second

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

// sshHostKeyAlias is the name a server/project namespace verifies its host key
// under, and the host field in its known_hosts line. The endpoint hash keeps
// independently routed servers with identical project IDs from sharing trust.

func sshHostKeyAlias(namespace string) string {
	return namespace + hostAliasSuffix
}

// hostAliasSuffix qualifies each pattern into an obviously discobox-owned
// namespace. Every sandbox gets both the bare form and the qualified one: the
// bare ID is what anyone pastes after `ssh`, and the qualified alias is the
// spelling to fall back on should a bare one collide with a real host elsewhere
// in the user's ssh_config.
const hostAliasSuffix = ".discobox.internal"

// sshConfigNamespace separates independently routed servers even when their
// restored databases contain the same project and sandbox IDs. Endpoint text
// is hashed so it cannot introduce ssh_config syntax or path components.
func sshConfigNamespace(serverURL, projectID string) string {
	sum := sha256.Sum256([]byte(serverURL))
	return projectID + "-" + hex.EncodeToString(sum[:16])
}
func sshConfigHost(namespace, sandboxID string) string {
	return sandboxID + "." + namespace + hostAliasSuffix
}

// sshConfigHostPatterns returns a sandbox's Host patterns: its ID, each bare
// and suffixed. The ID comes in both spellings — "sbx_…" as the CLI prints it
// and "sbx-…" as the discobox's own hostname does (id.Hostname) — so either one
// pasted after `ssh` connects.
//
// The sandbox's name is never a pattern. A generated name is neither stable nor
// unique the way an ID is: a renamed discobox would keep its old host until the
// next sync, a name can spell another discobox's ID, and two servers' discoboxes
// can share one. Every spelling derives from the ID and, for the final
// qualified pattern, the server/project namespace. Host tools use that pattern
// so identical IDs on separate servers cannot redirect them.
func sshConfigHostPatterns(namespace, sandboxID string) []string {
	patterns := []string{sandboxID, sandboxID + hostAliasSuffix}
	if hostname := idpkg.Hostname(sandboxID); hostname != sandboxID {
		patterns = append(patterns, hostname, hostname+hostAliasSuffix)
	}
	if namespace != "" {
		patterns = append(patterns, sshConfigHost(namespace, sandboxID))
	}
	return patterns
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
	// workingRoot is the discobox's working root, by its platform: where a
	// tool opens when folder is empty.
	workingRoot string
	// projectID is the discobox's project as the server resolved it, never
	// the `default` alias the command may have been given.
	projectID string
}

func (t sandboxSSHRemote) describe() string {
	if t.folder == "" {
		return t.host
	}
	return t.host + ":" + t.folder
}

// sandboxSSHRemote refreshes the project's managed ssh_config for every target
// and works out how this sandbox is reached in it. It is what a tool that runs
// on this machine is handed (ADR 0125 §4), and what the launcher's tools picker
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
	err = a.writeManagedSSHConfigs(ctx, managedSSHConfigRequest{
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

	res, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return sandboxSSHRemote{}, err
	}
	sandbox, err := expectResponse[apimodel.Sandbox](res)
	if err != nil {
		return sandboxSSHRemote{}, err
	}
	folder, err := sandboxSSHFolder(sandbox, sourceSlug)
	if err != nil {
		return sandboxSSHRemote{}, err
	}
	// A host tool gets the server/project-qualified pattern. Bare ID aliases
	// remain convenient for manual ssh, but cannot select a cross-server target.
	return sandboxSSHRemote{host: sshConfigHost(sshConfigNamespace(a.serverURL, resolvedProjectID), sandbox.ID), folder: folder, workingRoot: sandboxPaths(sandbox).WorkingRoot(), projectID: resolvedProjectID}, nil
}

// sandboxSSHFolder is the directory in the sandbox an ssh-driven program is
// pointed at.
//
// An SSH session lands in the run user's home directory rather than the
// sandbox's exec default, so unlike `discobox tools git` this cannot leave the
// directory unsaid: without one, VS Code would open a window on the home
// directory and the working tree would be somewhere else, and a git URL would
// name a directory that is not a repository. A primary source without an
// explicit destination uses the working root of the sandbox's platform. Empty
// means the sandbox has no primary source: an editor still opens on the
// working root (tools.Remote), but there is no git URL to print.
//
// The folder comes from the server's record, which the client does not write
// alone -- a box can create boxes, and an archive is anyone's -- and every
// host tool takes it as a path on the far side of an SSH host: an editor's
// `ssh-remote+<host><folder>` URL, `{workdir}` in a command. One that is not
// an absolute path in the sandbox could name a host there instead, so it is
// refused here, for every source and every record however old
// (ADR 26-10-15-417).
func sandboxSSHFolder(sandbox *apimodel.Sandbox, sourceSlug string) (string, error) {
	var folder string
	if sourceSlug != "" {
		dir, err := sourceSlugWorkdir(sandbox, sourceSlug)
		if err != nil {
			return "", err
		}
		folder = dir
	} else {
		source, ok := sandbox.Config.Source.Get()
		if !ok {
			return "", nil
		}
		folder = sourceWorkdir(source)
	}
	paths := sandboxPaths(sandbox)
	if folder == "" {
		return paths.WorkingRoot(), nil
	}
	if !paths.IsAbs(folder) || strings.HasPrefix(folder, `\\`) || strings.IndexFunc(folder, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("the discobox records %q as its source's directory, which is not an absolute path in it; refusing to hand it to a tool on this machine", folder)
	}
	return folder, nil
}
