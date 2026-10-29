package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/discobox-ai/discobox/pkg/secretroute"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/cli/internal/secretenv"
	idpkg "github.com/discobox-ai/x/id"
)

// A binding is a secret made available to a target — every discobox of the
// project, every discobox on a harness config, or one discobox — and existing
// is the whole of the permission (ADR 26-10-27-880 §1). `secret bind`,
// `unbind` and `bound` are the person's verbs over them, and say what they did
// in a sentence; `admin binding` is the same API as raw records.

// bindingTarget is what a binding is for, as the person named it: the API's
// kind and reference, and the words a sentence calls it by.
type bindingTarget struct {
	kind apiclientgen.CreateSecretBindingBodyTargetKind
	// ref is the harness config or discobox ID; empty for the project.
	ref string
	// said is how a sentence names it: "mybox", "harness claude-code",
	// "every discobox of the project".
	said string
}

// bindingTargetFlags are the ways a person names a target: a DISCOBOX
// argument, --harness, or --project-wide. Exactly one. It is not --project-wide,
// which is the root's flag for which project a command works in.
type bindingTargetFlags struct {
	harness string
	project bool
}

func (f *bindingTargetFlags) add(cmd *cobra.Command, verb string) {
	cmd.Flags().StringVar(&f.harness, "harness", "", "The harness config to "+verb+", by slug, name or ID: every discobox on it")
	cmd.Flags().BoolVar(&f.project, "project-wide", false, "The project to "+verb+": every discobox of it")
}

// resolve reads the target from the DISCOBOX argument and the flags. none says
// what to do when neither names one; nil refuses.
func (a *App) resolveBindingTarget(cmd *cobra.Command, discobox string, flags bindingTargetFlags, none *bindingTarget) (*App, string, *apiclientgen.Client, bindingTarget, error) {
	named := 0
	for _, given := range []bool{strings.TrimSpace(discobox) != "", strings.TrimSpace(flags.harness) != "", flags.project} {
		if given {
			named++
		}
	}
	if named > 1 {
		return nil, "", nil, bindingTarget{}, errors.New("name one of a discobox, --harness or --project-wide")
	}
	switch {
	case strings.TrimSpace(discobox) != "":
		app, projectID, sandboxID, client, err := a.selectSandbox(cmd, discobox)
		if err != nil {
			return nil, "", nil, bindingTarget{}, err
		}
		return app, projectID, client, bindingTarget{kind: apiclientgen.CreateSecretBindingBodyTargetKindSandbox, ref: sandboxID, said: discobox}, nil
	case strings.TrimSpace(flags.harness) != "":
		projectID, harnessID, client, err := a.harnessRequest(cmd.Context(), flags.harness)
		if err != nil {
			return nil, "", nil, bindingTarget{}, err
		}
		return a, projectID, client, bindingTarget{kind: apiclientgen.CreateSecretBindingBodyTargetKindHarnessConfig, ref: harnessID, said: "harness " + flags.harness}, nil
	case flags.project, none != nil:
		projectID, err := a.projectIDValue()
		if err != nil {
			return nil, "", nil, bindingTarget{}, err
		}
		client, err := a.apiClient()
		if err != nil {
			return nil, "", nil, bindingTarget{}, err
		}
		target := bindingTarget{kind: apiclientgen.CreateSecretBindingBodyTargetKindProject, said: "every discobox of the project"}
		if !flags.project && none != nil {
			target = *none
		}
		return a, projectID, client, target, nil
	default:
		return nil, "", nil, bindingTarget{}, errors.New("say what to bind it to: a discobox, --harness HARNESS, or --project-wide")
	}
}

