package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/discobox-ai/discobox/pkg/secretroute"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"
)

// `admin binding` is the binding API as raw records: IDs in, the records the
// API answered with out. The person's verbs over the same bindings are
// `secret bind`, `unbind` and `bound` (secret_bind.go), which name targets
// the way a person does and say what they did in a sentence.

func (a *App) newAdminBindingCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "binding",
		Aliases: []string{"bindings"},
		Short:   "Raw access to secret bindings, by ID",
		Long: `Raw access to secret bindings, by ID: list, read, create, change and delete
them, and read the trail of every change to them.

A binding makes a secret available to a target — the project, a harness config
or a discobox — to use in a variable, to give on, or both (ADR 26-10-27-880).
To bind a secret as a person does, use ` + "`discobox secret bind`" + `.`,
	}
	cmd.AddCommand(
		a.newAdminBindingListCommand(),
		a.newAdminBindingGetCommand(),
		a.newAdminBindingCreateCommand(),
		a.newAdminBindingUpdateCommand(),
		a.newAdminBindingDeleteCommand(),
		a.newAdminBindingEventsCommand(),
	)
	return cmd
}

// adminBindingClient is the project and client every `admin binding` command
// works against.
func (a *App) adminBindingClient() (string, *apiclientgen.Client, error) {
	projectID, err := a.projectIDValue()
	if err != nil {
		return "", nil, err
	}
	client, err := a.apiClient()
	if err != nil {
		return "", nil, err
	}
	return projectID, client, nil
}

func (a *App) newAdminBindingListCommand() *cobra.Command {
	var secret, targetKind, target, parent string
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the project's secret bindings",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, client, err := a.adminBindingClient()
			if err != nil {
				return err
			}
			params := apiclientgen.ListSecretBindingsParams{ProjectId: projectID}
			if secret != "" {
				params.Secret = apiclientgen.NewOptString(secret)
			}
			if targetKind != "" {
				kind := apiclientgen.ListSecretBindingsTargetKind(targetKind)
				if err := kind.Validate(); err != nil {
					return fmt.Errorf("--target-kind %q: want project, harnessConfig or sandbox", targetKind)
				}
				params.TargetKind = apiclientgen.NewOptListSecretBindingsTargetKind(kind)
			}
			if target != "" {
				params.Target = apiclientgen.NewOptString(target)
			}
			if parent != "" {
				params.ParentId = apiclientgen.NewOptString(parent)
			}
			res, err := client.ListSecretBindings(cmd.Context(), params)
			if err != nil {
				return err
			}
			body, err := expectResponse[apimodel.ListSecretBindingsBody](res)
			if err != nil {
				return err
			}
			bindings := body.GetSecretBindings()
			switch {
			case a.output == "json":
				return writeJSON(cmd.OutOrStdout(), body)
			case a.quiet:
				for _, b := range bindings {
					fmt.Fprintln(cmd.OutOrStdout(), b.ID)
				}
				return nil
			}
			return writeSecretBindings(cmd.OutOrStdout(), bindings)
		},
	}
	cmd.Flags().StringVar(&secret, "secret", "", "Only bindings of this secret, by name or ID")
	cmd.Flags().StringVar(&targetKind, "target-kind", "", "Only bindings on this kind of target: project, harnessConfig or sandbox")
	cmd.Flags().StringVar(&target, "target", "", "Only bindings on this harness config or discobox, by ID")
	cmd.Flags().StringVar(&parent, "parent", "", "Only bindings handed on under this delegate binding")
	a.addQuietFlag(cmd)
	return cmd
}

func writeSecretBindings(w io.Writer, bindings []apimodel.SecretBinding) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSECRET\tTARGET\tUSE\tDELEGATE\tVARIABLE\tROUTES\tPARENT\tEXPIRES")
	for _, b := range bindings {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%t\t%t\t%s\t%s\t%s\t%s\n",
			b.ID,
			terminalSafe(b.SecretName.Or(b.SecretId)),
			secretBindingTarget(b),
			b.Use,
			b.Delegate,
			orDash(terminalSafe(b.EnvVar.Or(""))),
			bindingRoutesText(b.Routes),
			orDash(b.ParentId.Or("")),
			bindingExpiresText(b.ExpiresAt),
		)
	}
	return tw.Flush()
}

// secretBindingTarget is a binding's target as a raw record names it: its
// kind, and the ID for any but the project.
func secretBindingTarget(b apimodel.SecretBinding) string {
	if b.TargetKind == apiclientgen.SecretBindingTargetKindProject || b.TargetId == "" {
		return string(b.TargetKind)
	}
	return string(b.TargetKind) + ":" + b.TargetId
}

