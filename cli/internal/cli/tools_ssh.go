package cli

import (
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
)

func (a *App) newToolsSSHCommand(sandboxID *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ssh [DISCOBOX_ID] [SSH_ARG...]",
		Short: "Open an SSH session to a discobox",
		Long: `Open an SSH session to a discobox, over the connection this CLI already has.

The server needs no SSH port for this: ssh reaches it over the same endpoint
the API uses, by running this CLI as its ProxyCommand. Proxy, user, key, and
host verification are all supplied here, so nothing is written to your
ssh_config. The key is the one exception — it is
enrolled in the project, and reused rather than replaced on later runs.

Every argument is passed to ssh untouched, including flags. A leading argument
that names one of the discoboxes "discobox ls" shows selects it; anything else, and
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

// runToolsSSH resolves the sandbox, ensures a key, and runs ssh against it
// through the server the discobox is on.
func (a *App) runToolsSSH(cmd *cobra.Command, sandboxArg string, args []string) error {
	app, projectID, sandboxID, client, sshArgs, err := a.resolveSSHTarget(cmd, sandboxArg, args)
	if err != nil {
		return err
	}
	userOptions, remoteCommand := splitSSHArgs(sshArgs)

	session, err := app.startSSHClientSession(cmd, client, projectID)
	if err != nil {
		return err
	}
	defer session.close()

	full := append([]string{"-l", sandboxID}, session.options...)
	full = append(full, userOptions...)
	full = append(full, sshClientHost(sandboxID))
	full = append(full, remoteCommand...)
	return runSSHClient(cmd, "ssh", full, session.env)
}

// resolveSSHTarget splits the arguments into the sandbox and ssh's own
// arguments. It is resolveShellTarget's rule, for the same reason: cobra sees
// one flat list and cannot tell a sandbox reference from a tool argument.
//
// The difference from `shell` is that flag parsing is off here, so args[0] may
// be an ssh flag — `discobox tools ssh -L 8080:localhost:3000` is the ordinary
// case. A leading `-` is never a sandbox reference, and matchSandboxArg
// (inside resolveShellTarget) rejects anything that does not name one of the
// sandboxes `discobox ls` shows, so everything else reaches ssh untouched.
//
// --discobox-id wins outright when given: it was said explicitly, and then no
// argument is consumed as a sandbox at all.
//
// app is the App aimed at the server the discobox is on (selectSandbox).
func (a *App) resolveSSHTarget(cmd *cobra.Command, sandboxArg string, args []string) (app *App, projectID, sandboxID string, client *apiclientgen.Client, sshArgs []string, err error) {
	if strings.TrimSpace(sandboxArg) != "" {
		app, projectID, sandboxID, client, err = a.selectSandbox(cmd, sandboxArg)
		return app, projectID, sandboxID, client, args, err
	}
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		// An ssh flag, so there is no sandbox argument to find; the picker
		// decides, exactly as it would with no arguments at all.
		app, projectID, sandboxID, client, err = a.selectSandbox(cmd, "")
		return app, projectID, sandboxID, client, args, err
	}
	app, projectID, sandboxID, client, sshArgs, err = a.resolveShellTarget(cmd, args)
	return app, projectID, sandboxID, client, sshArgs, err
}