func (a *App) newSecretBindCommand() *cobra.Command {
	var as string
	var raw rawFlag
	var routes []string
	var delegate bool
	var target bindingTargetFlags
	lasts := lifetime.NewFlag()
	cmd := &cobra.Command{
		Use:   "bind SECRET [DISCOBOX] [--harness HARNESS | --project-wide] [--as VAR] [--raw env|file]",
		Short: "Give a secret to a discobox, a harness config, or the whole project",
		Long: `Give a secret to a discobox, every discobox on a harness config (--harness),
or every discobox of the project (--project-wide).

--as names the environment variable it is delivered in: a sentinel that the
discobox's pool exchanges for the value on the way out. Left out, it is the
secret's name upper-snaked: github is GITHUB, npm-token is NPM_TOKEN. --route narrows where it
may be sent for this binding; left out, it goes where the secret's routes say. --for is how long the binding lasts, within
the secret's limit.

--raw hands over the value itself rather than a sentinel, for a program that
signs with it or sends it where an exchange cannot reach: env puts it in the
variable, file puts there the path of a file under /run/discobox/raw-secrets
holding it. The secret must allow it (secret update --allow-raw). The value is
held in memory, never on a disk, and the pool refuses a request carrying it
off its routes; but a discobox holding a value can send it anywhere, so that is
a guard against mistakes, not a boundary.

--delegate lets the discobox give the secret, to use, to the discoboxes it
creates: by answering their requests, or when it creates them. With --as as
well, it uses the secret too; without, it is delivered nothing itself.

For one discobox and one variable, the narrowest binding wins: a discobox's own
over its harness config's, and that over the project's.

` + routeHelp,
		Example: `  discobox secret bind github mybox --as GH_TOKEN
  discobox secret bind github --harness claude-code --as GH_TOKEN --for forever
  discobox secret bind github --project-wide --as GH_TOKEN --route 'api.github.com/repos/acme/**'
  discobox secret bind github lead --delegate --for 8h`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			box := ""
			if len(args) > 1 {
				box = args[1]
			}
			app, projectID, client, t, err := a.resolveBindingTarget(cmd, box, target, nil)
			if err != nil {
				return err
			}
			as = strings.TrimSpace(as)
			if as == "" && !delegate {
				if as, err = app.defaultBindingVariable(cmd.Context(), client, projectID, args[0]); err != nil {
					return err
				}
			}
			parsed, err := routeFlagValues("--route", routes)
			if err != nil {
				return err
			}
			body := &apimodel.CreateSecretBindingBody{Secret: args[0], TargetKind: t.kind, Routes: parsed}
			if t.ref != "" {
				body.SetTarget(apiclientgen.NewOptString(t.ref))
			}
			body.SetUse(apiclientgen.NewOptBool(as != ""))
			if as != "" {
				body.SetEnvVar(apiclientgen.NewOptString(as))
			}
			if delegate {
				body.SetDelegate(apiclientgen.NewOptBool(true))
			}
			if raw.set {
				if as == "" {
					return fmt.Errorf("--raw hands the value over to use, so it needs a variable (--as)")
				}
				body.SetRaw(apiclientgen.NewOptCreateSecretBindingBodyRaw(apiclientgen.CreateSecretBindingBodyRaw(raw.value)))
			}
			if seconds, given := lasts.Seconds(); given {
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
			if app.output == "json" {
				return writeJSON(cmd.OutOrStdout(), binding)
			}
			secret, err := app.getSecret(cmd.Context(), client, projectID, binding.SecretId)
			if err != nil {
				return err
			}
			return writeBound(cmd.OutOrStdout(), binding, secret, t, time.Now())
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "The environment variable to deliver it in (default the secret's name upper-snaked)")
	cmd.Flags().StringArrayVar(&routes, "route", nil, "Where it may be sent for this binding, narrowing the secret's routes, as "+secretroute.Syntax+"; one per flag, repeat for more")
	cmd.Flags().Var(lasts, "for", bindingLifetimeFlagUsage)
	cmd.Flags().Var(&raw, "raw", "Hand over the value itself: env (in the variable) or file (a file whose path is in it); left out, a sentinel")
	cmd.Flags().BoolVar(&delegate, "delegate", false, "Let the discobox give the secret to the discoboxes it creates; a person's to set")
	target.add(cmd, "bind it to")
	return cmd
}

// writeBound says what a bind did, in a sentence: what the target now holds,
// where it may be sent, and for how long — and, for a discobox, how a shell
// already running there takes it up.
func writeBound(w io.Writer, binding *apimodel.SecretBinding, secret *apimodel.Secret, t bindingTarget, now time.Time) error {
	name := binding.SecretName.Or(secret.Name)
	sentTo := routesText(secret.Routes.Or(nil))
	if len(binding.Routes) > 0 {
		sentTo = routesText(binding.Routes) + " (within the secret's " + routesText(secret.Routes.Or(nil)) + ")"
	}
	lasts := bindingLastsText(binding.ExpiresAt, now)
	// A discobox names itself, and an approval names the one that asked:
	// every name here is escaped once, before any sentence uses it.
	name = terminalSafe(name)
	holder := terminalSafe(delegateHolder(t))
	env := terminalSafe(binding.EnvVar.Or(""))
	raw, isRaw := binding.Raw.Get()
	switch {
	case binding.Use && isRaw && raw == apiclientgen.SecretBindingRawFile:
		fmt.Fprintf(w, "%s's value itself is now in a file in %s, its path in %s, refused off %s; it %s.\n", name, holder, env, sentTo, lasts)
	case binding.Use && isRaw:
		fmt.Fprintf(w, "%s's value itself is now %s in %s, refused off %s; it %s.\n", name, env, holder, sentTo, lasts)
	case binding.Use:
		fmt.Fprintf(w, "%s is now %s in %s, sent to %s; it %s.\n", name, env, holder, sentTo, lasts)
	}
	if binding.Use && isRaw {
		fmt.Fprintln(w, "The value leaves the pool: it is held in memory and refused off its routes, but a discobox holding it can send it anywhere.")
	}
	if binding.Delegate {
		fmt.Fprintf(w, "%s may give %s to the discoboxes it creates, sent to %s; it %s.\n", holder, name, sentTo, lasts)
	}
	switch {
	case binding.Use && isRaw && raw == apiclientgen.SecretBindingRawEnv && t.kind == apiclientgen.CreateSecretBindingBodyTargetKindSandbox:
		fmt.Fprintf(w, "Every process started in %s from now on has it; one already running does not.\n", holder)
	case binding.Use && t.kind == apiclientgen.CreateSecretBindingBodyTargetKindSandbox:
		fmt.Fprintf(w, "A shell already running in %s takes it up with: eval \"$(discobox-access env)\"\n", holder)
	}
	return nil
}

// delegateHolder is how a sentence names what a binding is on.
func delegateHolder(t bindingTarget) string {
	switch t.kind {
	case apiclientgen.CreateSecretBindingBodyTargetKindHarnessConfig:
		return "every discobox on " + strings.TrimPrefix(t.said, "harness ")
	case apiclientgen.CreateSecretBindingBodyTargetKindProject:
		return "every discobox of the project"
	}
	return t.said
}

// bindingLastsText is how long a binding lasts, as a sentence ends: "lasts
// 1 hour" or "never lapses".
func bindingLastsText(expiresAt apiclientgen.OptDateTime, now time.Time) string {
	at, ok := expiresAt.Get()
	if !ok || at.IsZero() {
		return "never lapses"
	}
	left := at.Sub(now).Round(time.Minute)
	if left <= 0 {
		return "has lapsed"
	}
	return "lasts " + lifetime.Label(left) + ", until " + formatTime(at)
}

func (a *App) newSecretUnbindCommand() *cobra.Command {
	var as string
	var delegate bool
	var target bindingTargetFlags
	cmd := &cobra.Command{
		Use:   "unbind SECRET [DISCOBOX] [--harness HARNESS | --project-wide]",
		Short: "Take a secret back from a discobox, a harness config, or the project",
		Long: `Take a secret back from a discobox, a harness config (--harness) or the project
(--project-wide): the variables it is delivered in there, or only the one in
--as. With --delegate it takes back the right to give it on instead.

A binding that both delivers a variable and may be given on loses only the half
asked for: unbind keeps the delegation, unbind --delegate keeps the variable.
What a discobox handed on under a delegation goes when the delegation does,
and is named.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			box := ""
			if len(args) > 1 {
				box = args[1]
			}
			_, projectID, client, t, err := a.resolveBindingTarget(cmd, box, target, nil)
			if err != nil {
				return err
			}
			params := apiclientgen.ListSecretBindingsParams{ProjectId: projectID, Secret: apiclientgen.NewOptString(args[0])}
			params.TargetKind = apiclientgen.NewOptListSecretBindingsTargetKind(apiclientgen.ListSecretBindingsTargetKind(t.kind))
			if t.ref != "" {
				params.Target = apiclientgen.NewOptString(t.ref)
			}
			res, err := client.ListSecretBindings(cmd.Context(), params)
			if err != nil {
				return err
			}
			body, err := expectResponse[apimodel.ListSecretBindingsBody](res)
			if err != nil {
				return err
			}
			as = strings.TrimSpace(as)
			var chosen []apimodel.SecretBinding
			onlyGivenOn := false
			for _, binding := range body.GetSecretBindings() {
				switch {
				case as != "" && binding.EnvVar.Or("") != as:
				case delegate && !binding.Delegate:
				case !delegate && !binding.Use:
					onlyGivenOn = true
				default:
					chosen = append(chosen, binding)
				}
			}
			if len(chosen) == 0 {
				if onlyGivenOn {
					return fmt.Errorf("%s is only given on from %s, not delivered in a variable; take that back with --delegate", args[0], t.said)
				}
				return fmt.Errorf("%s is not bound to %s", args[0], t.said)
			}
			for _, binding := range chosen {
				line, err := takeBack(cmd.Context(), client, projectID, binding, delegate, args[0], t)
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), line)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "Only the binding delivered in this variable")
	cmd.Flags().BoolVar(&delegate, "delegate", false, "Only the delegation")
	target.add(cmd, "take it back from")
	return cmd
}

// takeBack takes one half of a binding back — its variable, or with delegate
// the right to give it on — and says so. A binding holding only that half is
// deleted, with what was handed on under it; one holding both keeps the other
// half (UpdateSecretBinding).
func takeBack(ctx context.Context, client *apiclientgen.Client, projectID string, binding apimodel.SecretBinding, delegate bool, secret string, t bindingTarget) (string, error) {
	env := terminalSafe(binding.EnvVar.Or(""))
	if binding.Use && binding.Delegate {
		body := &apimodel.UpdateSecretBindingBody{}
		line := fmt.Sprintf("%s (%s) taken back from %s; it may still be given on.", secret, env, t.said)
		if delegate {
			body.SetDelegate(apiclientgen.NewOptBool(false))
			line = fmt.Sprintf("%s may no longer be given on from %s; it stays in %s.", secret, t.said, env)
		} else {
			body.SetUse(apiclientgen.NewOptBool(false))
		}
		res, err := client.UpdateSecretBinding(ctx, body, apiclientgen.UpdateSecretBindingParams{ProjectId: projectID, BindingId: binding.ID})
		if err != nil {
			return "", err
		}
		if _, err := expectResponse[apimodel.SecretBinding](res); err != nil {
			return "", err
		}
		return line, nil
	}
	res, err := client.DeleteSecretBinding(ctx, apiclientgen.DeleteSecretBindingParams{ProjectId: projectID, BindingId: binding.ID})
	if err != nil {
		return "", err
	}
	deleted, err := expectResponse[apimodel.DeleteSecretBindingResponse](res)
	if err != nil {
		return "", err
	}
	what := secret + " (" + env + ")"
	if delegate {
		what = secret + " (to give on)"
	}
	line := fmt.Sprintf("%s taken back from %s", what, t.said)
	if handed := len(deleted.Deleted) - 1; handed > 0 {
		line += ", and from the " + plural(handed, "discobox", "discoboxes") + " it was handed on to"
	}
	return line + ".", nil
}

func (a *App) newSecretBoundCommand() *cobra.Command {
	var target bindingTargetFlags
	cmd := &cobra.Command{
		Use:   "bound [DISCOBOX] [--harness HARNESS | --project-wide]",
		Short: "Show what a discobox, a harness config or the project can use, and why",
		Long: `Show what a discobox, a harness config (--harness) or the project (--project-wide,
the default) can use: each variable and the secret in it, which binding it comes
from, where it may be sent, and for how long — the narrowest binding per
variable, with every route list a request carrying it is held to. Delegations
it may give on are listed too.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			box := ""
			if len(args) > 0 {
				box = args[0]
			}
			app, projectID, client, t, err := a.resolveBindingTarget(cmd, box, target, &bindingTarget{kind: apiclientgen.CreateSecretBindingBodyTargetKindProject, said: "the project"})
			if err != nil {
				return err
			}
			params := apiclientgen.ListBoundSecretsParams{ProjectId: projectID, TargetKind: apiclientgen.ListBoundSecretsTargetKind(t.kind)}
			if t.ref != "" {
				params.Target = apiclientgen.NewOptString(t.ref)
			}
			res, err := client.ListBoundSecrets(cmd.Context(), params)
			if err != nil {
				return err
			}
			body, err := expectResponse[apimodel.ListBoundSecretsBody](res)
			if err != nil {
				return err
			}
			bound := body.GetBoundSecrets()
			if app.output == "json" {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"boundSecrets": bound})
			}
			if len(bound) == 0 {
				_, err := fmt.Fprintf(cmd.OutOrStdout(), "Nothing is bound to %s.\n", t.said)
				return err
			}
			return writeBoundSecrets(cmd.OutOrStdout(), bound, app.boundFromNames(cmd.Context(), client, projectID, bound), time.Now())
		},
	}
	target.add(cmd, "show")
	return cmd
}