// bindingRoutesText is a binding's own routes, or that it has none and goes
// where its secret's go.
func bindingRoutesText(routes []apimodel.SecretRoute) string {
	if len(routes) == 0 {
		return "(the secret's)"
	}
	return routesText(routes)
}

func bindingExpiresText(at apiclientgen.OptDateTime) string {
	if expires, ok := at.Get(); ok && !expires.IsZero() {
		return formatTime(expires)
	}
	return "never"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (a *App) newAdminBindingGetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "get BINDING_ID",
		Short: "Show a secret binding",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, client, err := a.adminBindingClient()
			if err != nil {
				return err
			}
			res, err := client.GetSecretBinding(cmd.Context(), apiclientgen.GetSecretBindingParams{ProjectId: projectID, BindingId: args[0]})
			if err != nil {
				return err
			}
			binding, err := expectResponse[apimodel.SecretBinding](res)
			if err != nil {
				return err
			}
			return a.writeSecretBinding(cmd, binding)
		},
	}
}

// writeSecretBinding prints one binding record: as JSON with -o json, as
// field and value otherwise.
func (a *App) writeSecretBinding(cmd *cobra.Command, b *apimodel.SecretBinding) error {
	if a.output == "json" {
		return writeJSON(cmd.OutOrStdout(), b)
	}
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	for _, row := range [][2]string{
		{"ID", b.ID},
		{"SECRET", terminalSafe(b.SecretName.Or(b.SecretId)) + " (" + b.SecretId + ")"},
		{"TARGET", secretBindingTarget(*b)},
		{"USE", fmt.Sprint(b.Use)},
		{"DELEGATE", fmt.Sprint(b.Delegate)},
		{"VARIABLE", orDash(terminalSafe(b.EnvVar.Or("")))},
		{"RAW", orDash(string(b.Raw.Or("")))},
		{"ROUTES", bindingRoutesText(b.Routes)},
		{"PARENT", orDash(b.ParentId.Or(""))},
		{"REQUEST", orDash(b.RequestId.Or(""))},
		{"CREATED BY", orDash(terminalSafe(b.CreatedBy.Or("")))},
		{"CREATED", formatTime(b.CreatedAt)},
		{"EXPIRES", bindingExpiresText(b.ExpiresAt)},
	} {
		fmt.Fprintf(tw, "%s\t%s\n", row[0], row[1])
	}
	return tw.Flush()
}

