package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/cli/internal/refreshcmd"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// secretRenewalOptions is how a token is renewed: how long a value lasts, and
// the command that prints a new one (ADR 26-09-25-122 §1).
type secretRenewalOptions struct {
	refreshCommand string
	ttl            *lifetime.Flag
}

func addSecretRenewalFlags(flags *pflag.FlagSet, opts *secretRenewalOptions) {
	flags.StringVar(&opts.refreshCommand, "refresh-command", "", "Command that prints a new value on this machine, such as 'gh auth token'; run without a shell, and offered to whoever renews the token (\"\" removes it)")
	opts.ttl = lifetime.NewFlag()
	flags.Var(opts.ttl, "ttl", "How long a value is good for before a new one is asked for: 5m, 1h, or forever (default 5m with --refresh-command)")
}

// apply sets what the flags say on a body's setters. The command is split as
// a person types it; it is never handed to a shell.
func (o secretRenewalOptions) apply(flags *pflag.FlagSet, setCommand func([]string), setTTL func(int64)) error {
	if flags.Changed("refresh-command") {
		command, err := refreshcmd.Split(o.refreshCommand)
		if err != nil {
			return fmt.Errorf("--refresh-command: %w", err)
		}
		setCommand(command)
	}
	if seconds, given := o.ttl.Seconds(); given {
		setTTL(seconds)
	}
	return nil
}

type secretValueOptions struct {
	valueJSON string
	token     string
	// What an oauth credential needs to renew itself, and the non-secret
	// metadata captured with it.
	refreshToken     string
	tokenURL         string
	clientID         string
	scopes           []string
	expiresAt        int64
	subscriptionType string
	// fields are an exchange credential's NAME=VALUE pairs, a VALUE of -
	// read from stdin, which is where they are read from. recipe is how they
	// are exchanged, JSON or @path; it is not part of the value, and is sent
	// beside it.
	fields []string
	stdin  io.Reader
	recipe string
}

func (a *App) newSecretCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "secret", Aliases: []string{"secrets"}, Short: "Manage project secrets"}
	cmd.AddCommand(a.newSecretListCommand())
	cmd.AddCommand(a.newSecretGetCommand())
	cmd.AddCommand(a.newSecretCreateCommand())
	cmd.AddCommand(a.newSecretUpdateCommand())
	cmd.AddCommand(a.newSecretDeleteCommand())
	cmd.AddCommand(a.newSecretRefreshCommand())
	cmd.AddCommand(a.newSecretRequestCommand())
	cmd.AddCommand(a.newSecretGrantCommand())
	return cmd
}

func (a *App) newSecretGrantCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "grant", Aliases: []string{"grants"}, Short: "Manage standing secret grants (pre-approvals)"}
	cmd.AddCommand(a.newSecretGrantListCommand())
	cmd.AddCommand(a.newSecretGrantCreateCommand())
	cmd.AddCommand(a.newSecretGrantRevokeCommand())
	return cmd
}