// writeBoundSecrets is the resolved view a person reads: variable, secret,
// where the binding comes from, where it may be sent, and how long it lasts.
func writeBoundSecrets(w io.Writer, bound []apimodel.BoundSecret, names map[string]string, now time.Time) error {
	slices.SortFunc(bound, func(x, y apimodel.BoundSecret) int {
		return strings.Compare(boundVariable(x), boundVariable(y))
	})
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "VARIABLE\tSECRET\tFROM\tSENT TO\tLASTS")
	for _, b := range bound {
		lasts := "forever"
		if at, ok := b.ExpiresAt.Get(); ok && !at.IsZero() {
			lasts = lifetime.Label(at.Sub(now).Round(time.Minute)) + " left"
		}
		// The variable can be a discobox's own word — a child's binding takes
		// it from the discobox that asked or delegated — so it is escaped as
		// the secret's name is (cli/REVIEW.md); routesText escapes the routes.
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", terminalSafe(boundVariable(b)), terminalSafe(b.SecretName.Or(b.SecretId)), terminalSafe(boundFrom(b, names)), routeListsText(b.Routes), lasts)
	}
	return tw.Flush()
}

func boundVariable(b apimodel.BoundSecret) string {
	if b.Use {
		if b.Delegate {
			return b.EnvVar.Or("") + " (+ delegate)"
		}
		return b.EnvVar.Or("")
	}
	return "(delegate)"
}

