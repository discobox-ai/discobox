package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/refreshcmd"
	"github.com/discobox-ai/discobox/internal/hostid"
)

// newSecretRefreshCommand renews a token from a shell: the answer to a refresh
// request for somebody not in the window, or a renewal nobody asked for yet
// (ADR 26-09-25-122 §5).
func (a *App) newSecretRefreshCommand() *cobra.Command {
	var value string
	var run bool
	cmd := &cobra.Command{
		Use:   "refresh SECRET_ID (--run | --value VALUE|-)",
		Short: "Write a new value for a token, answering its refresh request",
		Long: `Write a new value for a token, answering its open refresh request.

--run runs the token's refresh command here, on this machine, as you, and
sends what it prints. It is only run when asked for by name: the command is the
project's, and anybody who can edit the secret can change it, so without --run
this says what it would run and stops. --value gives the value instead;
--value - reads it from stdin.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := a.apiClient()
			if err != nil {
				return err
			}
			projectID, err := a.projectIDValue()
			if err != nil {
				return err
			}
			secretID, err := a.resolveSecretID(cmd.Context(), client, projectID, args[0])
			if err != nil {
				return err
			}
			res, err := client.GetSecret(cmd.Context(), apiclientgen.GetSecretParams{ProjectId: projectID, SecretId: secretID})
			if err != nil {
				return err
			}
			secret, err := expectResponse[apimodel.Secret](res)
			if err != nil {
				return err
			}
			command, _ := secret.RefreshCommand.Get()
			switch {
			case run && cmd.Flags().Changed("value"):
				return fmt.Errorf("--run and --value are two answers; give one")
			case !run && !cmd.Flags().Changed("value") && len(command) > 0:
				return fmt.Errorf("secret %s is renewed with `%s`; pass --run to run it here, or give the value with --value", secretID, refreshcmd.Join(command))
			case !run && !cmd.Flags().Changed("value"):
				return fmt.Errorf("secret %s has no refresh command; give the value with --value", secretID)
			}
			body := &apimodel.RefreshSecretBody{}
			if cmd.Flags().Changed("value") {
				entered, err := enteredRefreshValue(value, cmd.InOrStdin())
				if err != nil {
					return err
				}
				body.Value = entered
				body.Via = apiclientgen.RefreshSecretBodyViaEntered
			} else {
				if len(command) == 0 {
					return fmt.Errorf("secret %s has no refresh command; give the value with --value", secretID)
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "running %s\n", refreshcmd.Join(command))
				produced, err := refreshcmd.Run(cmd.Context(), command)
				if err != nil {
					return err
				}
				body.Value = produced
				body.Via = apiclientgen.RefreshSecretBodyViaCommand
				body.SetCommand(apiclientgen.NewOptNilStringArray(command))
			}
			if id, err := hostid.Get(); err == nil {
				body.SetClientHost(apiclientgen.NewOptString(id))
			}
			requestID, err := openRefreshRequest(cmd.Context(), client, projectID, secretID)
			if err != nil {
				return err
			}
			if requestID != "" {
				body.SetRequestId(apiclientgen.NewOptString(requestID))
			}
			refreshed, err := client.RefreshSecret(cmd.Context(), body, apiclientgen.RefreshSecretParams{ProjectId: projectID, SecretId: secretID})
			if err != nil {
				return err
			}
			updated, err := expectResponse[apimodel.Secret](refreshed)
			if err != nil {
				return err
			}
			return a.writeSecret(cmd, updated)
		},
	}
	cmd.Flags().BoolVar(&run, "run", false, "Run the token's refresh command here and send what it prints")
	cmd.Flags().StringVar(&value, "value", "", "The new value, or - to read it from stdin")
	return cmd
}

// enteredRefreshValue is the value --value names: itself, or stdin's first
// line for "-".
func enteredRefreshValue(flag string, stdin io.Reader) (string, error) {
	if flag != "-" {
		if strings.TrimSpace(flag) == "" {
			return "", fmt.Errorf("--value is empty")
		}
		return strings.TrimSpace(flag), nil
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, refreshcmd.MaxOutput+1))
	if err != nil {
		return "", err
	}
	if len(raw) > refreshcmd.MaxOutput {
		return "", fmt.Errorf("stdin is more than %d bytes; a credential is one line", refreshcmd.MaxOutput)
	}
	entered := strings.TrimSpace(string(raw))
	if entered == "" {
		return "", fmt.Errorf("stdin was empty")
	}
	return entered, nil
}

// openRefreshRequest is the open refresh request on a secret, or "" when
// nothing is asking for a new value.
func openRefreshRequest(ctx context.Context, client *apiclientgen.Client, projectID, secretID string) (string, error) {
	res, err := client.ListSecretRequests(ctx, apiclientgen.ListSecretRequestsParams{
		ProjectId: projectID,
		Status:    apiclientgen.NewOptListSecretRequestsStatus(apiclientgen.ListSecretRequestsStatusPending),
	})
	if err != nil {
		return "", err
	}
	body, err := expectResponse[apimodel.ListSecretRequestsBody](res)
	if err != nil {
		return "", err
	}
	for _, request := range body.GetSecretRequests() {
		if request.Reason.Or("") == apiclientgen.SecretRequestReasonRefresh && request.SecretId.Or("") == secretID {
			return request.ID, nil
		}
	}
	return "", nil
}
