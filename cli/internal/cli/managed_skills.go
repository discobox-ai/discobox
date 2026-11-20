package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"text/tabwriter"

	"github.com/spf13/cobra"

	api "github.com/discobox-ai/discobox/api/gen"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
)

func (a *App) newManagedSkillsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use: "skills", Short: "Manage a discobox's skills", Args: cobra.NoArgs,
	}
	list := &cobra.Command{
		Use: "ls BOX", Aliases: []string{"list"}, Short: "List the discobox's skill versions", Args: cobra.ExactArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, projectID, boxID, client, err := a.selectSandbox(cmd, args[0])
			if err != nil {
				return err
			}
			res, err := client.GetSandboxSkills(cmd.Context(), api.GetSandboxSkillsParams{ProjectId: projectID, SandboxId: boxID})
			if err != nil {
				return err
			}
			inventory, err := expectResponse[api.SandboxSkillsInventory](res)
			if err != nil {
				return err
			}
			if a.output == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(inventory)
			}
			installed, _ := inventory.Installed.Get()
			desired, _ := inventory.Desired.Get()
			names := make([]string, 0, len(installed)+len(desired))
			for name := range installed {
				names = append(names, name)
			}
			for name := range desired {
				if _, ok := installed[name]; !ok {
					names = append(names, name)
				}
			}
			slices.Sort(names)
			if a.quiet {
				for _, name := range names {
					fmt.Fprintln(cmd.OutOrStdout(), name)
				}
				return nil
			}
			out := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintf(out, "Desired revision %d, applied revision %d (%s)\n", inventory.DesiredRevision, inventory.AppliedRevision, inventory.Application.Or("pending"))
			fmt.Fprintln(out, "SKILL	SOURCE	VERSION	STATE")
			for _, name := range names {
				current, present := installed[name]
				if present {
					printSkillVersion(out, name, current, "installed")
				}
				next, wanted := desired[name]
				if wanted && (!present || current != next) {
					printSkillVersion(out, name, next, "desired")
				}
			}
			return out.Flush()
		},
	}
	a.addQuietFlag(list)
	cmd.AddCommand(list)
	for _, operation := range []api.SandboxSkillMutationOperation{"set", "add", "rm", "follow", "sync"} {
		cmd.AddCommand(a.newManagedSkillMutationCommand(operation))
	}
	return cmd
}

func (a *App) newManagedSkillMutationCommand(operation api.SandboxSkillMutationOperation) *cobra.Command {
	var directory, source string
	cmd := &cobra.Command{
		Use: string(operation) + " BOX", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			if operation == "rm" {
				name = args[1]
			}
			return a.publishManagedSkills(cmd, args[0], directory, source, name, operation)
		},
	}
	if operation == "rm" {
		cmd.Use += " NAME"
		cmd.Short = "Remove a managed skill"
		cmd.Aliases = []string{"delete", "remove"}
		cmd.Args = cobra.ExactArgs(2)
	} else {
		cmd.Short = "Publish skills from a directory or Git repository"
		cmd.Flags().StringVar(&directory, "skills", "", "Read skills from DIR or URL[@REF][//PATH]")
		cmd.Flags().StringVar(&source, "source", "local", "Stable identifier for this skills source")
		if err := cmd.MarkFlagRequired("skills"); err != nil {
			panic(err)
		}
	}
	return cmd
}

func (a *App) publishManagedSkills(cmd *cobra.Command, box, directory, source, name string, operation api.SandboxSkillMutationOperation) error {
	_, projectID, boxID, client, err := a.selectSandbox(cmd, box)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	mutation := api.SandboxSkillMutation{Operation: operation}
	if operation == "rm" {
		mutation.Name.SetTo(name)
	} else {
		mutation.Origin.SetTo(api.SandboxSkillOrigin{SourceId: source})
		if operation == "follow" || operation == "sync" {
			res, err := client.GetSandboxSkills(ctx, api.GetSandboxSkillsParams{ProjectId: projectID, SandboxId: boxID})
			if err != nil {
				return err
			}
			current, err := expectResponse[api.SandboxSkillsInventory](res)
			if err != nil {
				return err
			}
			mutation.ExpectedRevision.SetTo(current.DesiredRevision)
		}
		skills, commit, err := sandboxcreate.ReadManagedSkills(ctx, directory, func(message string) {
			fmt.Fprintln(cmd.ErrOrStderr(), message)
		})
		if err != nil {
			return err
		}
		if commit != "" {
			origin := mutation.Origin.Value
			origin.Commit.SetTo(commit)
			mutation.Origin.SetTo(origin)
		}
		config := api.SandboxCreateConfig{}
		sandboxcreate.SetCreateSandboxSkills(&config, skills)
		if uploaded, ok := config.Skills.Get(); ok {
			mutation.Skills.SetTo(api.SandboxSkillMutationSkills(uploaded))
		}
	}
	res, err := client.MutateSandboxSkills(ctx, &mutation, api.MutateSandboxSkillsParams{ProjectId: projectID, SandboxId: boxID})
	if err != nil {
		return err
	}
	inventory, err := expectResponse[api.SandboxSkillsInventory](res)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "Desired revision %d, applied revision %d\n", inventory.DesiredRevision, inventory.AppliedRevision)
	return err
}

func printSkillVersion(out io.Writer, name string, version api.SandboxSkillVersion, state string) {
	digest := version.Commit.Or(version.Digest)
	if digest == "" {
		digest = "unknown"
	}
	fmt.Fprintf(out, "%s	%s	%s	%s\n", name, version.SourceId, digest, state)
}
