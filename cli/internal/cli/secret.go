package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/apiroute"
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
	cmd.AddCommand(a.newSecretBindCommand(), a.newSecretUnbindCommand(), a.newSecretBoundCommand())
	return cmd
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
	var name, secretType, format string
	ttl := lifetime.NewFlag()
	var value secretValueOptions
	var renewal secretRenewalOptions
	cmd := &cobra.Command{Use: "create --name NAME --type TYPE", Short: "Create a secret", Long: `Create a secret.

An exchange credential (--type exchange) is fields such as an API key that
the server trades for a short-lived token, by a recipe given with
--exchange-recipe (JSON, or @path): where to POST, which fields, the body built
from them as {name}, and where the token is in the answer. The fields are given
with --field, a value of - reading stdin:

  discobox secret create --name boxd --type exchange --route boxd.sh \
    --exchange-recipe '{"url":"https://app.boxd.sh/api/v1/auth/token","fields":["api_key"],"body":{"api_key":"{api_key}"},"tokenPath":"token","expiresAtPath":"expires_at"}' \
    --field api_key=- < key

The server exchanges it at once, refusing a key the provider refuses, and a
discobox is only ever handed the short-lived token.

Every secret says where it may be sent, with --route, '*' for anywhere. A
secret with no routes is sent nowhere.

A token can be given as a value (--token) or got from a command on this
machine (--refresh-command), which is run now for the first value and offered
to whoever renews the token when it goes stale.

A sentinel stands in for the secret inside a discobox, shaped like the value so
a client that checks a key's prefix accepts it. --format sets that shape when
reading it from the value gets it wrong, and is kept when the value changes.

` + routeHelp, RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := a.apiClient()
		if err != nil {
			return err
		}
		projectID, err := a.projectIDValue()
		if err != nil {
			return err
		}
		value.stdin = cmd.InOrStdin()
		body, err := createSecretBody(cmd.Flags(), name, secretType, ttl, value)
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
	cmd.Flags().Var(ttl, "max-binding-ttl", maxBindingTTLCreateFlagUsage)
	cmd.Flags().Bool("allow-raw", false, allowRawFlagUsage)
	cmd.Flags().StringVar(&format, "format", "", secretFormatFlagUsage)
	cmd.Flags().StringArray("route", nil, secretRouteFlagUsage)
	addSecretValueFlags(cmd.Flags(), &value)
	addSecretRenewalFlags(cmd.Flags(), &renewal)
	return cmd
}