func (a *App) newSecretGrantListCommand() *cobra.Command {
	var secretRef string
	cmd := &cobra.Command{Use: "ls", Aliases: []string{"list"}, Short: "List secret grants", RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		params := apiclientgen.ListSecretGrantsParams{ProjectId: projectID}
		if strings.TrimSpace(secretRef) != "" {
			secretID, err := a.resolveSecretID(cmd.Context(), client, projectID, secretRef)
			if err != nil {
				return err
			}
			params.SecretId = apiclientgen.NewOptString(secretID)
		}
		res, err := client.ListSecretGrants(cmd.Context(), params)
		if err != nil {
			return err
		}
		body, err := expectResponse[apimodel.ListSecretGrantsBody](res)
		if err != nil {
			return err
		}
		return a.writeSecretGrants(cmd, body.GetSecretGrants())
	}}
	cmd.Flags().StringVar(&secretRef, "secret", "", "Filter by granted secret ID")
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newSecretGrantCreateCommand() *cobra.Command {
	var secretRef, scope, scopeKey string
	ttl := lifetime.NewFlag()
	var hosts []string
	var delegate bool
	cmd := &cobra.Command{Use: "create --secret SECRET --scope SCOPE", Short: "Create a standing grant (pre-approval)", Long: `Creates a grant directly, without a request to approve.

A grant authorizes the sentinel the discobox, harness config or project is
already provisioned with, for the hosts it names.

With --delegate it is a delegation grant instead: the discobox may give the
credential to the discoboxes it creates, approving their requests or giving it
when it creates them, and may not use it itself. A delegation is one
discobox's, so it takes sandbox scope.`, RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		secretID, err := a.resolveSecretID(cmd.Context(), client, projectID, secretRef)
		if err != nil {
			return err
		}
		typedScope, err := createSecretGrantBodyScope(scope)
		if err != nil {
			return err
		}
		body := &apimodel.CreateSecretGrantBody{SecretId: secretID, Scope: typedScope}
		if strings.TrimSpace(scopeKey) != "" {
			body.SetScopeKey(apiclientgen.NewOptString(strings.TrimSpace(scopeKey)))
		}
		body.Hosts = grantHosts(hosts)
		if seconds, given := ttl.Seconds(); given {
			body.SetGrantTTLSeconds(apiclientgen.NewOptInt64(seconds))
		}
		if delegate {
			body.SetPurpose(apiclientgen.NewOptCreateSecretGrantBodyPurpose(apiclientgen.CreateSecretGrantBodyPurposeDelegate))
		}
		res, err := client.CreateSecretGrant(cmd.Context(), body, apiclientgen.CreateSecretGrantParams{ProjectId: projectID})
		if err != nil {
			return err
		}
		grant, err := expectResponse[apimodel.SecretGrant](res)
		if err != nil {
			return err
		}
		return a.writeSecretGrant(cmd, grant)
	}}
	cmd.Flags().StringVar(&secretRef, "secret", "", "Secret to grant, by name or ID")
	cmd.Flags().StringVar(&scope, "scope", "", "Grant scope: sandbox, harnessConfig, or project")
	cmd.Flags().StringVar(&scopeKey, "scope-key", "", "Discobox ID or harness config ID the scope resolves against (defaults to project ID for project scope)")
	cmd.Flags().StringSliceVar(&hosts, "hosts", nil, "Limit the grant to these hosts, comma-separated or repeated; defaults to the secret's host")
	cmd.Flags().Var(ttl, "grant-ttl", grantTTLCreateFlagUsage)
	cmd.Flags().BoolVar(&delegate, "delegate", false, "Make this a delegation grant: the discobox may give the credential to the discoboxes it creates, within these hosts and this lifetime, and may not use it itself. Sandbox scope only")
	return cmd
}

func (a *App) newSecretGrantRevokeCommand() *cobra.Command {
	return &cobra.Command{Use: "rm GRANT_ID...", Aliases: []string{"revoke", "delete", "remove"}, Short: "Revoke secret grants", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		return runActionMany(cmd, args, "secret grant", "delete", "deleted", func(arg string) (string, error) {
			res, err := client.RevokeSecretGrant(cmd.Context(), apiclientgen.RevokeSecretGrantParams{ProjectId: projectID, GrantId: arg})
			if err != nil {
				return "", err
			}
			if err := expectNoContent[apiclientgen.RevokeSecretGrantNoContent](res); err != nil {
				return "", err
			}
			return arg, nil
		})
	}}
}

func createSecretGrantBodyScope(value string) (apiclientgen.CreateSecretGrantBodyScope, error) {
	switch strings.TrimSpace(value) {
	case "sandbox":
		return apiclientgen.CreateSecretGrantBodyScopeSandbox, nil
	case "harnessConfig", "harness-config":
		return apiclientgen.CreateSecretGrantBodyScopeHarnessConfig, nil
	case "project":
		return apiclientgen.CreateSecretGrantBodyScopeProject, nil
	default:
		return "", fmt.Errorf("grant scope must be sandbox, harnessConfig, or project")
	}
}

