package cli

import (
	"context"
	"fmt"
	"slices"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/sandboxmeta"
)

// sandboxFilter is what a listing of discoboxes is narrowed to, each part open
// when empty.
type sandboxFilter struct {
	// server is the one server asked, by the name the SERVER column gives it.
	server string
	// source is the SOURCE column's text, matched exactly: the directory or
	// repository URL a discobox was cut from, or "-" for one with none. It is
	// `ls --source`, and what the column prints is the whole of its grammar.
	source string
	// tags are selectors a discobox's recorded tags must all match (ADR 0136).
	tags []string
}

// newListCommand lists every discobox in the project, on every server, from
// whichever directory and machine it was created — what the console opens on
// (ADR 26-10-10-954). --on, --source and --tag narrow it by what its own
// SERVER, SOURCE and TAGS columns print.
//
// It differs from `admin box ls` in spanning the registered servers and in
// those filters; that one is the primary's raw listing.
func (a *App) newListCommand() *cobra.Command {
	var filter sandboxFilter
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"ps"},
		Short:   "List discoboxes",
		Long: `List every discobox in the project, whichever directory and machine it was
created from, with what each was cut from in the SOURCE column.

Three filters narrow the list, and a discobox is listed when it passes every
one given:

--source lists the discoboxes whose SOURCE column is exactly this: the
directory or repository URL as the column prints it, or "-" for the discoboxes
with no source.

--on lists one server's discoboxes, by the name the SERVER column gives it.

--tag lists the discoboxes carrying a tag: KEY for the tag with any value,
KEY=VALUE for exactly that value. Repeat it to require several. A discobox's
tags are its own, in ~/.discobox/meta.json inside it; this filters on the copy
it last reported, so a stopped discobox is listed by the tags it had.

Every server is listed: the primary, and the ones "discobox admin remote"
registered, with a SERVER column once there is more than one. A registered server that does
not answer is left out, and says so on stderr.`,
		Example: `  discobox ls
  discobox ls --source ~/src/api
  discobox ls --source https://github.com/acme/api.git
  discobox ls --on beta --tag wip --tag ticket=ENG-12
  discobox ls -o json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Checked here, so a selector the server would refuse is said once
			// rather than once per server.
			if _, err := sandboxmeta.ParseSelectors(filter.tags); err != nil {
				return err
			}
			// An empty source would read as no filter at all, and list
			// everything for somebody who asked for one place.
			if cmd.Flags().Changed("source") && filter.source == "" {
				return fmt.Errorf("--source needs a value from the SOURCE column, or - for the discoboxes with no source")
			}
			listed, unreachable, err := a.listEveryServer(cmd.Context(), filter)
			if err != nil {
				return err
			}
			note := printedNotes(cmd.ErrOrStderr())
			for _, silent := range unreachable {
				note("%s did not answer, so its discoboxes are not listed: %v", silent.server.name, silent.err)
			}
			set, err := a.servers()
			if err != nil {
				return err
			}
			sandboxes := make([]apimodel.Sandbox, 0, len(listed))
			var serverOf map[string]string
			if len(set) > 1 {
				serverOf = make(map[string]string, len(listed))
			}
			for _, row := range listed {
				sandboxes = append(sandboxes, row.sandbox)
				if serverOf != nil {
					serverOf[row.sandbox.ID] = row.server.name
				}
			}
			return a.writeSandboxes(cmd, sandboxes, serverOf)
		},
	}
	cmd.Flags().StringVar(&filter.source, "source", "", "Only list the discoboxes whose SOURCE column is exactly this; - for the ones with no source")
	cmd.Flags().StringVar(&filter.server, "on", "", "Only list the discoboxes on this server, by its name in the SERVER column")
	cmd.Flags().StringArrayVar(&filter.tags, "tag", nil, "Only list discoboxes with this tag: KEY, or KEY=VALUE for that value; repeat to require several")
	a.addQuietFlag(cmd)
	return cmd
}

// listProjectSandboxes lists one server's sandboxes in the project, narrowed
// to filter's source and tags; which server is the caller's to choose. It is
// the listing behind `discobox ls`, shared with the commands that take a
// discobox by name or offer a pick: they list the same discoboxes it does.
func (a *App) listProjectSandboxes(ctx context.Context, client *apiclientgen.Client, projectID string, filter sandboxFilter) ([]apimodel.Sandbox, error) {
	selectors, err := sandboxmeta.ParseSelectors(filter.tags)
	if err != nil {
		return nil, err
	}
	params := apiclientgen.ListSandboxesParams{ProjectId: projectID, Tag: filter.tags}
	bodyRes, err := client.ListSandboxes(ctx, params)
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListSandboxesBody](bodyRes)
	if err != nil {
		return nil, err
	}
	sandboxes := body.GetSandboxes()
	if filter.source != "" {
		// Matched here rather than by the server: the filter is the column's
		// own text, and the column is drawn here (sandboxSource).
		sandboxes = slices.DeleteFunc(sandboxes, func(sandbox apimodel.Sandbox) bool {
			return sandboxSource(sandbox) != filter.source
		})
	}
	if len(selectors) == 0 {
		return sandboxes, nil
	}
	// The server filters, and the answer is filtered again here: a server
	// older than tags ignores the parameter it does not know and lists every
	// discobox, and `discobox ls` asks every registered server. Its discoboxes
	// carry no tags, so matching here lists none of them, which is the true
	// answer rather than an unfiltered one.
	matched := sandboxes[:0]
	for _, sandbox := range sandboxes {
		if sandboxmeta.MatchesAll(sandboxTags(sandbox), selectors) {
			matched = append(matched, sandbox)
		}
	}
	return matched, nil
}
