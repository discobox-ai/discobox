package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/apiroute"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/cli/internal/refreshcmd"
	"github.com/discobox-ai/discobox/pkg/bindingplan"
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
	var secretRef string
	cmd := &cobra.Command{Use: "get REQUEST_ID", Short: "Show a secret request", Long: `Show a secret request: what each part asks, the roles it was asked by beside
what their routes grant, and the answer once there is one.

A pending request with a use part shows what applies in its variable now,
grant by grant with its lifetime, the routes it asks that none of them names,
and what granting it each way would do: as an addition, for its own lifetime
beside what the discobox holds, or as a replacement of it, naming any routes a
replacement would cut short or shadow.

--secret, on a pending request, shows what approving every part with that
secret, the use part as the request asks, would do to the bindings the
discobox already holds of it: a part approved to replace replaces them, so
access it loses is read before it is approved. approve --secret says it again
for the parts it approves.`, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		var replacements []partReplacement
		if strings.TrimSpace(secretRef) != "" {
			if request.Status != apiclientgen.SecretRequestStatusPending {
				return fmt.Errorf("--secret shows what approving would replace; request %s is already %s", request.ID, request.Status)
			}
			secretID, err := a.resolveSecretID(cmd.Context(), client, projectID, secretRef)
			if err != nil {
				return err
			}
			if !a.usesSandboxRole() {
				if replacements, err = a.requestReplacements(cmd.Context(), client, projectID, request, secretID, askedAs(request), true); err != nil {
					return err
				}
			}
		}
		var grants *useGrants
		if request.Status == apiclientgen.SecretRequestStatusPending && a.output != "json" && !a.usesSandboxRole() {
			if grants, err = a.requestUseGrants(cmd.Context(), client, projectID, request); err != nil {
				return err
			}
		}
		return a.writeSecretRequest(cmd, request, replacements, grants)
	}}
	cmd.Flags().StringVar(&secretRef, "secret", "", "On a pending request: show what approving it with this secret, by name or ID, would replace")
	return cmd
}

