package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
	"github.com/discobox-ai/discobox/cli/internal/tui"
	"github.com/discobox-ai/discobox/internal/hostid"
	"github.com/discobox-ai/discobox/pkg/endpoint"
	idpkg "github.com/discobox-ai/x/id"
)

// newAttachCommand implements `discobox attach`: open a discobox's own screen,
// which is the launcher's window on that one discobox and nothing else. It is
// the everyday shortcut for what pressing Enter on its row in `discobox console`
// does, and --raw is what `discobox admin terminal attach DISCOBOX primary`
// spells out in full.
func (a *App) newAttachCommand() *cobra.Command {
	var raw bool
	cmd := &cobra.Command{
		Use:   "attach [DISCOBOX]",
		Short: "Open a discobox: its terminal and what runs beside it",
		Long: `Open a discobox: its primary terminal, and everything running beside it.

Without DISCOBOX the discobox is taken from the ones "discobox ls" shows: the
only one when there is one, otherwise you are asked to pick.

The primary terminal is the discobox's default session: the configured harness,
or a plain shell when it has none. Attaching relaunches it with the harness's
relaunch command when it has stopped.

The window is the console's, opened on this discobox alone: the primary
terminal, the shells and services running beside it, its forwarded ports, and
the same keys that screen has in "discobox console". The leader key then d — Ctrl-A
d by default, and Ctrl-A Ctrl-D works too — leaves it, which detaches and exits;
every session keeps running. Set DISCOBOX_LEADER to change the Ctrl-A when it
collides with what you run: another Ctrl letter, or an Alt one such as alt+g
("discobox console --help" says what an Alt leader needs of your terminal).

--raw attaches this terminal to the primary terminal directly, with no window
around it. Stdin is always attached, and a PTY is allocated the same way the
discobox's default terminal already runs one, so a pipe, a recording, or a
terminal you would rather keep as it is gets the stream and nothing else. Ctrl-A
d detaches there too. If an interrupt stops getting through — the discobox or
the server has gone quiet — Ctrl-C again says so, and one more quits. The
terminal keeps running, exactly as a detach leaves it. The end of stdin detaches
too, so a script's attach ends when its input does. SIGTERM, SIGINT and SIGHUP
sent to this command are passed on to the program in front on the primary
terminal, as a local terminal would deliver them: they end that program, not
the attach, which stays on the terminal's shell until it detaches or its input
ends. Without a terminal to
draw a window on, attach is raw whether or not the flag was given.`,
		Example: `  discobox attach
  discobox attach sbx_01hq
  discobox attach --raw sbx_01hq`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			var sandboxArg string
			if len(args) > 0 {
				sandboxArg = args[0]
			}
			if err := a.refuseOwnSandbox(sandboxArg); err != nil {
				return err
			}
			// Everything below talks to the server the discobox is on.
			a, projectID, sandboxID, client, err := a.selectSandbox(cmd, sandboxArg)
			if err != nil {
				return err
			}
			if !raw && canOpenWindow(cmd) {
				// The window is opened on the discobox as the server has it,
				// so its header names the box, its branch and its diff from
				// the first frame rather than filling in when the listing
				// arrives.
				res, err := client.GetSandbox(cmd.Context(), apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
				if err != nil {
					return err
				}
				sandbox, err := expectResponse[apimodel.Sandbox](res)
				if err != nil {
					return err
				}
				if err := sandboxcreate.CleanupConfirmedSources(cmd.Context(), a.serverURL, sandbox); err != nil {
					return err
				}
				// A machine with no resolvable identity simply has no
				// discobox of its own to push into; see pushable.
				hostID, _ := hostid.Get()
				box := toTUISandbox(*sandbox, hostID)
				return a.runConsole(cmd, "", tui.WithAttach(box))
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Attaching to the discobox's primary terminal (%s to detach)\n", a.detachHint())
			return a.attachSandboxTerminal(cmd.Context(), projectID, sandboxID, primaryExecID, execAttachOptions{detachAtInputEnd: true}, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().BoolVar(&raw, "raw", false, "Attach this terminal straight to the discobox's primary terminal instead of opening the window on the discobox")
	return cmd
}

// refuseOwnSandbox is the error for an attach, run inside a discobox, that
// names that same discobox: its terminal is the one already in front of the
// person, so there is nothing to attach. It is said before anything asks the
// server, whose answer to a discobox without API access is a refusal that
// advises turning that access on — the wrong advice here (#1631).
//
// Only what names the discobox for certain without the server is recognized:
// its full ID (or the hostname's sbx- spelling of it), bare or in a
// discobox:/// address, or in an address whose server is the one this
// discobox's own address (DISCOBOX_ADDRESS) names. A short ID could as well be
// another discobox's, a name or a pick needs the listing, and an address
// naming another server names a discobox there, so those go on to the server
// as before. Outside a discobox there is no sandbox config, and nothing is
// refused.
func (a *App) refuseOwnSandbox(sandboxArg string) error {
	reference, ok := certainSandboxID(sandboxArg)
	if !ok || !a.isOwnSandbox(reference) {
		return nil
	}
	return fmt.Errorf("you are already in discobox %s: this terminal is in it; to attach to it, run `%s attach %s` on your own machine", reference, commandName(), reference)
}

// isOwnSandbox reports whether id is the discobox this CLI runs in. Outside a
// discobox, or with a sandbox config it cannot read, it is none.
func (a *App) isOwnSandbox(id string) bool {
	self, err := a.ownSandboxID()
	return err == nil && self == id
}

// certainSandboxID is the full discobox ID sandboxArg names without asking
// the server, as refuseOwnSandbox reads it, and whether it names one. A
// malformed address names none here; selectSandbox reports it.
func certainSandboxID(sandboxArg string) (string, bool) {
	reference := strings.TrimSpace(sandboxArg)
	address, isAddress, err := endpoint.ParseSandboxAddress(reference)
	if isAddress {
		if err != nil || (address.Server != "" && address.Server != ownSandboxServer()) {
			return "", false
		}
		reference = address.Sandbox
	}
	reference = idpkg.Canonical(idpkg.PrefixSandbox, reference)
	return reference, allFullSandboxIDs([]string{reference})
}

// ownSandboxServer is the server half of this discobox's own address
// (DISCOBOX_ADDRESS), or empty when it has none.
func ownSandboxServer() string {
	own, isAddress, err := endpoint.ParseSandboxAddress(os.Getenv(endpoint.EnvSandboxAddress))
	if !isAddress || err != nil {
		return ""
	}
	return own.Server
}
