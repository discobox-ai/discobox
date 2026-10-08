package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/sandboxmeta"
)

// newTagCommand is `discobox tag`: the everyday form of update-sandbox-meta,
// which carries a change to a discobox's tags or description into its
// ~/.discobox/meta.json (ADR 0136). It is a root command, beside `ls --tag`
// that filters on what it sets, and has no `admin box` twin: `admin box get`
// already prints the raw record, meta included.
//
// Its argument is resolved the way `start` and `stop` resolve theirs, and for
// the same reason a full ID skips the listing: a discobox tagging the ones it
// created (ADR 26-10-08-447) has every call judged against its use, and
// "tag a discobox I created" says nothing of listing them.
func (a *App) newTagCommand() *cobra.Command {
	var removed []string
	var description string
	cmd := &cobra.Command{
		Use:   "tag DISCOBOX [KEY[=VALUE]...] [--rm KEY]... [--description TEXT]",
		Short: "Set and remove a discobox's tags",
		Long: `Set and remove a discobox's tags, and change its description, from outside it.

Each KEY=VALUE sets a tag, and a bare KEY sets a plain label with no value;
--rm removes one, and naming a tag the discobox does not have is not an error.
A key is printable, with no whitespace, "=" or ","; a value is one line with
no ",". Tags not named are left as they are. --description replaces the
description, and --description "" clears it.

A discobox's tags are its own, in ~/.discobox/meta.json inside it: the change
is written there, and a stopped discobox is started to take it. "discobox ls
--tag KEY" lists by them. The tags the discobox holds afterwards are printed,
one per line.

DISCOBOX is the NAME "discobox ls" shows for the current project directory, its
ID, or a short prefix of that ID.`,
		Example: `  discobox tag mybox to-delete
  discobox tag sbx_9qk5 ticket=ENG-12 wip
  discobox tag mybox --rm wip --rm to-delete
  discobox tag mybox --description "Fixing the flaky test"`,
		Args: cobra.MinimumNArgs(1),
		ValidArgsFunction: func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return a.completeSandboxes(cmd, args, toComplete)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var describe *string
			if cmd.Flags().Changed("description") {
				describe = &description
			}
			body, err := tagChange(args[1:], removed, describe)
			if err != nil {
				return err
			}
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			var sandboxes []apimodel.Sandbox
			if !allFullSandboxIDs(args[:1]) {
				sandboxes, err = a.listProjectSandboxes(cmd.Context(), client, projectID, false, nil)
				if err != nil {
					return err
				}
			}
			sandboxID, err := a.resolveSandboxReference(cmd.Context(), client, projectID, args[0], sandboxes, listedName)
			if err != nil {
				return err
			}
			res, err := client.UpdateSandboxMeta(cmd.Context(), body, apiclientgen.UpdateSandboxMetaParams{ProjectId: projectID, SandboxId: sandboxID})
			if err != nil {
				return err
			}
			sandbox, err := expectResponse[apimodel.Sandbox](res)
			if err != nil {
				return err
			}
			return a.writeSandboxMeta(cmd, sandbox)
		},
	}
	cmd.Flags().StringArrayVar(&removed, "rm", nil, "Remove this tag, by KEY; repeat to remove several")
	cmd.Flags().StringVar(&description, "description", "", `Replace the description; "" clears it`)
	return cmd
}

// tagChange is the request for what was asked: every KEY[=VALUE] set, every
// --rm removed, and the description replaced when describe is set. It is
// checked whole by sandboxmeta before anything is sent, the listing included,
// so a bad argument is refused by the name it was typed as. The server checks
// the change again before it reaches the discobox.
func tagChange(set, removed []string, describe *string) (*apimodel.UpdateSandboxMetaBody, error) {
	if len(set) == 0 && len(removed) == 0 && describe == nil {
		return nil, errors.New("nothing to change: give a KEY[=VALUE] to set, --rm KEY, or --description")
	}
	change := sandboxmeta.Change{Description: describe, RemoveTags: removed}
	if len(set) > 0 {
		change.SetTags = make(map[string]string, len(set))
	}
	for _, text := range set {
		key, value, err := sandboxmeta.ParseTag(text)
		if err != nil {
			return nil, err
		}
		if _, ok := change.SetTags[key]; ok {
			return nil, fmt.Errorf("tag %q is set more than once", key)
		}
		change.SetTags[key] = value
	}
	for _, key := range removed {
		if err := sandboxmeta.ValidateKey(key); err != nil {
			return nil, fmt.Errorf("--rm %q: %w", key, err)
		}
	}
	if err := change.Check(); err != nil {
		return nil, err
	}
	body := &apimodel.UpdateSandboxMetaBody{RemoveTags: change.RemoveTags}
	if describe != nil {
		body.Description = apiclientgen.NewOptString(*describe)
	}
	if change.SetTags != nil {
		body.SetTags = apiclientgen.NewOptUpdateSandboxMetaBodySetTags(change.SetTags)
	}
	return body, nil
}

// writeSandboxMeta prints the tags the discobox holds after a change, one per
// line as TagStrings spells them, or with -o json its ID, description, and
// tags. Both were written inside the discobox, so both are escaped for the
// terminal.
func (a *App) writeSandboxMeta(cmd *cobra.Command, sandbox *apimodel.Sandbox) error {
	tags := sandboxTags(*sandbox)
	if a.output == "json" {
		if tags == nil {
			tags = map[string]string{}
		}
		var description string
		if meta, ok := sandbox.Meta.Get(); ok {
			description = meta.Description.Or("")
		}
		return writeTerminalSafeJSON(cmd.OutOrStdout(), map[string]any{
			"id":          sandbox.ID,
			"description": description,
			"tags":        tags,
		})
	}
	for _, tag := range sandboxmeta.TagStrings(tags) {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), terminalSafe(tag)); err != nil {
			return err
		}
	}
	return nil
}