func (a *App) newSecretRequestApproveCommand() *cobra.Command {
	var secretRef string
	var raw rawFlag
	var routes, delegateRoutes []string
	var useDecision, delegateDecision decisionFlag
	var useAs asFlag
	var useScope, delegateScope scopeFlag
	lifetimeFlag := lifetime.NewFlag()
	cmd := &cobra.Command{Use: "approve REQUEST_ID", Short: "Answer a secret request, binding the secret to the discobox that asked", Long: `Answer a secret request: each part approved binds the secret to the discobox
that asked. A request has a use part — the secret in the variable it asked for
— a delegate part — the secret given on to the discoboxes it leads — or both.

An answer decides every part the request has. A request with one part is
approved by this command; one with both needs a decision for each, --use and
--delegate, approve or deny, and is refused without one. A part approved
replaces the discobox's own bindings of that kind for the secret; a part
denied leaves them as they are.

--use-as add grants the use part as an addition instead: a binding of its own
beside what the discobox holds in the variable, for its own lifetime, which
leaves that as it is and alone goes when it lapses.
--use-as replace makes it the discobox's whole use of the secret. Left out, the
use part is granted as the request asked, which is replace unless the discobox
asked to add. secret request get shows what each would do first.

--secret names the secret that answers it when the request named one that is
not a project secret's name, or to choose another. --route narrows where the
use part may send it, --delegate-route what the delegate part may hand on;
left out, a part is held to the routes it asked for. --for is how long each
part approved lasts; left out, what the request asked for, else an hour,
within the secret's limit.

--use-scope and --delegate-scope choose routes (requested routes, the default),
hosts (any method, path and protocol on requested hosts), or anywhere (no grant
restriction). Credential and parent scope still limit access. Route overrides
require routes scope. Each part has its own choice.

--raw grants the value itself rather than a sentinel, for a program that
signs with it or sends it where an exchange cannot reach: env puts it in the
variable, file the path of a file in memory holding it. It is yours to choose
— a request never asks for it — the secret must allow it (secret update
--allow-raw), and only the use part takes it. The discobox can then send the
value anywhere; its pool only refuses a request carrying it off its routes.

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
		// Which parts the request has decides what the answer must say, so it
		// is read first.
		res, err := client.GetSecretRequest(cmd.Context(), apiclientgen.GetSecretRequestParams{ProjectId: projectID, RequestId: requestID})
		if err != nil {
			return err
		}
		asked, err := expectResponse[apimodel.SecretRequest](res)
		if err != nil {
			return err
		}
		useRoutes, err := routeFlagValues("--route", routes)
		if err != nil {
			return err
		}
		handedOn, err := routeFlagValues("--delegate-route", delegateRoutes)
		if err != nil {
			return err
		}
		body, err := approvalBody(asked, approvalAnswer{
			use:            useDecision,
			useAs:          useAs,
			useScope:       useScope,
			delegateScope:  delegateScope,
			delegate:       delegateDecision,
			useRoutes:      useRoutes,
			delegateRoutes: handedOn,
			raw:            raw,
			lifetime:       lifetimeFlag,
		})
		if err != nil {
			return err
		}
		// A discobox approving answers with the secret it was delegated, so
		// the secret is named only when there is a choice to make.
		// What the approval replaces is read before it is sent, while the
		// discobox still holds it, and said after.
		var replacements []partReplacement
		if strings.TrimSpace(secretRef) != "" {
			selectedSecretID, err := a.resolveSecretID(cmd.Context(), client, projectID, secretRef)
			if err != nil {
				return err
			}
			body.SetSecretId(apiclientgen.NewOptString(selectedSecretID))
			if !a.usesSandboxRole() {
				if replacements, err = a.requestReplacements(cmd.Context(), client, projectID, asked, selectedSecretID, grantedAs(asked, body.Use), approves(body.Delegate)); err != nil {
					return err
				}
			}
		}
		res2, err := client.ApproveSecretRequest(cmd.Context(), body, apiclientgen.ApproveSecretRequestParams{ProjectId: projectID, RequestId: requestID})
		if err != nil {
			return err
		}
		request, err := expectResponse[apimodel.SecretRequest](res2)
		if err != nil {
			return err
		}
		if a.output == "json" || a.quiet {
			return a.writeSecretRequest(cmd, request, nil, nil)
		}
		if a.usesSandboxRole() {
			fmt.Fprintf(cmd.OutOrStdout(), "Request %s %s\n", terminalSafe(request.ID), request.Status)
			for _, part := range []apiclientgen.OptSecretRequestPart{request.Use, request.Delegate} {
				if p, ok := part.Get(); ok {
					if receipt, ok := p.Receipt.Get(); ok {
						if err := writeGrantReceipt(cmd.OutOrStdout(), receipt); err != nil {
							return err
						}
					}
				}
			}
			return nil
		}
		// What was approved is each binding, not the ask: its routes and
		// lifetime can be narrower than what the agent asked for, so they are
		// read back and said as `secret bind` says them.
		return a.writeApprovedParts(cmd, client, projectID, request, replacements)
	}}
	cmd.Flags().StringVar(&secretRef, "secret", "", "The secret that answers the request, by name or ID")
	cmd.Flags().Var(&useDecision, "use", "The use part's answer: approve or deny (required when the request also asks to delegate)")
	cmd.Flags().Var(&useScope, "use-scope", "Grant restriction: routes (requested routes), hosts (any method, path and protocol on requested hosts), anywhere (no grant restriction); still limited by credential and parent scope")
	cmd.Flags().Var(&delegateScope, "delegate-scope", "Delegation restriction: routes, hosts or anywhere; default requested routes")
	cmd.Flags().Var(&useAs, "use-as", "How an approved use part is granted: add (an addition beside what the discobox holds, for its own lifetime) or replace (its whole use of the secret); default what the request asked")
	cmd.Flags().Var(&delegateDecision, "delegate", "The delegate part's answer: approve or deny (required when the request also asks to use)")
	cmd.Flags().StringArrayVar(&routes, "route", nil, "Where the use part may send it, narrowing the secret's routes, as "+secretroute.Syntax+"; one per flag, repeat for more (default the routes the part named)")
	cmd.Flags().StringArrayVar(&delegateRoutes, "delegate-route", nil, "What the delegate part may hand on, as a route like --route; one per flag (default the routes the part named)")
	cmd.Flags().Var(lifetimeFlag, "for", bindingLifetimeApproveFlagUsage)
	cmd.Flags().Var(&raw, "raw", "Use part only: grant the value itself, env (in the variable) or file (a file whose path is in it); left out, a sentinel")
	return cmd
}

// decisionFlag is --use and --delegate: a part's answer, approve or deny.
type decisionFlag struct{ value string }

func (f *decisionFlag) Set(text string) error {
	switch text = strings.TrimSpace(text); text {
	case "approve", "deny":
		f.value = text
		return nil
	}
	return fmt.Errorf("%q is not an answer: approve or deny", text)
}

// String is empty until set, so help prints no default: what leaving it out
// means is the usage's to say.
func (f *decisionFlag) String() string { return f.value }

func (f *decisionFlag) Type() string { return "answer" }

// scopeFlag is an explicit per-part grant restriction.
type scopeFlag struct{ value string }

func (f *scopeFlag) Set(text string) error {
	switch text {
	case "routes", "hosts", "anywhere":
		f.value = text
		return nil
	}
	return fmt.Errorf("%q is not an approval scope: routes, hosts or anywhere", text)
}
func (f *scopeFlag) String() string { return f.value }
func (f *scopeFlag) Type() string   { return "routes|hosts|anywhere" }

// asFlag is --use-as: how an approved use part is granted.
type asFlag struct{ value bindingplan.As }

func (f *asFlag) Set(text string) error {
	switch as := bindingplan.As(strings.TrimSpace(text)); as {
	case bindingplan.Add, bindingplan.Replace:
		f.value = as
		return nil
	}
	return fmt.Errorf("%q is not a way to grant it: add or replace", text)
}

// String is empty until set: left out is what the request asked.
func (f *asFlag) String() string { return string(f.value) }

func (f *asFlag) Type() string { return "add|replace" }

// grantedAs is how an answer grants a request's use part: the answer's way
// when it names one, else the request's; empty when the use part is not
// approved.
func grantedAs(request *apimodel.SecretRequest, decision apiclientgen.OptSecretRequestPartDecision) bindingplan.As {
	part, ok := decision.Get()
	if !ok || part.Decision != apiclientgen.SecretRequestPartDecisionDecisionApprove {
		return ""
	}
	if as, ok := part.As.Get(); ok {
		return bindingplan.As(as)
	}
	return askedAs(request)
}

// approvalAnswer is what the approve command was told about each part.
type approvalAnswer struct {
	use, delegate             decisionFlag
	useAs                     asFlag
	useScope, delegateScope   scopeFlag
	useRoutes, delegateRoutes []apimodel.SecretRouteInput
	raw                       rawFlag
	lifetime                  *lifetime.Flag
}

// approvalBody turns the flags into an answer naming a decision for every
// part the request has (ADR 26-11-02-022 §4): the server refuses one that
// leaves a part out, so it is refused here first, naming the flag. A request
// with one part is approved by default; one with both is not, since
// approving a delegation nobody looked at is what the rule is for.
func approvalBody(request *apimodel.SecretRequest, answer approvalAnswer) (*apimodel.ApproveSecretRequestBody, error) {
	_, hasUse := request.Use.Get()
	_, hasDelegate := request.Delegate.Get()
	if !hasUse && !hasDelegate {
		// A request from before parts: it asks to use.
		hasUse = true
	}
	switch {
	case answer.use.value != "" && !hasUse:
		return nil, fmt.Errorf("--use: request %s does not ask to use the credential; answer its delegate part with --delegate", request.ID)
	case answer.delegate.value != "" && !hasDelegate:
		return nil, fmt.Errorf("--delegate: request %s does not ask to delegate the credential; answer its use part with --use", request.ID)
	case hasUse && hasDelegate && (answer.use.value == "" || answer.delegate.value == ""):
		missing := "--use"
		if answer.use.value != "" {
			missing = "--delegate"
		}
		return nil, fmt.Errorf("request %s asks both to use the credential and to delegate it: answer each part, --use approve|deny and --delegate approve|deny (missing %s)", request.ID, missing)
	}
	decide := func(given string) string {
		if given == "" {
			return "approve"
		}
		return given
	}
	body := &apimodel.ApproveSecretRequestBody{}
	seconds, timed := answer.lifetime.Seconds()
	if hasUse {
		decision := decide(answer.use.value)
		part := apimodel.SecretRequestPartDecision{Decision: apiclientgen.SecretRequestPartDecisionDecision(decision)}
		if decision == "approve" {
			if answer.useScope.value != "" {
				if answer.useScope.value != "routes" && answer.useRoutes != nil {
					return nil, fmt.Errorf("--route requires --use-scope routes")
				}
				part.Scope = apiclientgen.NewOptSecretRequestPartDecisionScope(apiclientgen.SecretRequestPartDecisionScope(answer.useScope.value))
			}
			part.Routes = answer.useRoutes
			// Sent only when given: left out, the server binds for what the
			// request asked, else an hour, within the secret's limit — what
			// the window opens on.
			if timed {
				part.TtlSeconds = apiclientgen.NewOptInt64(seconds)
			}
			if answer.raw.set {
				part.Raw = apiclientgen.NewOptSecretRequestPartDecisionRaw(apiclientgen.SecretRequestPartDecisionRaw(answer.raw.value))
			}
			if answer.useAs.value != "" {
				part.As = apiclientgen.NewOptSecretRequestPartDecisionAs(apiclientgen.SecretRequestPartDecisionAs(answer.useAs.value))
			}
		} else if len(answer.useRoutes) > 0 || answer.raw.set || answer.useAs.value != "" || answer.useScope.value != "" {
			return nil, fmt.Errorf("--route, --raw, --use-as and --use-scope are for an approved use part; the use part is denied")
		}
		body.Use = apiclientgen.NewOptSecretRequestPartDecision(part)
	} else if len(answer.useRoutes) > 0 || answer.raw.set || answer.useAs.value != "" || answer.useScope.value != "" {
		return nil, fmt.Errorf("--route, --raw, --use-as and --use-scope are the use part's; request %s asks only to delegate (--delegate-route narrows what it hands on)", request.ID)
	}
	if hasDelegate {
		decision := decide(answer.delegate.value)
		part := apimodel.SecretRequestPartDecision{Decision: apiclientgen.SecretRequestPartDecisionDecision(decision)}
		if decision == "approve" {
			if answer.delegateScope.value != "" {
				if answer.delegateScope.value != "routes" && answer.delegateRoutes != nil {
					return nil, fmt.Errorf("--delegate-route requires --delegate-scope routes")
				}
				part.Scope = apiclientgen.NewOptSecretRequestPartDecisionScope(apiclientgen.SecretRequestPartDecisionScope(answer.delegateScope.value))
			}
			part.Routes = answer.delegateRoutes
			if timed {
				part.TtlSeconds = apiclientgen.NewOptInt64(seconds)
			}
		} else if len(answer.delegateRoutes) > 0 || answer.delegateScope.value != "" {
			return nil, fmt.Errorf("--delegate-route and --delegate-scope require an approved delegate part; the delegate part is denied")
		}
		body.Delegate = apiclientgen.NewOptSecretRequestPartDecision(part)
	} else if len(answer.delegateRoutes) > 0 || answer.delegateScope.value != "" {
		return nil, fmt.Errorf("--delegate-route and --delegate-scope are the delegate part's; request %s does not ask to delegate", request.ID)
	}
	return body, nil
}

// writeApprovedParts says what an answer granted, part by part, in the
// sentence `secret bind` prints: the variable, where it may be sent, how
// long. A part denied says so.
//
// replacements are what each part was planned to replace before the answer
// was sent; an approved part's is said, so the access the discobox lost is
// read here rather than found later.
func (a *App) writeApprovedParts(cmd *cobra.Command, client *apiclientgen.Client, projectID string, request *apimodel.SecretRequest, replacements []partReplacement) error {
	fmt.Fprintf(cmd.OutOrStdout(), "Answered %s.\n", request.ID)
	for _, part := range requestParts(request) {
		bindingID := part.part.BindingId.Or("")
		if scope := part.part.ApprovedScope.Or(""); scope != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "The %s scope is %s (still limited by credential and parent scope).\n", part.label, scope)
		}
		if part.outcome() != apiclientgen.SecretRequestPartOutcomeApproved || bindingID == "" {
			fmt.Fprintf(cmd.OutOrStdout(), "The %s part is %s.\n", part.label, part.statusText(request.Status))
			continue
		}
		binding, err := a.writeApprovedBinding(cmd, client, projectID, request, bindingID)
		if err != nil {
			return err
		}
		if part.part.ApprovedAs.Or("") == apiclientgen.SecretRequestPartApprovedAsAdd {
			fmt.Fprintf(cmd.OutOrStdout(), "The %s part was granted as an addition: what the discobox held in %s stays as it was, and only this goes when it lapses.\n",
				part.label, terminalSafe(request.EnvName.Or("")))
		}
		// A role's name is never left standing over routes it no longer
		// describes (ADR 26-11-02-022 §3).
		if len(part.part.Policies) > 0 && !sameRouteSet(binding.Routes, part.part.Routes) {
			fmt.Fprintf(cmd.OutOrStdout(), "The %s part was approved with routes other than its roles resolved to; the roles no longer describe it.\n", part.label)
		}
		for _, r := range replacements {
			if r.label != part.label {
				continue
			}
			for _, line := range r.lines(true) {
				fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", line)
			}
		}
	}
	return nil
}

// writeApprovedBinding says what one approved part granted, in the sentence
// `secret bind` prints, and returns the binding.
func (a *App) writeApprovedBinding(cmd *cobra.Command, client *apiclientgen.Client, projectID string, request *apimodel.SecretRequest, bindingID string) (*apimodel.SecretBinding, error) {
	res, err := client.GetSecretBinding(cmd.Context(), apiclientgen.GetSecretBindingParams{ProjectId: projectID, BindingId: bindingID})
	if err != nil {
		return nil, err
	}
	binding, err := expectResponse[apimodel.SecretBinding](res)
	if err != nil {
		return nil, err
	}
	secret, err := a.getSecret(cmd.Context(), client, projectID, binding.SecretId)
	if err != nil {
		return nil, err
	}
	box := request.SandboxId.Or(binding.TargetId)
	if res, err := client.GetSandbox(cmd.Context(), apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: box}); err == nil {
		if sandbox, ok := res.(*apimodel.Sandbox); ok && sandbox.DisplayName != "" {
			box = sandbox.DisplayName
		}
	}
	return binding, writeBound(cmd.OutOrStdout(), binding, secret, bindingTarget{kind: apiclientgen.CreateSecretBindingBodyTargetKindSandbox, said: box}, time.Now())
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

// sameRouteSet reports whether two route lists hold the same routes, in any
// order: the server normalizes and may reorder what it stores, and a binding
// approved as asked must not read as edited.
func sameRouteSet(a, b []apimodel.SecretRoute) bool {
	set := func(routes []apimodel.SecretRoute) []string {
		out := make([]string, 0, len(routes))
		for _, route := range apiroute.Routes(routes) {
			out = append(out, secretroute.Format(route))
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	return slices.Equal(set(a), set(b))
}

// approves reports whether a part's decision approves it.
func approves(decision apiclientgen.OptSecretRequestPartDecision) bool {
	d, ok := decision.Get()
	return ok && d.Decision == apiclientgen.SecretRequestPartDecisionDecisionApprove
}
