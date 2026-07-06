package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
)

type secretValueOptions struct {
	valueJSON  string
	username   string
	password   string
	privateKey string
	passphrase string
	token      string
}

func (a *App) newSecretCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "secret", Aliases: []string{"secrets"}, Short: "Manage project secrets"}
	cmd.AddCommand(a.newSecretListCommand())
	cmd.AddCommand(a.newSecretGetCommand())
	cmd.AddCommand(a.newSecretCreateCommand())
	cmd.AddCommand(a.newSecretUpdateCommand())
	cmd.AddCommand(a.newSecretDeleteCommand())
	cmd.AddCommand(a.newSecretRequestCommand())
	return cmd
}

func (a *App) newSecretListCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "list", Short: "List secrets", RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		res, err := client.ListSecrets(cmd.Context(), apiclientgen.ListSecretsParams{ProjectId: projectID})
		if err != nil {
			return err
		}
		body, err := expectResponse[apimodel.ListSecretsBody](res)
		if err != nil {
			return err
		}
		return a.writeSecrets(cmd, body.GetSecrets())
	}}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newSecretGetCommand() *cobra.Command {
	return &cobra.Command{Use: "get SECRET_ID", Short: "Get a secret", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		return a.writeSecret(cmd, secret)
	}}
}

func (a *App) newSecretCreateCommand() *cobra.Command {
	var name, secretType, host string
	var autoApprove bool
	var ttl int64
	var value secretValueOptions
	cmd := &cobra.Command{Use: "create --name NAME --type TYPE", Short: "Create a secret", RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		body, err := createSecretBody(cmd.Flags(), name, secretType, host, autoApprove, ttl, value)
		if err != nil {
			return err
		}
		res, err := client.CreateSecret(cmd.Context(), body, apiclientgen.CreateSecretParams{ProjectId: projectID})
		if err != nil {
			return err
		}
		secret, err := expectResponse[apimodel.Secret](res)
		if err != nil {
			return err
		}
		return a.writeSecret(cmd, secret)
	}}
	cmd.Flags().StringVar(&name, "name", "", "Secret name")
	cmd.Flags().StringVar(&secretType, "type", "", "Secret type: git, ssh, or bearer")
	cmd.Flags().StringVar(&host, "host", "", "Optional host hint, such as github.com")
	cmd.Flags().BoolVar(&autoApprove, "auto-approve", false, "Automatically approve matching requests")
	cmd.Flags().Int64Var(&ttl, "grant-ttl", 0, "Default grant duration in seconds")
	addSecretValueFlags(cmd.Flags(), &value)
	return cmd
}

func (a *App) newSecretUpdateCommand() *cobra.Command {
	var name, host string
	var autoApprove bool
	var ttl int64
	var value secretValueOptions
	cmd := &cobra.Command{Use: "update SECRET_ID", Short: "Update a secret", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		body, err := updateSecretBody(cmd.Flags(), name, host, autoApprove, ttl, value)
		if err != nil {
			return err
		}
		res, err := client.UpdateSecret(cmd.Context(), body, apiclientgen.UpdateSecretParams{ProjectId: projectID, SecretId: secretID})
		if err != nil {
			return err
		}
		secret, err := expectResponse[apimodel.Secret](res)
		if err != nil {
			return err
		}
		return a.writeSecret(cmd, secret)
	}}
	cmd.Flags().StringVar(&name, "name", "", "Secret name")
	cmd.Flags().StringVar(&host, "host", "", "Optional host hint, such as github.com")
	cmd.Flags().BoolVar(&autoApprove, "auto-approve", false, "Automatically approve matching requests")
	cmd.Flags().Lookup("auto-approve").NoOptDefVal = "true"
	cmd.Flags().Int64Var(&ttl, "grant-ttl", 0, "Default grant duration in seconds")
	addSecretValueFlags(cmd.Flags(), &value)
	return cmd
}

func (a *App) newSecretDeleteCommand() *cobra.Command {
	return &cobra.Command{Use: "delete SECRET_ID...", Short: "Delete secrets", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		return runDeleteMany(cmd, args, "secret", func(arg string) (string, error) {
			secretID, err := a.resolveSecretID(cmd.Context(), client, projectID, arg)
			if err != nil {
				return "", err
			}
			res, err := client.DeleteSecret(cmd.Context(), apiclientgen.DeleteSecretParams{ProjectId: projectID, SecretId: secretID})
			if err != nil {
				return "", err
			}
			if err := expectNoContent[apiclientgen.DeleteSecretNoContent](res); err != nil {
				return "", err
			}
			return secretID, nil
		})
	}}
}

func (a *App) newSecretRequestCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "request", Aliases: []string{"requests"}, Short: "Manage secret access requests"}
	cmd.AddCommand(a.newSecretRequestListCommand())
	cmd.AddCommand(a.newSecretRequestGetCommand())
	cmd.AddCommand(a.newSecretRequestCreateCommand())
	cmd.AddCommand(a.newSecretRequestApproveCommand())
	cmd.AddCommand(a.newSecretRequestDenyCommand())
	return cmd
}

