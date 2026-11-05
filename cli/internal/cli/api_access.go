package cli

import (
	"fmt"
	"time"

	api "github.com/discobox-ai/discobox/api/gen"
	model "github.com/discobox-ai/discobox/api/model"
	"github.com/spf13/cobra"
)

func (a *App) newAPIAccessCommand() *cobra.Command {
	root := &cobra.Command{Use: "api-access", Short: "Request and answer discobox API access"}
	var reason string
	request := &cobra.Command{Use: "request", Short: "Ask a person to enable this box's API access and wait", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		project, err := a.projectIDValue()
		if err != nil {
			return err
		}
		res, err := client.CreateAPIAccessRequest(cmd.Context(), &model.CreateAPIAccessRequestBody{Justification: reason}, api.CreateAPIAccessRequestParams{ProjectId: project})
		if err != nil {
			return err
		}
		req, err := expectResponse[model.APIAccessRequest](res)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "API access request %s is waiting for a person. Ctrl-C leaves it waiting.\n", terminalSafe(req.ID))
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			switch req.Status {
			case api.APIAccessRequestStatusApproved:
				fmt.Fprintln(cmd.OutOrStdout(), "API access enabled. A person can turn it off in Settings.")
				return nil
			case api.APIAccessRequestStatusDenied:
				return fmt.Errorf("API access request %s was denied", terminalSafe(req.ID))
			}
			select {
			case <-cmd.Context().Done():
				return cmd.Context().Err()
			case <-ticker.C:
			}
			res, err := client.GetAPIAccessRequest(cmd.Context(), api.GetAPIAccessRequestParams{ProjectId: project, RequestId: req.ID})
			if err != nil {
				return err
			}
			req, err = expectResponse[model.APIAccessRequest](res)
			if err != nil {
				return err
			}
		}
	}}
	request.Flags().StringVar(&reason, "reason", "", "Why this box needs API access (shown to the person)")
	_ = request.MarkFlagRequired("reason")
	root.AddCommand(request)
	for _, verb := range []string{"ls", "get", "approve", "deny"} {
		cmd := &cobra.Command{Use: verb + " REQUEST_ID", Short: map[string]string{"ls": "List API access requests and consent history", "get": "Show an API access request", "approve": "Enable the requesting box's API access", "deny": "Deny an API access request"}[verb], Args: cobra.ExactArgs(1)}
		if verb == "ls" {
			cmd.Use = "ls"
			cmd.Args = cobra.NoArgs
			cmd.Aliases = []string{"list"}
			a.addQuietFlag(cmd)
		}
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			project, err := a.projectIDValue()
			if err != nil {
				return err
			}
			if verb == "ls" {
				res, err := client.ListAPIAccessRequests(cmd.Context(), api.ListAPIAccessRequestsParams{ProjectId: project})
				if err != nil {
					return err
				}
				body, err := expectResponse[model.ListAPIAccessRequestsBody](res)
				if err != nil {
					return err
				}
				if a.output == "json" {
					return writeTerminalSafeJSON(cmd.OutOrStdout(), body)
				}
				for _, r := range body.ApiAccessRequests {
					if a.quiet {
						fmt.Fprintln(cmd.OutOrStdout(), terminalSafe(r.ID))
						continue
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", terminalSafe(r.ID), r.Status, terminalSafe(r.RequesterSandboxId))
				}
				return nil
			}
			switch verb {
			case "get":
				res, err := client.GetAPIAccessRequest(cmd.Context(), api.GetAPIAccessRequestParams{ProjectId: project, RequestId: args[0]})
				if err != nil {
					return err
				}
				r, err := expectResponse[model.APIAccessRequest](res)
				if err != nil {
					return err
				}
				if a.output == "json" {
					return writeTerminalSafeJSON(cmd.OutOrStdout(), r)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Request: %s\nBox: %s\nReason (box's words): %s\nStatus: %s\nDecided by: %s\n\nApproval enables API access until a person turns it off in Settings.\nIt permits project discovery and creation using project harness configs and their credentials,\nand bounded management of direct children. Wider-egress children are read-only.\nExisting credential and egress restrictions still apply.\n", terminalSafe(r.ID), terminalSafe(r.RequesterSandboxId), terminalSafe(r.Justification), r.Status, terminalSafe(r.DecidedBy.Or("")))
				return nil
			case "approve":
				res, err := client.ApproveAPIAccessRequest(cmd.Context(), api.ApproveAPIAccessRequestParams{ProjectId: project, RequestId: args[0]})
				if err != nil {
					return err
				}
				_, err = expectResponse[model.APIAccessRequest](res)
				return err
			default:
				res, err := client.DenyAPIAccessRequest(cmd.Context(), api.DenyAPIAccessRequestParams{ProjectId: project, RequestId: args[0]})
				if err != nil {
					return err
				}
				return expectNoContent[api.DenyAPIAccessRequestNoContent](res)
			}
		}
		root.AddCommand(cmd)
	}
	return root
}