// boundFrom says where a binding comes from by the names a person knows: the
// harness's slug and the discobox's name, an ID only for one that could not be
// read.
func boundFrom(b apimodel.BoundSecret, names map[string]string) string {
	name := names[b.FromId]
	if name == "" {
		name = b.FromId
	}
	switch b.From {
	case apiclientgen.BoundSecretFromProject:
		return "project"
	case apiclientgen.BoundSecretFromHarnessConfig:
		return "harness " + name
	}
	return "discobox " + name
}

// boundFromNames reads the names of the harness configs and discoboxes the
// bindings come from. One that cannot be read is left out, and drawn by ID.
func (a *App) boundFromNames(ctx context.Context, client *apiclientgen.Client, projectID string, bound []apimodel.BoundSecret) map[string]string {
	names := map[string]string{}
	harnesses := false
	for _, b := range bound {
		switch b.From {
		case apiclientgen.BoundSecretFromHarnessConfig:
			harnesses = true
		case apiclientgen.BoundSecretFromSandbox:
			if _, seen := names[b.FromId]; seen {
				continue
			}
			names[b.FromId] = ""
			res, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: b.FromId})
			if err != nil {
				continue
			}
			if sandbox, ok := res.(*apimodel.Sandbox); ok {
				names[b.FromId] = sandbox.DisplayName
			}
		}
	}
	if harnesses {
		if configs, err := a.listHarnessConfigs(ctx, client, projectID); err == nil {
			for _, h := range configs {
				names[h.ID] = h.Slug
			}
		}
	}
	return names
}