func (a *App) newSecretRequestListCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "list", Short: "List secret requests", RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		res, err := client.ListSecretRequests(cmd.Context(), apiclientgen.ListSecretRequestsParams{ProjectId: projectID})
		if err != nil {
			return err
		}
		body, err := expectResponse[apimodel.ListSecretRequestsBody](res)
		if err != nil {
			return err
		}
		return a.writeSecretRequests(cmd, body.GetSecretRequests())
	}}
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newSecretRequestGetCommand() *cobra.Command {
	return &cobra.Command{Use: "get REQUEST_ID", Short: "Get a secret request", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		requestID, err := a.resolveSecretRequestID(cmd.Context(), client, projectID, args[0])
		if err != nil {
			return err
		}
		res, err := client.GetSecretRequest(cmd.Context(), apiclientgen.GetSecretRequestParams{ProjectId: projectID, RequestId: requestID})
		if err != nil {
			return err
		}
		request, err := expectResponse[apimodel.SecretRequest](res)
		if err != nil {
			return err
		}
		return a.writeSecretRequest(cmd, request)
	}}
}

func (a *App) newSecretRequestCreateCommand() *cobra.Command {
	var secretType, host string
	cmd := &cobra.Command{Use: "create --type TYPE", Short: "Request access to a secret", RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		body, err := createSecretRequestBody(secretType, host)
		if err != nil {
			return err
		}
		res, err := client.CreateSecretRequest(cmd.Context(), body, apiclientgen.CreateSecretRequestParams{ProjectId: projectID})
		if err != nil {
			return err
		}
		request, err := expectResponse[apimodel.SecretRequest](res)
		if err != nil {
			return err
		}
		return a.writeSecretRequest(cmd, request)
	}}
	cmd.Flags().StringVar(&secretType, "type", "", "Requested secret type: git, ssh, or bearer")
	cmd.Flags().StringVar(&host, "host", "", "Optional host hint, such as github.com")
	return cmd
}

func (a *App) newSecretRequestApproveCommand() *cobra.Command {
	var secretID string
	var ttl int64
	cmd := &cobra.Command{Use: "approve REQUEST_ID --secret-id SECRET_ID", Short: "Approve a secret request", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		requestID, err := a.resolveSecretRequestID(cmd.Context(), client, projectID, args[0])
		if err != nil {
			return err
		}
		selectedSecretID, err := a.resolveSecretID(cmd.Context(), client, projectID, secretID)
		if err != nil {
			return err
		}
		body := &apimodel.ApproveSecretRequestBody{SecretId: selectedSecretID}
		if ttl > 0 {
			body.SetGrantTTLSeconds(apiclientgen.NewOptInt64(ttl))
		}
		res, err := client.ApproveSecretRequest(cmd.Context(), body, apiclientgen.ApproveSecretRequestParams{ProjectId: projectID, RequestId: requestID})
		if err != nil {
			return err
		}
		request, err := expectResponse[apimodel.SecretRequest](res)
		if err != nil {
			return err
		}
		return a.writeSecretRequest(cmd, request)
	}}
	cmd.Flags().StringVar(&secretID, "secret-id", "", "Secret ID to grant")
	cmd.Flags().Int64Var(&ttl, "grant-ttl", 0, "Grant duration in seconds")
	return cmd
}

func (a *App) newSecretRequestDenyCommand() *cobra.Command {
	return &cobra.Command{Use: "deny REQUEST_ID", Short: "Deny a secret request", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		requestID, err := a.resolveSecretRequestID(cmd.Context(), client, projectID, args[0])
		if err != nil {
			return err
		}
		res, err := client.DenySecretRequest(cmd.Context(), apiclientgen.DenySecretRequestParams{ProjectId: projectID, RequestId: requestID})
		if err != nil {
			return err
		}
		if err := expectNoContent[apiclientgen.DenySecretRequestNoContent](res); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s denied\n", requestID)
		return nil
	}}
}

func addSecretValueFlags(flags *pflag.FlagSet, opts *secretValueOptions) {
	flags.StringVar(&opts.valueJSON, "value-json", "", "Secret value JSON or @path")
	flags.StringVar(&opts.username, "username", "", "Git credential username")
	flags.StringVar(&opts.password, "password", "", "Git credential password")
	flags.StringVar(&opts.privateKey, "private-key", "", "SSH private key PEM")
	flags.StringVar(&opts.passphrase, "passphrase", "", "SSH key passphrase")
	flags.StringVar(&opts.token, "token", "", "Bearer token")
}

