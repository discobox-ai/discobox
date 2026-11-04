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
