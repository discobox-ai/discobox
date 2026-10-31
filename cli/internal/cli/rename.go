package cli

import (
	"fmt"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/sandboxmeta"
	"github.com/spf13/cobra"
)

// newRenameCommand is `discobox rename`: give a discobox a name that stays
// where it was put (ADR 26-10-12-026). The name is the discobox's own, in its
// meta file beside its description and tags, so this is the write `discobox
// tag` makes with a different field in it — not an edit of the configured
// name, which is the generated handle `discobox admin box update --name`
// changes and a listing stops showing once anything else names the discobox.
//
// It is a command of its own rather than a flag on `tag` because it is what a
// person looks for: the name is the first thing a listing shows, and nobody
// trying to change it reaches for a command about tags.
func (a *App) newRenameCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rename DISCOBOX [NAME]",
		Short: "Name a discobox",
		Long: `Give a discobox the name "discobox ls" and the console show for it, in place of
the title its agent set.

NAME is one line, of any words; quote it if it has spaces. It need not be
unique, but a command given a name two discoboxes share refuses it as
ambiguous. With no NAME the name is cleared, and the discobox is listed by its
agent's title again.

A discobox's name is its own, in ~/.discobox/meta.json inside it: the change is
written there, and a stopped discobox is started to take it. The agent in the
discobox may set it too, by writing "name" into that file. The name the
discobox is then listed as is printed.

DISCOBOX is the NAME "discobox ls" shows, its ID, or a short prefix of that ID.`,
		Example: `  discobox rename sbx_9qk5 "Daily GH check-in"
  discobox rename "Commit files" "Daily GH check-in"
  discobox rename mybox`,
		Args: cobra.RangeArgs(1, 2),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return a.completeSandboxes(cmd, args, toComplete)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if len(args) == 2 {
				name = sandboxmeta.NormalizeName(args[1])
				if name == "" {
					return fmt.Errorf("the name is empty: leave NAME out to clear the name")
				}
			}
			// Checked before anything is sent, the listing included, so a name
			// that cannot be taken is refused by what was typed. The server
			// checks it again before it reaches the discobox.
			if err := (sandboxmeta.Change{Name: &name}).Check(); err != nil {
				return err
			}
			body := &apimodel.UpdateSandboxMetaBody{Name: apiclientgen.NewOptString(name)}
			sandbox, err := a.changeSandboxMeta(cmd, args[0], body)
			if err != nil {
				return err
			}
			return a.writeSandboxName(cmd, sandbox)
		},
	}
	return cmd
}

// writeSandboxName prints what the discobox is listed as after a rename, or
// with -o json its ID, the name its meta now holds, and that listed name. The
// two differ while the program in the primary terminal reports a msg, which
// follows the name ("name: msg", ADR 26-10-31-618), and with the name cleared,
// when the listing falls back to that msg or the agent's title. Both were
// written inside the discobox, so both are escaped for the terminal.
func (a *App) writeSandboxName(cmd *cobra.Command, sandbox *apimodel.Sandbox) error {
	if a.output == "json" {
		var name string
		if meta, ok := sandbox.Meta.Get(); ok {
			name = meta.Name.Or("")
		}
		return writeTerminalSafeJSON(cmd.OutOrStdout(), map[string]any{
			"id":          sandbox.ID,
			"name":        name,
			"displayName": sandbox.DisplayName,
		})
	}
	_, err := fmt.Fprintln(cmd.OutOrStdout(), terminalSafe(sandbox.DisplayName))
	return err
}