func (a *App) newAdminBindingCreateCommand() *cobra.Command {
	var secret, targetKind, target, envVar, raw string
	var use, delegate bool
	var routes []string
	ttl := lifetime.NewFlag()
	cmd := &cobra.Command{
		Use:   "create --secret SECRET --target-kind KIND [--target ID] [--as VAR] [--raw env|file] [--delegate]",
		Short: "Create a secret binding",
		Long: `Create a secret binding, as the API takes one: a secret by name or ID, a
target kind (project, harnessConfig or sandbox) and, for any but the project, its
ID. --as names the variable a use binding is delivered in, and --raw hands over
the value itself there (env) or a file holding it (file) rather than a
sentinel; --delegate makes it a binding to give on, and --use=false one only
given on. --route narrows the
secret's routes for this binding; left out, it goes where the secret's go.

` + routeHelp,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			kind := apiclientgen.CreateSecretBindingBodyTargetKind(targetKind)
			if err := kind.Validate(); err != nil {
				return fmt.Errorf("--target-kind %q: want project, harnessConfig or sandbox", targetKind)
			}
			parsed, err := routeFlagValues("--route", routes)
			if err != nil {
				return err
			}
			projectID, client, err := a.adminBindingClient()
			if err != nil {
				return err
			}
			body := &apimodel.CreateSecretBindingBody{Secret: secret, TargetKind: kind, Routes: parsed}
			if target != "" {
				body.SetTarget(apiclientgen.NewOptString(target))
			}
			if cmd.Flags().Changed("use") {
				body.SetUse(apiclientgen.NewOptBool(use))
			}
			if delegate {
				body.SetDelegate(apiclientgen.NewOptBool(true))
			}
			if envVar != "" {
				body.SetEnvVar(apiclientgen.NewOptString(envVar))
			}
			if raw != "" {
				value := apiclientgen.CreateSecretBindingBodyRaw(raw)
				if err := value.Validate(); err != nil {
					return fmt.Errorf("--raw %q: want env or file", raw)
				}
				body.SetRaw(apiclientgen.NewOptCreateSecretBindingBodyRaw(value))
			}
			if seconds, given := ttl.Seconds(); given {
				body.SetTtlSeconds(apiclientgen.NewOptInt64(seconds))
			}
			res, err := client.CreateSecretBinding(cmd.Context(), body, apiclientgen.CreateSecretBindingParams{ProjectId: projectID})
			if err != nil {
				return err
			}
			binding, err := expectResponse[apimodel.SecretBinding](res)
			if err != nil {
				return err
			}
			return a.writeSecretBinding(cmd, binding)
		},
	}
	cmd.Flags().StringVar(&secret, "secret", "", "The secret, by name or ID")
	cmd.Flags().StringVar(&targetKind, "target-kind", "", "What it is bound to: project, harnessConfig or sandbox")
	cmd.Flags().StringVar(&target, "target", "", "The harness config or discobox, by name or ID; omitted for the project")
	cmd.Flags().StringVar(&envVar, "as", "", "The variable a use binding is delivered in")
	cmd.Flags().StringVar(&raw, "raw", "", "Hand over the value itself: env, or file for a file whose path is in the variable")
	cmd.Flags().BoolVar(&use, "use", true, "Deliver it to use (default true unless --delegate)")
	cmd.Flags().BoolVar(&delegate, "delegate", false, "Let the target give it to the discoboxes it creates")
	cmd.Flags().StringArrayVar(&routes, "route", nil, "A route narrowing the secret's, as "+secretroute.Syntax+"; one per flag, repeat for more")
	cmd.Flags().Var(ttl, "for", "How long it lasts (e.g. 1h, 2w, forever); the secret's limit when omitted")
	_ = cmd.MarkFlagRequired("secret")
	_ = cmd.MarkFlagRequired("target-kind")
	return cmd
}

func (a *App) newAdminBindingUpdateCommand() *cobra.Command {
	var envVar, raw string
	var routes []string
	var clearRoutes, use, delegate bool
	ttl := lifetime.NewFlag()
	cmd := &cobra.Command{
		Use:   "update BINDING_ID [--route ROUTE]... [--clear-route] [--for LIFETIME] [--as VAR] [--raw env|file|none] [--use=BOOL] [--delegate=BOOL]",
		Short: "Change a secret binding's routes, lifetime, variable, delivery or purpose",
		Long: `Change a secret binding by ID. --route replaces its routes, and --clear-route
removes them, so it goes where its secret's go.

` + routeHelp,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if clearRoutes && len(routes) > 0 {
				return fmt.Errorf("--clear-route and --route say opposite things; give one")
			}
			parsed, err := routeFlagValues("--route", routes)
			if err != nil {
				return err
			}
			body := &apimodel.UpdateSecretBindingBody{}
			changed := false
			if len(parsed) > 0 || clearRoutes {
				body.Routes = parsed
				if clearRoutes {
					body.Routes = []apimodel.SecretRouteInput{}
				}
				changed = true
			}
			if seconds, given := ttl.Seconds(); given {
				body.SetTtlSeconds(apiclientgen.NewOptInt64(seconds))
				changed = true
			}
			if cmd.Flags().Changed("as") {
				body.SetEnvVar(apiclientgen.NewOptString(envVar))
				changed = true
			}
			if cmd.Flags().Changed("use") {
				body.SetUse(apiclientgen.NewOptBool(use))
				changed = true
			}
			if cmd.Flags().Changed("delegate") {
				body.SetDelegate(apiclientgen.NewOptBool(delegate))
				changed = true
			}
			if cmd.Flags().Changed("raw") {
				value := apiclientgen.UpdateSecretBindingBodyRaw(raw)
				if err := value.Validate(); err != nil {
					return fmt.Errorf("--raw %q: want env, file or none", raw)
				}
				body.SetRaw(apiclientgen.NewOptUpdateSecretBindingBodyRaw(value))
				changed = true
			}
			if !changed {
				return fmt.Errorf("nothing to change: give --route, --clear-route, --for, --as, --raw, --use or --delegate")
			}
			projectID, client, err := a.adminBindingClient()
			if err != nil {
				return err
			}
			res, err := client.UpdateSecretBinding(cmd.Context(), body, apiclientgen.UpdateSecretBindingParams{ProjectId: projectID, BindingId: args[0]})
			if err != nil {
				return err
			}
			binding, err := expectResponse[apimodel.SecretBinding](res)
			if err != nil {
				return err
			}
			return a.writeSecretBinding(cmd, binding)
		},
	}
	cmd.Flags().StringArrayVar(&routes, "route", nil, "Replace its routes with these, each "+secretroute.Syntax+"; one per flag, repeat for more")
	cmd.Flags().BoolVar(&clearRoutes, "clear-route", false, "Remove its routes, so it goes where its secret's go")
	cmd.Flags().Var(ttl, "for", "How long it lasts from now (e.g. 1h, 2w, forever)")
	cmd.Flags().StringVar(&envVar, "as", "", "The variable it is delivered in")
	cmd.Flags().StringVar(&raw, "raw", "", "Hand over the value itself (env, or file), or a sentinel again (none)")
	cmd.Flags().BoolVar(&use, "use", false, "Whether it is delivered to use; --use=false stops that, keeping the delegation")
	cmd.Flags().BoolVar(&delegate, "delegate", false, "Whether it may be given on; --delegate=false stops that, keeping the variable")
	return cmd
}

