package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/pkg/endpoint"
	execclient "github.com/discobox-ai/discobox/pkg/execstream/client"
)

// sshClientSession is what an OpenSSH client this command starts needs to
// reach a discobox: the enrolled key, a known_hosts pinning the server's host
// key, and a `ProxyCommand` that runs this executable's `admin ssh-proxy`
// against the server the discobox is on — the same way in the ssh_config
// `admin ssh-config` writes takes, spelled on the command line instead.
//
// Nothing listens and nothing is written to the user's ssh_config: ssh starts
// the proxy for each connection it makes, so `scp -3` between two discoboxes
// runs one per end, and the session outlives nothing of this process's but
// the temporary known_hosts.
type sshClientSession struct {
	// options are the ssh(1) options every client pointed at the discobox
	// needs; scp(1) passes them through as they are. Where the two differ —
	// how the user and host are named — stays with the caller.
	options []string
	// env is the environment the client runs with, which is what the
	// ProxyCommand inherits: whatever reached this App's server other than
	// --server has to reach the proxy too.
	env     []string
	cleanup func()
}

func (s *sshClientSession) close() { s.cleanup() }

// startSSHClientSession resolves the identity and host key and pins the key
// under the project's host key alias. The identity is enrolled in the project
// if it is not already, which is why this takes a client and a project rather
// than deriving them.
func (a *App) startSSHClientSession(cmd *cobra.Command, client *apiclientgen.Client, projectID string) (*sshClientSession, error) {
	ctx := cmd.Context()
	// `tools ssh` and `cp` are commands the user typed, and a key generated or
	// enrolled for them is theirs to know about, so the notes are printed where
	// the command's own reporting goes.
	identityFile, err := a.resolveSSHIdentity(ctx, client, projectID, "", printedNotes(cmd.ErrOrStderr()))
	if err != nil {
		return nil, err
	}
	hostKey, err := a.sshHostKey(ctx, client)
	if err != nil {
		return nil, err
	}
	// The alias `admin ssh-config` verifies the same key under, so the entry
	// reads the same wherever it is written. It is per project, and the "default"
	// alias would name a project no stanza does.
	resolvedProjectID, err := a.concreteProjectID(ctx, client, projectID)
	if err != nil {
		return nil, err
	}
	hostKeyAlias := sshHostKeyAlias(resolvedProjectID)
	target, err := localSSHTarget()
	if err != nil {
		return nil, err
	}
	proxyCommand, err := target.proxyCommandLine(a.serverURL)
	if err != nil {
		return nil, err
	}
	knownHosts, cleanup, err := writeTemporaryKnownHosts(hostKeyAlias, hostKey)
	if err != nil {
		return nil, err
	}
	return &sshClientSession{
		options: sshClientOptions(identityFile, knownHosts, hostKeyAlias, proxyCommand),
		env:     a.sshProxyEnv(),
		cleanup: cleanup,
	}, nil
}

// sshClientOptions are the options every OpenSSH client pointed at a discobox
// needs beyond its user and host: how to reach the server, which key to offer,
// and which host key to accept.
func sshClientOptions(identityFile, knownHostsFile, hostKeyAlias, proxyCommand string) []string {
	return []string{
		"-i", identityFile,
		// Without IdentitiesOnly, ssh offers every agent key before this one
		// and can exhaust MaxAuthTries before reaching it.
		"-o", "IdentitiesOnly=yes",
		// ssh reads a -o argument as a config line, so every value below is
		// spelled the way the written ssh_config spells it: the ProxyCommand
		// is the rest of the line and percent-expanded, and a path is split on
		// whitespace as well — a temp directory under "C:\Users\Ada Lovelace"
		// would otherwise arrive as two filenames neither of which exists. The
		// -i above is neither, and is passed as it is.
		"-o", "ProxyCommand=" + escapeSSHPercent(proxyCommand),
		// The host ssh is given is only an alias with no address behind it, so
		// the key is looked up under the project's alias rather than under it,
		// in a file written for this command alone: verification is real
		// without touching the user's known_hosts.
		"-o", "HostKeyAlias=" + hostKeyAlias,
		"-o", "UserKnownHostsFile=" + sshConfigPath(knownHostsFile),
		"-o", "StrictHostKeyChecking=yes",
		// The user's ssh_config has nothing to say about a connection this
		// command resolved, and a stray `Host *` block there could otherwise
		// override the identity, user or proxy just chosen.
		"-F", "none",
	}
}

// sshClientHost is the host a client is pointed at for a discobox: the
// qualified alias `admin ssh-config` gives it, which resolves to nothing — the
// ProxyCommand is what connects — and reads as the discobox in ssh's messages.
// The user, which is what the server routes by, is the discobox ID beside it.
func sshClientHost(sandboxID string) string {
	return sandboxID + hostAliasSuffix
}

// sshProxyEnv is the environment the ProxyCommand runs with. Its command line
// names the server and nothing else, since ssh's argv is readable by every
// process on the machine; the token and the iroh settings this App dials with
// travel here, set outright so that what an App aimed at a registered server
// dropped (forServer) is not put back by what this process inherited.
func (a *App) sshProxyEnv() []string {
	return append(os.Environ(),
		"DISCOBOX_TOKEN="+a.token,
		"DISCOBOX_IROH_RELAY_URLS="+a.irohRelayURLs,
		endpoint.IrohLogEnv+"="+a.irohLogLevel,
	)
}

// runSSHClient runs an OpenSSH client attached to this terminal and reports
// its exit status as this command's own.
func runSSHClient(cmd *cobra.Command, binary string, args, env []string) error {
	path, err := exec.LookPath(binary)
	if err != nil {
		return fmt.Errorf("%s is not installed: %w", binary, err)
	}
	session := exec.CommandContext(cmd.Context(), path, args...) //nolint:gosec // G204: this command's own arguments, plus the user's own client arguments.
	session.Stdin, session.Stdout, session.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	session.Env = env
	if err := session.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// The session's own status, reported the way an attached process's
			// is: ExitCode() turns this into a silent exit with that code,
			// rather than printing a wrapper's message over the client's.
			return execclient.ExitError{Code: exitErr.ExitCode()}
		}
		return err
	}
	return nil
}

// writeTemporaryKnownHosts pins the server's host key for this command only,
// under alias, and returns the file and what removes it.
func writeTemporaryKnownHosts(alias, hostKey string) (string, func(), error) {
	if strings.TrimSpace(hostKey) == "" {
		return "", nil, fmt.Errorf("server advertised no SSH host key to verify against")
	}
	file, err := os.CreateTemp("", "discobox-known-hosts-*")
	if err != nil {
		return "", nil, fmt.Errorf("create known_hosts: %w", err)
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	if _, err := fmt.Fprintf(file, "%s %s\n", alias, strings.TrimSpace(hostKey)); err != nil {
		_ = file.Close()
		cleanup()
		return "", nil, fmt.Errorf("write known_hosts: %w", err)
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("write known_hosts: %w", err)
	}
	return filepath.Clean(path), cleanup, nil
}
