package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func (a *App) newToolsSSHCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ssh [DISCOBOX] [-- SSH_ARG...]",
		Short: "Open an SSH session to a discobox",
		Long: `Open an SSH session to a discobox, over the connection this CLI already has.

The server needs no SSH port for this: ssh reaches it over the same endpoint
the API uses, by running this CLI as its ProxyCommand. Proxy, user, key, and
host verification are all supplied here, so nothing is written to your
ssh_config. The key is the one exception — it is
enrolled in the project, and reused rather than replaced on later runs.

DISCOBOX is the NAME "discobox ls" prints, an ID, or a short prefix of one;
without it the only discobox there is, or a pick. Everything after -- is passed
to ssh untouched, its options and then the remote command, as ssh reads them.`,
		Example: `  discobox tools ssh
  discobox tools ssh mybox
  discobox tools ssh -- -L 8080:localhost:3000
  discobox tools ssh mybox -- uname -a`,
		// Flag parsing is off entirely: everything after the -- is ssh's, and
		// cobra would reject ssh's flags as unknown before ever reaching us.
		DisableFlagParsing: true,
		ValidArgsFunction:  a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
				return cmd.Help()
			}
			// With no flag parsing, an ssh option written where DISCOBOX goes
			// reaches here as one; it is said where it goes rather than looked
			// for as a discobox.
			if len(args) > 0 && strings.HasPrefix(args[0], "-") && args[0] != "--" {
				return fmt.Errorf("ssh's arguments go after --: %s -- %s", cmd.CommandPath(), strings.Join(args, " "))
			}
			return a.runToolsSSH(cmd, args)
		},
	}
	return cmd
}

// runToolsSSH resolves the sandbox, ensures a key, and runs ssh against it
// through the server the discobox is on.
func (a *App) runToolsSSH(cmd *cobra.Command, args []string) error {
	app, projectID, sandboxID, client, sshArgs, err := a.selectDiscoboxCommand(cmd, args)
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