// routeListsText says where a credential held to several route lists may go:
// the narrowest list first, each within the ones before it in the chain.
func routeListsText(lists [][]apimodel.SecretRoute) string {
	if len(lists) == 0 {
		return "nowhere"
	}
	parts := make([]string, 0, len(lists))
	for i := len(lists) - 1; i >= 0; i-- {
		parts = append(parts, routesText(lists[i]))
	}
	return strings.Join(parts, " within ")
}

// defaultBindingVariable is the variable a binding that names none is
// delivered in: the secret's name upper-snaked, read from the secret when it
// was given by ID.
func (a *App) defaultBindingVariable(ctx context.Context, client *apiclientgen.Client, projectID, secret string) (string, error) {
	name := strings.TrimSpace(secret)
	if idpkg.IsGenerated(name) {
		found, err := a.getSecret(ctx, client, projectID, name)
		if err != nil {
			return "", err
		}
		name = found.Name
	}
	if env := secretenv.Name(name); env != "" {
		return env, nil
	}
	return "", fmt.Errorf("%q makes no variable name; name one with --as", secret)
}

func (a *App) getSecret(ctx context.Context, client *apiclientgen.Client, projectID, secretID string) (*apimodel.Secret, error) {
	res, err := client.GetSecret(ctx, apiclientgen.GetSecretParams{ProjectId: projectID, SecretId: secretID})
	if err != nil {
		return nil, err
	}
	return expectResponse[apimodel.Secret](res)
}