func (a *App) newSecretUpdateCommand() *cobra.Command {
	var name, format string
	ttl := lifetime.NewFlag()
	var value secretValueOptions
	var renewal secretRenewalOptions
	cmd := &cobra.Command{Use: "update SECRET", Short: "Update a secret", Long: `Update a secret: its name, value, renewal, sentinel format, binding limit
or routes. --route replaces every route the secret has; --clear-route leaves
it none, so it is sent nowhere.

` + routeHelp, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		body, err := updateSecretBody(cmd.Flags(), name, ttl, value)
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
	cmd.Flags().Var(ttl, "max-binding-ttl", maxBindingTTLUpdateFlagUsage)
	cmd.Flags().Bool("allow-raw", false, allowRawFlagUsage+"; --allow-raw=false removes the raw bindings that stand")
	cmd.Flags().StringVar(&format, "format", "", secretFormatFlagUsage+`; "" goes back to that. A running discobox keeps the sentinel already in its environment`)
	cmd.Flags().StringArray("route", nil, secretRouteFlagUsage+"; replaces the secret's routes")
	addClearFlag(cmd, "route", "Remove the secret's routes, so it is sent nowhere until it has some again")
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

func (a *App) newSecretRequestApproveCommand() *cobra.Command {
	var secretRef string
	var raw rawFlag
	var routes []string
	lifetimeFlag := lifetime.NewFlag()
	cmd := &cobra.Command{Use: "approve REQUEST_ID", Short: "Approve a secret request, binding the secret to the discobox that asked", Long: `Approve a secret request: the secret is bound to the discobox that asked,
in the variable it asked for, or delegated to it when it asked to delegate.

--secret names the secret that answers it when the request named one that is
not a project secret's name, or to choose another. --route narrows where it may be
sent; left out, the binding is held to the routes the request named. --for is
how long it lasts; left out, what the request asked for, else an hour, within
the secret's limit.

--raw grants the value itself rather than a sentinel, for a program that
signs with it or sends it where an exchange cannot reach: env puts it in the
variable, file the path of a file in memory holding it. It is yours to choose
— a request never asks for it — and the secret must allow it (secret update
--allow-raw). The discobox can then send the value anywhere; its pool only
refuses a request carrying it off its routes.

` + routeHelp, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		parsed, err := routeFlagValues("--route", routes)
		if err != nil {
			return err
		}
		body.Routes = parsed
		// Sent only when given: left out, the server binds for what the
		// request asked, else an hour, within the secret's limit — what the
		// window opens on — so approving reads nothing first.
		if seconds, given := lifetimeFlag.Seconds(); given {
			body.SetTtlSeconds(apiclientgen.NewOptInt64(seconds))
		}
		if raw.set {
			body.SetRaw(apiclientgen.NewOptApproveSecretRequestBodyRaw(apiclientgen.ApproveSecretRequestBodyRaw(raw.value)))
		}
		res, err := client.ApproveSecretRequest(cmd.Context(), body, apiclientgen.ApproveSecretRequestParams{ProjectId: projectID, RequestId: requestID})
		if err != nil {
			return err
		}
		request, err := expectResponse[apimodel.SecretRequest](res)
		if err != nil {
			return err
		}
		bindingID, made := request.BindingId.Get()
		if a.output == "json" || a.quiet || !made || bindingID == "" {
			return a.writeSecretRequest(cmd, request)
		}
		// What was approved is the binding, not the ask: its routes and
		// lifetime can be narrower than what the agent asked for, so they are
		// read back and said as `secret bind` says them.
		return a.writeApprovedBinding(cmd, client, projectID, request, bindingID)
	}}
	cmd.Flags().StringVar(&secretRef, "secret", "", "The secret that answers the request, by name or ID")
	cmd.Flags().StringArrayVar(&routes, "route", nil, "Where it may be sent, narrowing the secret's routes, as "+secretroute.Syntax+"; one per flag, repeat for more (default the routes the request named)")
	cmd.Flags().Var(lifetimeFlag, "for", bindingLifetimeApproveFlagUsage)
	cmd.Flags().Var(&raw, "raw", "Grant the value itself: env (in the variable) or file (a file whose path is in it); left out, a sentinel")
	return cmd
}

// writeApprovedBinding says what an approval granted, in the sentence
// `secret bind` prints: the variable, where it may be sent, how long.
func (a *App) writeApprovedBinding(cmd *cobra.Command, client *apiclientgen.Client, projectID string, request *apimodel.SecretRequest, bindingID string) error {
	res, err := client.GetSecretBinding(cmd.Context(), apiclientgen.GetSecretBindingParams{ProjectId: projectID, BindingId: bindingID})
	if err != nil {
		return err
	}
	binding, err := expectResponse[apimodel.SecretBinding](res)
	if err != nil {
		return err
	}
	secret, err := a.getSecret(cmd.Context(), client, projectID, binding.SecretId)
	if err != nil {
		return err
	}
	box := request.SandboxId.Or(binding.TargetId)
	if res, err := client.GetSandbox(cmd.Context(), apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: box}); err == nil {
		if sandbox, ok := res.(*apimodel.Sandbox); ok && sandbox.DisplayName != "" {
			box = sandbox.DisplayName
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Approved %s.\n", request.ID)
	return writeBound(cmd.OutOrStdout(), binding, secret, bindingTarget{kind: apiclientgen.CreateSecretBindingBodyTargetKindSandbox, said: box}, time.Now())
}

// routeFlagValues reads a repeatable --route flag into what the API takes:
// one route per flag, read by secretroute.Parse — commas belong to the route,
// between its methods, hosts, paths, fields or tools. None is nil, which
// leaves the list out and the server its default.
func routeFlagValues(flag string, values []string) ([]apimodel.SecretRouteInput, error) {
	var routes []apimodel.SecretRouteInput
	for _, value := range values {
		route, err := secretroute.Parse(value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", flag, err)
		}
		routes = append(routes, apiroute.Input(route))
	}
	return routes, nil
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

// The lifetime flags, said once. A binding's lifetime and a secret's ceiling
// on binding lifetimes are the same kind of value, typed by the same person in
// the same session, so they take the same words — the words the window's
// picker offers (see cli/internal/lifetime).
//
// Each says what leaving it out means, because they differ: approving a
// request binds for what it asked, else an hour, as the window does; a binding
// a person makes takes the secret's limit; a new secret's limit is the
// server's default; and update leaves the limit as it is.
const (
	bindingLifetimeApproveFlagUsage = "How long the binding lasts: 1h, 90m, 3d, 2w, 1mo, or forever (default: what the request asked for, else 1h, within the secret's limit)"
	bindingLifetimeFlagUsage        = "How long the binding lasts: 1h, 90m, 3d, 2w, 1mo, or forever (default: the secret's limit)"
	maxBindingTTLCreateFlagUsage    = "Longest a binding of this secret may last: 1h, 3d, 2w, 1mo, or forever (default 1h)"
	maxBindingTTLUpdateFlagUsage    = "Longest a binding of this secret may last: 1h, 3d, 2w, 1mo, or forever (omit to leave it as it is)"
)

func createSecretBody(flags *pflag.FlagSet, name, secretType string, ttl *lifetime.Flag, valueOpts secretValueOptions) (*apimodel.CreateSecretBody, error) {
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
	if seconds, given := ttl.Seconds(); given {
		body.SetMaxBindingTTLSeconds(apiclientgen.NewOptInt64(seconds))
	}
	if flags.Changed("allow-raw") {
		allow, err := flags.GetBool("allow-raw")
		if err != nil {
			return nil, err
		}
		body.SetAllowRaw(apiclientgen.NewOptBool(allow))
	}
	// A secret with no routes is sent nowhere (ADR 26-10-27-880 §1), so one
	// made here says where it may go: '*' is how a person says anywhere
	// (ADR 26-10-27-939).
	routes, ok, err := secretRoutesFlags(flags)
	if err != nil {
		return nil, err
	}
	if !ok || len(routes) == 0 {
		return nil, fmt.Errorf("say where the secret may be sent with --route ROUTE, repeated for more, or --route '*' for anywhere; see --help for a route's form")
	}
	body.SetRoutes(apiclientgen.NewOptNilSecretRouteInputArray(routes))
	return body, nil
}

const allowRawFlagUsage = "Let a person hand a discobox the value itself (secret bind --raw, secret request approve --raw), rather than only a sentinel its pool exchanges"

const secretRouteFlagUsage = "Where the secret may be sent, whoever holds it, as " + secretroute.Syntax + ", or '*' for any host: a host is exact, *.h is the hosts beneath h. Every request it is sent in must fall on one; the most specific decides (repeatable)"

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
	routes, err = routeFlagValues("--route", values)
	if err != nil {
		return nil, false, err
	}
	return routes, true, nil
}

func updateSecretBody(flags *pflag.FlagSet, name string, ttl *lifetime.Flag, valueOpts secretValueOptions) (*apimodel.UpdateSecretBody, error) {
	body := &apimodel.UpdateSecretBody{}
	if flags.Changed("name") {
		body.SetName(apiclientgen.NewOptString(strings.TrimSpace(name)))
	}
	if seconds, given := ttl.Seconds(); given {
		body.SetMaxBindingTTLSeconds(apiclientgen.NewOptInt64(seconds))
	}
	if flags.Changed("allow-raw") {
		allow, err := flags.GetBool("allow-raw")
		if err != nil {
			return nil, err
		}
		body.SetAllowRaw(apiclientgen.NewOptBool(allow))
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
