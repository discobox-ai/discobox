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
	execclient "github.com/discobox-ai/discobox/execstream/client"
)

func (a *App) newToolsSSHCommand(sandboxID *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ssh [DISCOBOX_ID] [SSH_ARG...]",
		Short: "Open an SSH session to a discobox",
		Long: `Open an SSH session to a discobox, over the connection this CLI already has.

The server needs no SSH port for this: the session is carried over the same
endpoint the API uses, through a loopback port that exists only while the
command runs. Address, user, key, and host verification are all supplied here,
so nothing is written to your ssh_config. The key is the one exception — it is
enrolled in the project, and reused rather than replaced on later runs.

Every argument is passed to ssh untouched, including flags. A leading argument
that names one of this directory's discoboxes selects it; anything else, and
everything after it, belongs to ssh.`,
		Example: `  discobox tools ssh
  discobox tools ssh mybox
  discobox tools ssh -L 8080:localhost:3000
  discobox tools ssh mybox -- uname -a`,
		// Flag parsing is off entirely, not just SetInterspersed(false): ssh's
		// own flags come first in the common case (`discobox tools ssh -L ...`),
		// and cobra would reject them as unknown before ever reaching us.
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
				return cmd.Help()
			}
			return a.runToolsSSH(cmd, *sandboxID, args)
		},
	}
	return cmd
}

// runToolsSSH resolves the sandbox, ensures a key, opens the bridge, and runs
// ssh against it.
func (a *App) runToolsSSH(cmd *cobra.Command, sandboxArg string, args []string) error {
	projectID, sandboxID, client, sshArgs, err := a.resolveSSHTarget(cmd, sandboxArg, args)
	if err != nil {
		return err
	}
	userOptions, remoteCommand, background := splitSSHArgs(sshArgs)
	if background {
		// ssh -f forks and returns, and this process owns the bridge its
		// session runs over: returning here tears the bridge down under the
		// backgrounded ssh, which is why it currently "succeeds" and leaves
		// nothing behind. Backgrounding the whole command keeps the two
		// lifetimes together and leaves one process to kill.
		return fmt.Errorf("ssh -f cannot be used here: the connection is carried by this command, " +
			"so ssh must not outlive it. Background the command instead: discobox tools ssh -N ... &")
	}

	identityFile, err := a.resolveSSHIdentity(cmd, client, projectID, "")
	if err != nil {
		return err
	}
	hostKey, err := a.sshHostKey(cmd, client)
	if err != nil {
		return err
	}

	bridge, err := a.startSSHBridge(cmd.Context())
	if err != nil {
		return err
	}
	defer bridge.Close()

	knownHosts, cleanup, err := writeTemporaryKnownHosts(bridge.port(), hostKey)
	if err != nil {
		return err
	}
	defer cleanup()

	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh is not installed: %w", err)
	}
	full := append(sshBridgeArgs(bridge.port(), sandboxID, identityFile, knownHosts), userOptions...)
	full = append(full, sshBridgeHost)
	full = append(full, remoteCommand...)
	session := exec.CommandContext(cmd.Context(), sshPath, full...) //nolint:gosec // G204: this command's own arguments, plus the user's own ssh arguments.
	session.Stdin, session.Stdout, session.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	if err := session.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// The session's own status, reported the way an attached process's
			// is: ExitCode() turns this into a silent exit with that code,
			// rather than printing a wrapper's message over ssh's.
			return execclient.ExitError{Code: exitErr.ExitCode()}
		}
		return err
	}
	return nil
}

// resolveSSHTarget splits the arguments into the sandbox and ssh's own
// arguments. It is resolveShellTarget's rule, for the same reason: cobra sees
// one flat list and cannot tell a sandbox reference from a tool argument.
//
// The difference from `shell` is that flag parsing is off here, so args[0] may
// be an ssh flag — `discobox tools ssh -L 8080:localhost:3000` is the ordinary
// case. A leading `-` is never a sandbox reference, and matchSandboxArg
// (inside resolveShellTarget) rejects anything that does not name one of this
// directory's sandboxes, so everything else reaches ssh untouched.
//
// --discobox-id wins outright when given: it was said explicitly, and then no
// argument is consumed as a sandbox at all.
func (a *App) resolveSSHTarget(cmd *cobra.Command, sandboxArg string, args []string) (projectID, sandboxID string, client *apiclientgen.Client, sshArgs []string, err error) {
	if strings.TrimSpace(sandboxArg) != "" {
		projectID, sandboxID, client, err = a.selectSandbox(cmd, sandboxArg)
		return projectID, sandboxID, client, args, err
	}
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		// An ssh flag, so there is no sandbox argument to find; the picker
		// decides, exactly as it would with no arguments at all.
		projectID, sandboxID, client, err = a.selectSandbox(cmd, "")
		return projectID, sandboxID, client, args, err
	}
	return a.resolveShellTarget(cmd, args)
}

// writeTemporaryKnownHosts pins the server's host key for this command only.
// The bridge's port is different every run, so the entry is written against the
// port in use and thrown away with it: a stale entry for a reused loopback port
// would be worse than none.
func writeTemporaryKnownHosts(port int, hostKey string) (string, func(), error) {
	if strings.TrimSpace(hostKey) == "" {
		return "", nil, fmt.Errorf("server advertised no SSH host key to verify against")
	}
	file, err := os.CreateTemp("", "discobox-known-hosts-*")
	if err != nil {
		return "", nil, fmt.Errorf("create known_hosts: %w", err)
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	entry := fmt.Sprintf("%s %s\n", knownHostsHost("127.0.0.1", port), strings.TrimSpace(hostKey))
	if _, err := file.WriteString(entry); err != nil {
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
