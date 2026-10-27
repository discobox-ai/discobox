package cli

import (
	"fmt"
	"runtime"

	api "github.com/discobox-ai/discobox/api/gen"
	"github.com/spf13/cobra"
)

// The candidate ID is mandatory on writes: saving never silently captures a
// different login from the immutable candidate a person reviewed.
func (a *App) newHarnessChangesCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "changes SANDBOX", Short: "Preview portable settings and logins changed in a discobox", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		project, box, client, err := a.sandboxRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		res, err := client.GetSandboxHarnessConfigChanges(cmd.Context(), api.GetSandboxHarnessConfigChangesParams{ProjectId: project, SandboxId: box})
		if err != nil {
			return err
		}
		response, err := expectResponse[api.HarnessConfigChanges](res)
		if err != nil {
			return err
		}
		if a.output == "json" {
			return writeJSON(cmd.OutOrStdout(), response)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s (candidate %s, revision %d)\n", terminalSafe(response.State), terminalSafe(response.CandidateId.Or("none")), response.ConfigRevision)
		for _, f := range response.Files {
			action := "update"
			if f.Remove.Or(false) {
				action = "remove"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", action, terminalSafe(f.Path))
			if content, ok := f.File.Get(); ok {
				fmt.Fprintln(cmd.OutOrStdout(), terminalSafeMultiline(content.Content))
			}
		}
		for _, c := range response.Credentials {
			fmt.Fprintf(cmd.OutOrStdout(), "credential %s %s (%s)\n", terminalSafe(c.Provider), terminalSafe(c.EnvName), terminalSafe(c.Type))
		}
		if len(response.Files)+len(response.Credentials) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No saveable settings or credentials.")
		}
		for _, d := range response.Diagnostics.Or(nil) {
			label := d.Code
			if path := d.Path.Or(""); path != "" {
				label += " (" + path + ")"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", terminalSafe(label), terminalSafe(d.Message))
		}
		if candidate := response.CandidateId.Or(""); candidate != "" && response.State == "pending" {
			prefix := "discobox"
			if a.serverURL != "" {
				prefix += " --server " + shellQuote(terminalSafe(a.serverURL), runtime.GOOS == "windows")
			}
			prefix += " --project " + shellQuote(terminalSafe(project), runtime.GOOS == "windows")
			boxArg := shellQuote(terminalSafe(box), runtime.GOOS == "windows")
			candidateArg := shellQuote(terminalSafe(candidate), runtime.GOOS == "windows")
			if len(response.Files)+len(response.Credentials) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "\nSave shared defaults: %s admin harness save %s %s\n", prefix, boxArg, candidateArg)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Discard candidate: %s admin harness discard %s %s\n", prefix, boxArg, candidateArg)
		}
		return nil
	}}
	return cmd
}
func (a *App) newHarnessSaveCommand() *cobra.Command {
	return &cobra.Command{Use: "save SANDBOX CANDIDATE", Short: "Save a reviewed candidate as the harness defaults and shared credentials", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		project, box, client, err := a.sandboxRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		res, err := client.SaveSandboxHarnessConfigChanges(cmd.Context(), &api.HarnessConfigCandidateRequest{CandidateId: args[1]}, api.SaveSandboxHarnessConfigChangesParams{ProjectId: project, SandboxId: box})
		if err != nil {
			return err
		}
		result, err := expectResponse[api.HarnessConfigSaveResult](res)
		if err != nil {
			return err
		}
		if a.output == "json" {
			return writeJSON(cmd.OutOrStdout(), result)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s (revision %d)\n", terminalSafe(result.State), result.ConfigRevision)
		return nil
	}}
}
func (a *App) newHarnessDiscardCommand() *cobra.Command {
	return &cobra.Command{Use: "discard SANDBOX CANDIDATE", Short: "Discard a pending candidate without changing local files or shared defaults", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		project, box, client, err := a.sandboxRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		res, err := client.DiscardSandboxHarnessConfigChanges(cmd.Context(), &api.HarnessConfigCandidateRequest{CandidateId: args[1]}, api.DiscardSandboxHarnessConfigChangesParams{ProjectId: project, SandboxId: box})
		if err != nil {
			return err
		}
		result, err := expectResponse[api.HarnessConfigChanges](res)
		if err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), result)
	}}
}
func (a *App) newHarnessReconsiderCommand() *cobra.Command {
	var files, credentials []string
	cmd := &cobra.Command{Use: "reconsider SANDBOX CANDIDATE", Short: "Create a new candidate against current defaults, retaining only selected local changes", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		project, box, client, err := a.sandboxRequest(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		res, err := client.ReconsiderSandboxHarnessConfigChanges(cmd.Context(), &api.HarnessConfigSelection{CandidateId: args[1], KeepFiles: files, ReplaceCredentials: credentials}, api.ReconsiderSandboxHarnessConfigChangesParams{ProjectId: project, SandboxId: box})
		if err != nil {
			return err
		}
		result, err := expectResponse[api.HarnessConfigChanges](res)
		if err != nil {
			return err
		}
		return writeJSON(cmd.OutOrStdout(), result)
	}}
	cmd.Flags().StringArrayVar(&files, "keep-file", nil, "Portable file path whose local change should be retained (repeatable)")
	cmd.Flags().StringArrayVar(&credentials, "replace-credential", nil, "Credential environment variable to replace despite the conflict (repeatable)")
	return cmd
}
