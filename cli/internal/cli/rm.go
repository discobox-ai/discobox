package cli

import (
	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
)

// newRemoveCommand implements `discobox rm`: the everyday counterpart to
// `admin box delete`, which stays the raw form taking IDs. Both post the same
// DELETE, which archives rather than destroys (ADR 0022 §2), and both report
// each argument independently through runActionMany.
//
// What the root command adds is the reference rule every other everyday
// command follows (resolveSandboxReference), at its widest setting: an argument
// is the NAME `discobox ls` prints for the current project directory, or an ID
// from anywhere in the project. listedName rather than configuredName because
// the name a running discobox shows is its terminal's window title, and the
// configured name it otherwise falls back to is generated — `run` has no
// --name — so matching only that would mean matching a string the listing had
// stopped printing. Every argument here is a discobox, so there is no command
// word for a free-form title to be mistaken for.
//
// A title two discoboxes share makes that argument ambiguous, and it is refused
// with both IDs rather than resolved to either: archiving is reversible, but
// archiving the wrong discobox still takes a running agent out from under
// somebody. Only that argument fails; runActionMany runs the rest.
//
// `delete` is an alias rather than the name: rm is what the shell, docker and
// kubectl all spell it, and the API's own verb still reaches the same command
// for anyone who reads it there first. Purging has no root spelling — see
// `admin box purge`; destroying data now is not an everyday verb.
func (a *App) newRemoveCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "rm DISCOBOX...",
		Aliases: []string{"delete"},
		Short:   "Archive discoboxes",
		Long: `Archive discoboxes.

Each argument names one of the discoboxes "discobox ls" shows for the current
project directory: the NAME it is listed under, its ID, or a short prefix of
that ID. A discobox started from another directory, or on another machine, is
named by ID.

A NAME is a discobox's terminal title once its agent has set one, so two
discoboxes can share it. An argument that names more than one is refused, with
the IDs to choose between -- nothing is archived for it. Every argument is
acted on independently either way, so one that fails does not hide the rest.

The container and its runtime resources are removed; the discobox's workspace,
config, and secrets are kept, so "discobox admin box unarchive" brings it back
with its work intact. Archived discoboxes are purged automatically once the
retention runs out: the project's own if it set one, otherwise the server's
default -- 24h, though a development server commonly sets a far shorter one.

To destroy a discobox and its data now, use "discobox admin box purge".`,
		Example: `  discobox rm mybox
  discobox rm sbx_9qk5 sbx_2f7p
  discobox rm 'Fixing the flaky test'
  discobox delete mybox`,
		Args:              cobra.MinimumNArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			// The whole `discobox ls` listing, archived discoboxes included,
			// rather than the runtime candidates the picker offers: archiving
			// is not something a discobox needs a runtime for, and a name the
			// listing still shows must not come back as no such discobox.
			// Archiving an already-archived one is the no-op it reads as.
			sandboxes, err := a.listProjectSandboxes(cmd.Context(), client, projectID, false)
			if err != nil {
				return err
			}
			return runActionMany(cmd, args, "discobox", "archived", func(arg string) (string, error) {
				// listedName: every argument here is a discobox, so the NAME
				// `discobox ls` prints can be typed back. Two discoboxes under
				// one window title make that argument ambiguous, and it is
				// refused rather than guessed at.
				sandboxID, err := a.resolveSandboxReference(cmd.Context(), client, projectID, arg, sandboxes, listedName)
				if err != nil {
					return "", err
				}
				res, err := client.DeleteSandbox(cmd.Context(), apiclientgen.DeleteSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
				if err != nil {
					return "", err
				}
				if err := expectNoContent[apiclientgen.DeleteSandboxAccepted](res); err != nil {
					return "", err
				}
				return sandboxID, nil
			})
		},
	}
}