func createSecretBody(flags *pflag.FlagSet, name, secretType, host string, autoApprove bool, ttl int64, valueOpts secretValueOptions) (*apimodel.CreateSecretBody, error) {
	secretType = strings.TrimSpace(secretType)
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("secret name is required")
	}
	typed, err := createSecretBodyType(secretType)
	if err != nil {
		return nil, err
	}
	value, err := secretValueFromOptions(flags, valueOpts)
	if err != nil {
		return nil, err
	}
	body := &apimodel.CreateSecretBody{
		Name:  strings.TrimSpace(name),
		Type:  typed,
		Value: value,
	}
	if strings.TrimSpace(host) != "" {
		body.SetHost(apiclientgen.NewOptString(strings.TrimSpace(host)))
	}
	if flags.Changed("auto-approve") {
		body.SetAutoApprove(apiclientgen.NewOptBool(autoApprove))
	}
	if ttl > 0 {
		body.SetDefaultGrantTTLSeconds(apiclientgen.NewOptInt64(ttl))
	}
	return body, nil
}

func updateSecretBody(flags *pflag.FlagSet, name, host string, autoApprove bool, ttl int64, valueOpts secretValueOptions) (*apimodel.UpdateSecretBody, error) {
	body := &apimodel.UpdateSecretBody{}
	if flags.Changed("name") {
		body.SetName(apiclientgen.NewOptString(strings.TrimSpace(name)))
	}
	if flags.Changed("host") {
		body.SetHost(apiclientgen.NewOptString(strings.TrimSpace(host)))
	}
	if flags.Changed("auto-approve") {
		body.SetAutoApprove(apiclientgen.NewOptBool(autoApprove))
	}
	if flags.Changed("grant-ttl") && ttl > 0 {
		body.SetDefaultGrantTTLSeconds(apiclientgen.NewOptInt64(ttl))
	}
	if secretValueFlagsChanged(flags) {
		value, err := secretValueFromOptions(flags, valueOpts)
		if err != nil {
			return nil, err
		}
		body.SetValue(apiclientgen.NewOptSecretValue(value))
	}
	return body, nil
}

func createSecretRequestBody(secretType, host string) (*apimodel.CreateSecretRequestBody, error) {
	typed, err := createSecretRequestBodyType(secretType)
	if err != nil {
		return nil, err
	}
	body := &apimodel.CreateSecretRequestBody{Type: typed}
	if strings.TrimSpace(host) != "" {
		body.SetHost(apiclientgen.NewOptString(strings.TrimSpace(host)))
	}
	return body, nil
}

func secretValueFromOptions(flags *pflag.FlagSet, opts secretValueOptions) (apimodel.SecretValue, error) {
	if strings.TrimSpace(opts.valueJSON) != "" {
		if secretValueFlagsWithoutJSONChanged(flags) {
			return apimodel.SecretValue{}, fmt.Errorf("--value-json cannot be combined with type-specific secret value flags")
		}
		raw, err := rawJSON(opts.valueJSON)
		if err != nil {
			return apimodel.SecretValue{}, err
		}
		var value apimodel.SecretValue
		if err := json.Unmarshal(raw, &value); err != nil {
			return apimodel.SecretValue{}, fmt.Errorf("secret value JSON is invalid: %w", err)
		}
		return value, nil
	}
	value := apimodel.SecretValue{}
	if flags.Changed("username") {
		value.SetUsername(apiclientgen.NewOptString(opts.username))
	}
	if flags.Changed("password") {
		value.SetPassword(apiclientgen.NewOptString(opts.password))
	}
	if flags.Changed("private-key") {
		value.SetPrivateKey(apiclientgen.NewOptString(opts.privateKey))
	}
	if flags.Changed("passphrase") {
		value.SetPassphrase(apiclientgen.NewOptString(opts.passphrase))
	}
	if flags.Changed("token") {
		value.SetToken(apiclientgen.NewOptString(opts.token))
	}
	return value, nil
}

func secretValueFlagsChanged(flags *pflag.FlagSet) bool {
	return flags.Changed("value-json") || secretValueFlagsWithoutJSONChanged(flags)
}

func secretValueFlagsWithoutJSONChanged(flags *pflag.FlagSet) bool {
	for _, name := range []string{"username", "password", "private-key", "passphrase", "token"} {
		if flags.Changed(name) {
			return true
		}
	}
	return false
}

func createSecretBodyType(value string) (apiclientgen.CreateSecretBodyType, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "git":
		return apiclientgen.CreateSecretBodyTypeGit, nil
	case "ssh":
		return apiclientgen.CreateSecretBodyTypeSSH, nil
	case "bearer":
		return apiclientgen.CreateSecretBodyTypeBearer, nil
	default:
		return "", fmt.Errorf("secret type must be git, ssh, or bearer")
	}
}

func createSecretRequestBodyType(value string) (apiclientgen.CreateSecretRequestBodyType, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "git":
		return apiclientgen.CreateSecretRequestBodyTypeGit, nil
	case "ssh":
		return apiclientgen.CreateSecretRequestBodyTypeSSH, nil
	case "bearer":
		return apiclientgen.CreateSecretRequestBodyTypeBearer, nil
	default:
		return "", fmt.Errorf("secret request type must be git, ssh, or bearer")
	}
}