func (a *App) newAdminBindingDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "rm BINDING_ID...",
		Aliases: []string{"delete", "remove"},
		Short:   "Delete secret bindings, and every binding handed on under them",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectID, client, err := a.adminBindingClient()
			if err != nil {
				return err
			}
			var deleted []string
			for _, id := range args {
				res, err := client.DeleteSecretBinding(cmd.Context(), apiclientgen.DeleteSecretBindingParams{ProjectId: projectID, BindingId: id})
				if err != nil {
					return err
				}
				body, err := expectResponse[apimodel.DeleteSecretBindingResponse](res)
				if err != nil {
					return err
				}
				deleted = append(deleted, body.Deleted...)
			}
			if a.output == "json" {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"deleted": deleted})
			}
			for _, id := range deleted {
				fmt.Fprintln(cmd.OutOrStdout(), id)
			}
			return nil
		},
	}
}

func (a *App) newAdminBindingEventsCommand() *cobra.Command {
	var bindingID, secret string
	var limit int64
	since := newSinceFlag()
	cmd := &cobra.Command{
		Use:   "events [--binding BINDING_ID] [--secret SECRET] [--since TIME] [--limit N]",
		Short: "Show the trail of changes to the project's secret bindings",
		Long: `Show the trail of changes to the project's secret bindings: each creation,
change, deletion and expiry, who or what made it, and why — the request it
answered, the binding it was handed on under, or the upgrade that made it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			projectID, client, err := a.adminBindingClient()
			if err != nil {
				return err
			}
			params := apiclientgen.ListSecretBindingEventsParams{ProjectId: projectID}
			if bindingID != "" {
				params.BindingId = apiclientgen.NewOptString(bindingID)
			}
			if secret != "" {
				params.Secret = apiclientgen.NewOptString(secret)
			}
			if from := since.from(time.Now()); !from.IsZero() {
				params.Since = apiclientgen.NewOptDateTime(from)
			}
			if limit > 0 {
				params.Limit = apiclientgen.NewOptInt64(limit)
			}
			res, err := client.ListSecretBindingEvents(cmd.Context(), params)
			if err != nil {
				return err
			}
			body, err := expectResponse[apimodel.ListSecretBindingEventsBody](res)
			if err != nil {
				return err
			}
			if a.output == "json" {
				return writeTerminalSafeJSON(cmd.OutOrStdout(), body)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "AT\tEVENT\tBINDING\tSECRET\tTARGET\tVARIABLE\tACTOR\tCAUSE")
			for _, e := range body.GetSecretBindingEvents() {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					formatTime(e.At),
					e.Event,
					e.BindingId,
					terminalSafe(e.Binding.SecretName.Or(e.Binding.SecretId)),
					secretBindingTarget(e.Binding),
					orDash(terminalSafe(e.Binding.EnvVar.Or(""))),
					terminalSafe(e.Actor),
					orDash(terminalSafe(strings.TrimSpace(e.Cause.Or("")))),
				)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&bindingID, "binding", "", "Only this binding's events")
	cmd.Flags().StringVar(&secret, "secret", "", "Only events of bindings of this secret, by name or ID")
	cmd.Flags().Var(since, "since", "Only events from this long ago (e.g. 1h) or since this RFC 3339 time")
	cmd.Flags().Int64Var(&limit, "limit", 0, "Maximum number of events to return")
	return cmd
}
