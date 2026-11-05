package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/spf13/cobra"
)

func (a *App) newAdoptCommand() *cobra.Command {
	var reason string
	cmd := &cobra.Command{Use: "adopt DISCOBOX", Short: "Ask a person to let this discobox lead another", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeSandboxes,
		Long: `Ask a person to let this discobox lead DISCOBOX, and wait for their answer.

Run this inside a discobox with discobox API access (--discobox-api at creation).
A person approves or denies the request in the console's inbox on the secrets
screen (F4), or from the requester's row (C). --reason is required and is shown
to that person. Ctrl-C stops waiting; the request stays in the inbox.

Approval replaces the child's current parent. The child's credentials and
creator stay its own. The parent may run commands and login shells, read and
write files, and attach to or end existing execs. This may expose existing files
and credential sentinels. A child with wider egress can only be read: writes and
sends into it are refused. The child must be in the same project, cannot be
this discobox or an ancestor, and cannot be archived or deleting.

Use "discobox release" to give a child back and "discobox ls --parent me" to
list the discoboxes this one leads.`,
		Example: "  discobox adopt mybox --reason 'Carry this feature through review'\n  discobox ls --parent me",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, projectID, childID, client, err := a.selectSandbox(cmd, args[0])
			if err != nil {
				return err
			}
			res, err := client.CreateAdoptionRequest(cmd.Context(), &apimodel.CreateAdoptionRequestBody{ChildSandboxId: childID, Justification: reason}, apiclientgen.CreateAdoptionRequestParams{ProjectId: projectID})
			if err != nil {
				return err
			}
			req, err := expectResponse[apimodel.AdoptionRequest](res)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Adoption request %s is waiting for a person.\n", terminalSafe(req.ID))
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				switch req.Status {
				case apiclientgen.AdoptionRequestStatusApproved:
					fmt.Fprintf(cmd.OutOrStdout(), "Now leading %s\n", terminalSafe(childID))
					return nil
				case apiclientgen.AdoptionRequestStatusDenied:
					return fmt.Errorf("adoption request %s was denied", terminalSafe(req.ID))
				}
				select {
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				case <-ticker.C:
				}
				result, err := client.GetAdoptionRequest(cmd.Context(), apiclientgen.GetAdoptionRequestParams{ProjectId: projectID, RequestId: req.ID})
				if err != nil {
					return err
				}
				req, err = expectResponse[apimodel.AdoptionRequest](result)
				if err != nil {
					return err
				}
			}
		}}
	cmd.Flags().StringVar(&reason, "reason", "", "Why this discobox should lead the child")
	_ = cmd.MarkFlagRequired("reason")
	return cmd
}

func (a *App) newReleaseCommand() *cobra.Command {
	return &cobra.Command{Use: "release DISCOBOX", Short: "Give a child discobox back", Args: cobra.ExactArgs(1), ValidArgsFunction: a.completeSandboxes,
		Long: `Give DISCOBOX back by clearing its parent. Its creator, files and credentials
stay its own, and it keeps running. A person may release any discobox; a
discobox with API access may release only a child it leads.

The former parent loses the authority adoption or creation gave it over this
child. Adopting it again needs a person's approval.`,
		Example: "  discobox release mybox\n  discobox ls --parent me",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, projectID, childID, client, err := a.selectSandbox(cmd, args[0])
			if err != nil {
				return err
			}
			res, err := client.ReleaseSandbox(cmd.Context(), apiclientgen.ReleaseSandboxParams{ProjectId: projectID, SandboxId: childID})
			if err != nil {
				return err
			}
			if err := expectNoContent[apiclientgen.ReleaseSandboxNoContent](res); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Released %s\n", terminalSafe(childID))
			return nil
		}}
}

func ownSandboxID() (string, error) {
	data, err := os.ReadFile("/etc/discobox/sandbox.json")
	if err != nil {
		return "", fmt.Errorf("--parent me needs this discobox's sandbox config: %w", err)
	}
	var cfg sandboxconfig.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", err
	}
	if cfg.SandboxID == "" {
		return "", fmt.Errorf("this discobox's sandbox config has no ID")
	}
	return cfg.SandboxID, nil
}