func (a *App) newSecretListCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "ls", Aliases: []string{"list"}, Short: "List secrets", RunE: func(cmd *cobra.Command, _ []string) error {
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
	return &cobra.Command{Use: "get SECRET", Short: "Show a secret", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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

// secretFormatFlagUsage is the help for --format on create and update: the
// template grammar, since nothing else in the CLI shows it.
const secretFormatFlagUsage = `Shape of the sentinels that stand in for this secret: literal text with {charset:length} tokens, such as 'sk-ant-oat01-{base64url:95}'. Charsets: digits, hex, HEX, lower, upper, alnum, base62, base32, base64url, base64. Default: read from the value` //nolint:gosec // A template for sentinels, not a credential.

func (a *App) newSecretCreateCommand() *cobra.Command {
	var name, secretType, host, format string
	ttl := lifetime.NewFlag()
	var value secretValueOptions
	var renewal secretRenewalOptions
	cmd := &cobra.Command{Use: "create --name NAME --type TYPE", Short: "Create a secret", Long: `Create a secret.

An exchange credential (--type exchange) is fields such as an API key that
the server trades for a short-lived token, by a recipe given with
--exchange-recipe (JSON, or @path): where to POST, which fields, the body built
from them as {name}, and where the token is in the answer. Its host (--host)
must cover the recipe's URL. The fields are given with --field, a value of -
reading stdin:

  discobox secret create --name boxd --type exchange --host boxd.sh \
    --exchange-recipe '{"url":"https://app.boxd.sh/api/v1/auth/token","fields":["api_key"],"body":{"api_key":"{api_key}"},"tokenPath":"token","expiresAtPath":"expires_at"}' \
    --field api_key=- < key

The server exchanges it at once, refusing a key the provider refuses, and a
discobox is only ever handed the short-lived token.

A token can be given as a value (--token) or got from a command on this
machine (--refresh-command), which is run now for the first value and offered
to whoever renews the token when it goes stale.

A sentinel stands in for the secret inside a discobox, shaped like the value so
a client that checks a key's prefix accepts it. --format sets that shape when
reading it from the value gets it wrong, and is kept when the value changes.`, RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		value.stdin = cmd.InOrStdin()
		body, err := createSecretBody(cmd.Flags(), name, secretType, host, ttl, value)
		if err != nil {
			return err
		}
		if f := strings.TrimSpace(format); f != "" {
			body.SetFormat(apiclientgen.NewOptString(f))
		}
		if err := renewal.apply(cmd.Flags(), func(command []string) {
			body.SetRefreshCommand(apiclientgen.NewOptNilStringArray(command))
		}, func(seconds int64) {
			body.SetTtlSeconds(apiclientgen.NewOptInt64(seconds))
		}); err != nil {
			return err
		}
		// A token got from a command starts with a value: the command is run
		// here, now, for the first one — the same run the window does when a
		// person picks "get it from a command".
		if command, ok := body.RefreshCommand.Get(); ok && len(command) > 0 && !body.Value.Token.IsSet() && !cmd.Flags().Changed("value-json") {
			fmt.Fprintf(cmd.ErrOrStderr(), "running %s for the first value\n", refreshcmd.Join(command))
			token, err := refreshcmd.Run(cmd.Context(), command)
			if err != nil {
				return err
			}
			body.Value.SetToken(apiclientgen.NewOptString(token))
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
	cmd.Flags().StringVar(&secretType, "type", "", "Secret type: token, oauth, or exchange (default token)")
	cmd.Flags().StringVar(&host, "host", "", "Optional host hint, such as github.com")
	cmd.Flags().Var(ttl, "max-grant-ttl", maxGrantTTLCreateFlagUsage)
	cmd.Flags().StringVar(&format, "format", "", secretFormatFlagUsage)
	cmd.Flags().StringArray("route", nil, secretRouteFlagUsage)
	addSecretValueFlags(cmd.Flags(), &value)
	addSecretRenewalFlags(cmd.Flags(), &renewal)
	return cmd
}

func (a *App) newSecretUpdateCommand() *cobra.Command {
	var name, host, format string
	ttl := lifetime.NewFlag()
	var value secretValueOptions
	var renewal secretRenewalOptions
	cmd := &cobra.Command{Use: "update SECRET", Short: "Update a secret", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		value.stdin = cmd.InOrStdin()
		body, err := updateSecretBody(cmd.Flags(), name, host, ttl, value)
		if err != nil {
			return err
		}
		if cmd.Flags().Changed("format") {
			body.SetFormat(apiclientgen.NewOptString(strings.TrimSpace(format)))
		}
		if err := renewal.apply(cmd.Flags(), func(command []string) {
			body.SetRefreshCommand(apiclientgen.NewOptNilStringArray(command))
		}, func(seconds int64) {
			body.SetTtlSeconds(apiclientgen.NewOptInt64(seconds))
		}); err != nil {
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
	cmd.Flags().Var(ttl, "max-grant-ttl", maxGrantTTLUpdateFlagUsage)
	cmd.Flags().StringVar(&format, "format", "", secretFormatFlagUsage+`; "" goes back to that. A running discobox keeps the sentinel already in its environment`)
	cmd.Flags().StringArray("route", nil, secretRouteFlagUsage+"; replaces the secret's routes")
	addClearFlag(cmd, "route", "Remove the secret's routes, leaving where it may be sent to its grants")
	addSecretValueFlags(cmd.Flags(), &value)
	addSecretRenewalFlags(cmd.Flags(), &renewal)
	return cmd
}

func (a *App) newSecretDeleteCommand() *cobra.Command {
	return &cobra.Command{Use: "rm SECRET...", Aliases: []string{"delete", "remove"}, Short: "Delete secrets", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		return runActionMany(cmd, args, "secret", "delete", "deleted", func(arg string) (string, error) {
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
	var status string
	cmd := &cobra.Command{Use: "ls", Aliases: []string{"list"}, Short: "List secret requests", RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		params := apiclientgen.ListSecretRequestsParams{ProjectId: projectID}
		if s := strings.TrimSpace(status); s != "" {
			params.Status = apiclientgen.NewOptListSecretRequestsStatus(apiclientgen.ListSecretRequestsStatus(s))
		}
		res, err := client.ListSecretRequests(cmd.Context(), params)
		if err != nil {
			return err
		}
		body, err := expectResponse[apimodel.ListSecretRequestsBody](res)
		if err != nil {
			return err
		}
		return a.writeSecretRequests(cmd, body.GetSecretRequests())
	}}
	cmd.Flags().StringVar(&status, "status", "", "Filter by status: pending, approved, or denied")
	a.addQuietFlag(cmd)
	return cmd
}

func (a *App) newSecretRequestGetCommand() *cobra.Command {
	return &cobra.Command{Use: "get REQUEST_ID", Short: "Show a secret request", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
	cmd.Flags().StringVar(&secretType, "type", "", "Requested secret type: token or oauth (default token)")
	cmd.Flags().StringVar(&host, "host", "", "Optional host hint, such as github.com")
	return cmd
}

func (a *App) newSecretRequestApproveCommand() *cobra.Command {
	var secretRef, scope string
	ttl := lifetime.NewFlag()
	var hosts []string
	cmd := &cobra.Command{Use: "approve REQUEST_ID --secret SECRET", Short: "Approve a secret request", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		body := &apimodel.ApproveSecretRequestBody{}
		// A discobox approving answers with the secret it was delegated, so
		// the secret is named only when there is a choice to make.
		if strings.TrimSpace(secretRef) != "" {
			selectedSecretID, err := a.resolveSecretID(cmd.Context(), client, projectID, secretRef)
			if err != nil {
				return err
			}
			body.SetSecretId(apiclientgen.NewOptString(selectedSecretID))
		}
		// The lifetime is sent only when one was given. Left out, the server
		// grants what the agent asked for, else an hour, within the secret's
		// limit — what the window opens on — so approving reads nothing first
		// (ADR 26-09-30-782 §4).
		if seconds, given := ttl.Seconds(); given {
			body.SetGrantTTLSeconds(apiclientgen.NewOptInt64(seconds))
		}
		if strings.TrimSpace(scope) != "" {
			typedScope, err := approveSecretRequestBodyScope(scope)
			if err != nil {
				return err
			}
			body.SetScope(apiclientgen.NewOptApproveSecretRequestBodyScope(typedScope))
		}
		body.Hosts = grantHosts(hosts)
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
	cmd.Flags().StringVar(&secretRef, "secret", "", "Secret to grant, by name or ID")
	cmd.Flags().StringVar(&scope, "scope", "", "Grant scope: sandbox, harnessConfig, or project (defaults to sandbox for sandbox requests, else project)")
	cmd.Flags().StringSliceVar(&hosts, "hosts", nil, "Hosts the grant is limited to, comma-separated or repeated; defaults to the hosts the request named")
	cmd.Flags().Var(ttl, "grant-ttl", grantTTLApproveFlagUsage)
	return cmd
}

// grantHosts are the --hosts values as a body takes them: trimmed, with empty
// ones dropped. None at all is nil, which leaves the list out and the server
// its default.
func grantHosts(values []string) []string {
	var hosts []string
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			hosts = append(hosts, value)
		}
	}
	return hosts
}

func approveSecretRequestBodyScope(value string) (apiclientgen.ApproveSecretRequestBodyScope, error) {
	switch strings.TrimSpace(value) {
	case "sandbox":
		return apiclientgen.ApproveSecretRequestBodyScopeSandbox, nil
	case "harnessConfig", "harness-config":
		return apiclientgen.ApproveSecretRequestBodyScopeHarnessConfig, nil
	case "project":
		return apiclientgen.ApproveSecretRequestBodyScopeProject, nil
	default:
		return "", fmt.Errorf("grant scope must be sandbox, harnessConfig, or project")
	}
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
	flags.StringVar(&opts.token, "token", "", "The credential itself, or an OAuth access token")
	flags.StringVar(&opts.refreshToken, "refresh-token", "", "OAuth refresh token, spent server-side to renew the access token")
	flags.StringVar(&opts.tokenURL, "token-url", "", "Where the refresh token is exchanged for a new access token")
	flags.StringVar(&opts.clientID, "client-id", "", "OAuth client the grant belongs to")
	flags.StringArrayVar(&opts.scopes, "scope", nil, "What the grant may do, as the authorization server returned it (repeatable)")
	flags.Int64Var(&opts.expiresAt, "access-token-expires-at", 0, "When the access token goes stale, unix milliseconds")
	flags.StringVar(&opts.subscriptionType, "subscription-type", "", "The plan or account kind the grant belongs to")
	flags.StringArrayVar(&opts.fields, "field", nil, "An exchange credential's field as NAME=VALUE, such as api_key=bxd_...; a VALUE of - reads it from stdin (repeatable)")
	flags.StringVar(&opts.recipe, "exchange-recipe", "", "How an exchange credential's fields are traded for a token: recipe JSON or @path (url, fields, body, header, form, tokenPath, expiresAtPath, expiresInPath). On update, given only with --field")
}

// The two lifetime flags, said once. A grant's lifetime and a credential's
// ceiling on grant lifetimes are the same kind of value and are typed by the
// same person in the same session, so they take the same words — the words the
// window's picker offers (see cli/internal/lifetime).
//
// Each command says what leaving its flag out means, because they differ:
// approving a request grants for what the agent asked, else agentcreds.DefaultGrantTTL,
// as the window does; a
// standing grant takes the secret's limit; a new secret's limit is the
// server's default; and update leaves the limit as it is.
const (
	grantTTLApproveFlagUsage   = "How long the grant lives: 1h, 90m, 3d, 2w, 1mo, or forever (default: what the agent asked for, else 1h, within the secret's limit)"
	grantTTLCreateFlagUsage    = "How long the grant lives: 1h, 90m, 3d, 2w, 1mo, or forever (default: the secret's limit)"
	maxGrantTTLCreateFlagUsage = "Longest a grant on this secret may live: 1h, 3d, 2w, 1mo, or forever (default 1h)"
	maxGrantTTLUpdateFlagUsage = "Longest a grant on this secret may live: 1h, 3d, 2w, 1mo, or forever (omit to leave it as it is)"
)

func createSecretBody(flags *pflag.FlagSet, name, secretType, host string, ttl *lifetime.Flag, valueOpts secretValueOptions) (*apimodel.CreateSecretBody, error) {
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
	if recipe, ok, err := exchangeRecipeFlag(valueOpts.recipe); err != nil {
		return nil, err
	} else if ok {
		body.SetExchange(apiclientgen.NewOptExchangeRecipe(recipe))
	}
	if strings.TrimSpace(host) != "" {
		body.SetHost(apiclientgen.NewOptString(strings.TrimSpace(host)))
	}
	if seconds, given := ttl.Seconds(); given {
		body.SetMaxGrantTTLSeconds(apiclientgen.NewOptInt64(seconds))
	}
	if routes, ok, err := secretRoutesFlags(flags); err != nil {
		return nil, err
	} else if ok {
		body.SetRoutes(apiclientgen.NewOptNilSecretRouteInputArray(routes))
	}
	return body, nil
}

const secretRouteFlagUsage = "Where the secret may be sent, whoever holds it, as HOST[/PATH][=blocked]: a host covers itself and every host beneath it, a path is a prefix on whole segments, and a blocked route refuses every request it is the most specific route for. Every request it is sent in must fall on one; the most specific decides (repeatable)"

// secretRoutesFlags reads --route, and --clear-route where the command has
// it. ok reports whether either was given; --clear-route is an empty list,
// which clears.
func secretRoutesFlags(flags *pflag.FlagSet) (routes []apimodel.SecretRouteInput, ok bool, err error) {
	if listCleared(flags, "route") {
		return []apimodel.SecretRouteInput{}, true, nil
	}
	if !flags.Changed("route") {
		return nil, false, nil
	}
	values, err := flags.GetStringArray("route")
	if err != nil {
		return nil, false, err
	}
	for _, value := range values {
		parsed, err := secretroute.Parse(value)
		if err != nil {
			return nil, false, fmt.Errorf("--route %q: %w", value, err)
		}
		route := apimodel.SecretRouteInput{Host: parsed.Host}
		if parsed.Path != "" {
			route.SetPath(apiclientgen.NewOptString(parsed.Path))
		}
		if parsed.Blocked {
			route.SetBlocked(apiclientgen.NewOptBool(true))
		}
		routes = append(routes, route)
	}
	return routes, true, nil
}

func updateSecretBody(flags *pflag.FlagSet, name, host string, ttl *lifetime.Flag, valueOpts secretValueOptions) (*apimodel.UpdateSecretBody, error) {
	body := &apimodel.UpdateSecretBody{}
	if flags.Changed("name") {
		body.SetName(apiclientgen.NewOptString(strings.TrimSpace(name)))
	}
	if flags.Changed("host") {
		body.SetHost(apiclientgen.NewOptString(strings.TrimSpace(host)))
	}
	if seconds, given := ttl.Seconds(); given {
		body.SetMaxGrantTTLSeconds(apiclientgen.NewOptInt64(seconds))
	}
	if secretValueFlagsChanged(flags) {
		value, err := secretValueFromOptions(flags, valueOpts)
		if err != nil {
			return nil, err
		}
		body.SetValue(apiclientgen.NewOptSecretValue(value))
	}
	if recipe, ok, err := exchangeRecipeFlag(valueOpts.recipe); err != nil {
		return nil, err
	} else if ok {
		body.SetExchange(apiclientgen.NewOptExchangeRecipe(recipe))
	}
	if routes, ok, err := secretRoutesFlags(flags); err != nil {
		return nil, err
	} else if ok {
		body.SetRoutes(apiclientgen.NewOptNilSecretRouteInputArray(routes))
	}
	return body, nil
}

// exchangeRecipeFlag reads --exchange-recipe: JSON, or @path to a file of it.
func exchangeRecipeFlag(flag string) (apiclientgen.ExchangeRecipe, bool, error) {
	raw, err := rawJSON(flag)
	if err != nil {
		return apiclientgen.ExchangeRecipe{}, false, fmt.Errorf("--exchange-recipe: %w", err)
	}
	if raw == nil {
		return apiclientgen.ExchangeRecipe{}, false, nil
	}
	var recipe apiclientgen.ExchangeRecipe
	if err := recipe.UnmarshalJSON(raw); err != nil {
		return apiclientgen.ExchangeRecipe{}, false, fmt.Errorf("--exchange-recipe is not a recipe: %w", err)
	}
	return recipe, true, nil
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
	if flags.Changed("token") {
		value.SetToken(apiclientgen.NewOptString(opts.token))
	}
	if flags.Changed("refresh-token") {
		value.SetRefreshToken(apiclientgen.NewOptString(opts.refreshToken))
	}
	if flags.Changed("token-url") {
		value.SetTokenUrl(apiclientgen.NewOptString(opts.tokenURL))
	}
	if flags.Changed("client-id") {
		value.SetClientId(apiclientgen.NewOptString(opts.clientID))
	}
	if flags.Changed("scope") {
		value.SetScopes(apiclientgen.NewOptNilStringArray(opts.scopes))
	}
	if flags.Changed("access-token-expires-at") {
		value.SetAccessTokenExpiresAt(apiclientgen.NewOptInt64(opts.expiresAt))
	}
	if flags.Changed("subscription-type") {
		value.SetSubscriptionType(apiclientgen.NewOptString(opts.subscriptionType))
	}
	if flags.Changed("field") {
		fields := apiclientgen.SecretValueExchange{}
		for _, pair := range opts.fields {
			name, given, ok := strings.Cut(pair, "=")
			if name = strings.TrimSpace(name); !ok || name == "" {
				return apimodel.SecretValue{}, fmt.Errorf("--field %q is not NAME=VALUE", pair)
			}
			entered, err := enteredValue("--field "+name, given, opts.stdin)
			if err != nil {
				return apimodel.SecretValue{}, err
			}
			fields[name] = entered
		}
		value.SetExchange(apiclientgen.NewOptSecretValueExchange(fields))
	}
	return value, nil
}

func secretValueFlagsChanged(flags *pflag.FlagSet) bool {
	return flags.Changed("value-json") || secretValueFlagsWithoutJSONChanged(flags)
}

func secretValueFlagsWithoutJSONChanged(flags *pflag.FlagSet) bool {
	for _, name := range []string{"token", "refresh-token", "token-url", "client-id", "scope", "access-token-expires-at", "subscription-type", "field"} {
		if flags.Changed(name) {
			return true
		}
	}
	return false
}

func createSecretBodyType(value string) (apiclientgen.CreateSecretBodyType, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "token":
		return apiclientgen.CreateSecretBodyTypeToken, nil
	case "oauth":
		return apiclientgen.CreateSecretBodyTypeOAuth, nil
	case "exchange":
		return apiclientgen.CreateSecretBodyTypeExchange, nil
	default:
		return "", fmt.Errorf("secret type must be token, oauth, or exchange")
	}
}

func createSecretRequestBodyType(value string) (apiclientgen.CreateSecretRequestBodyType, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "token":
		return apiclientgen.CreateSecretRequestBodyTypeToken, nil
	case "oauth":
		return apiclientgen.CreateSecretRequestBodyTypeOAuth, nil
	default:
		return "", fmt.Errorf("secret type must be token or oauth")
	}
}
