package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/bindingplan"
	"github.com/discobox-ai/discobox/pkg/credentialpolicy"
)

// rebindView is what rebinding a discobox's variable from one secret to
// another does, read before a person sends it (ADR 26-11-19-927 §6): both
// secrets and their routes, what the replaced binding loses, what it keeps,
// and whether the variable keeps its sentinel. What becomes of each binding
// is bindingplan.Rebind, the rule the server applies.
type rebindView struct {
	envVar   string
	replaced apimodel.SecretBinding
	from, to apimodel.Secret
	toRoutes string
	plan     bindingplan.Plan
	byID     map[string]apimodel.SecretBinding
	handedOn int
	// sentinel is whether the variable keeps its sentinel: kept, minted
	// again, or unknown when the old secret records no format.
	sentinel string
}

// readRebind reads what rebinding envVar on box from the binding replaces to
// toSecretID does; toRoutes says the new binding's own routes.
func (a *App) readRebind(ctx context.Context, client *apiclientgen.Client, projectID, box, envVar, replaces, toSecretID, toRoutes string) (*rebindView, error) {
	res, err := client.GetSecretBinding(ctx, apiclientgen.GetSecretBindingParams{ProjectId: projectID, BindingId: replaces})
	if err != nil {
		return nil, err
	}
	replaced, err := expectResponse[apimodel.SecretBinding](res)
	if err != nil {
		return nil, fmt.Errorf("--replaces %s: %w", replaces, err)
	}
	if replaced.TargetKind != apiclientgen.SecretBindingTargetKindSandbox || replaced.TargetId != box || replaced.EnvVar.Or("") != envVar || !replaced.Use {
		return nil, fmt.Errorf("--replaces %s: that binding does not deliver %s in discobox %s", replaced.ID, terminalSafe(envVar), box)
	}
	if replaced.SecretId == toSecretID {
		return nil, fmt.Errorf("--replaces %s: that binding is of the secret being bound already; leave --replaces out", replaced.ID)
	}
	view := &rebindView{envVar: envVar, replaced: *replaced, toRoutes: toRoutes, byID: map[string]apimodel.SecretBinding{}}
	for _, secret := range []struct {
		id  string
		out *apimodel.Secret
	}{{replaced.SecretId, &view.from}, {toSecretID, &view.to}} {
		res, err := client.GetSecret(ctx, apiclientgen.GetSecretParams{ProjectId: projectID, SecretId: secret.id})
		if err != nil {
			return nil, err
		}
		got, err := expectResponse[apimodel.Secret](res)
		if err != nil {
			return nil, err
		}
		*secret.out = *got
	}
	// The variable's sentinel was minted from the old secret's format as it
	// is stored, unless that changed since; the rule is the server's
	// (bindingplan.KeepsSentinel). A secret storing none minted from a shape
	// read from its value, or the default, which no client can tell: neither
	// outcome is promised then, as the console promises neither.
	switch from := view.from.Format.Or(""); {
	case from == "":
		view.sentinel = sentinelUnknown
	case bindingplan.KeepsSentinel(from, view.to.Format.Or("")):
		view.sentinel = sentinelKept
	default:
		view.sentinel = sentinelMinted
	}
	listed, err := client.ListSecretBindings(ctx, apiclientgen.ListSecretBindingsParams{
		ProjectId: projectID, Secret: apiclientgen.NewOptString(replaced.SecretId),
		TargetKind: apiclientgen.NewOptListSecretBindingsTargetKind(apiclientgen.ListSecretBindingsTargetKindSandbox),
		Target:     apiclientgen.NewOptString(box),
	})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListSecretBindingsBody](listed)
	if err != nil {
		return nil, err
	}
	var standing []bindingplan.Binding
	for _, b := range body.GetSecretBindings() {
		if b.TargetKind != apiclientgen.SecretBindingTargetKindSandbox || b.TargetId != box || b.SecretId != replaced.SecretId {
			continue
		}
		view.byID[b.ID] = b
		standing = append(standing, bindingplan.Binding{ID: b.ID, Use: b.Use, Delegate: b.Delegate, Adds: b.Adds.Or(false), EnvName: b.EnvVar.Or(""), Created: b.CreatedAt,
			Parented: b.ParentId.Or("") != "", Raw: b.Raw.Or("") != ""})
	}
	view.byID[replaced.ID] = *replaced
	view.plan = bindingplan.Rebind(standing, replaced.ID, envVar)
	if replaced.Delegate {
		children, err := client.ListSecretBindings(ctx, apiclientgen.ListSecretBindingsParams{ProjectId: projectID, ParentId: apiclientgen.NewOptString(replaced.ID)})
		if err != nil {
			return nil, err
		}
		body, err := expectResponse[apimodel.ListSecretBindingsBody](children)
		if err != nil {
			return nil, err
		}
		view.handedOn = len(body.GetSecretBindings())
	}
	return view, nil
}

// approvedRoutesText says the routes an approved use part's binding is held
// to: the --route given, else what the scope makes of the asked routes.
func approvedRoutesText(asked []apimodel.SecretRoute, routeFlags []string, scope string) string {
	switch {
	case len(routeFlags) > 0:
		return strings.Join(routeFlags, "; ")
	case scope == "anywhere":
		return "no restriction of its own"
	case scope == "hosts":
		return "any method and path on the asked hosts"
	}
	return bindingRoutesText(asked)
}

// secretText names a secret by name and ID.
func secretText(secret apimodel.Secret) string {
	return terminalSafe(secret.Name) + " (" + secret.ID + ")"
}

// write says the rebind as rows of a table: what it would do, or, done,
// what it did.
func (v *rebindView) write(w io.Writer, done bool) {
	verb := func(would, did string) string {
		if done {
			return did
		}
		return would
	}
	fmt.Fprintf(w, "REBIND\t%s %s from %s to %s\n", terminalSafe(v.envVar), verb("moves", "moved"), secretText(v.from), secretText(v.to))
	fmt.Fprintf(w, "  FROM ROUTES\tsecret %s: %s; binding %s: %s, %s\n", terminalSafe(v.from.Name), routesText(v.from.Routes.Or(nil)),
		v.replaced.ID, bindingRoutesText(v.replaced.Routes), grantLifetime(v.replaced.ExpiresAt))
	fmt.Fprintf(w, "  TO ROUTES\tsecret %s: %s; binding: %s\n", terminalSafe(v.to.Name), routesText(v.to.Routes.Or(nil)), terminalSafe(v.toRoutes))
	label := "  LOSES"
	for _, step := range v.plan.Steps {
		b := v.byID[step.Binding.ID]
		what := fmt.Sprintf("%s (sent to %s, %s)", b.ID, bindingRoutesText(b.Routes), grantLifetime(b.ExpiresAt))
		var line string
		switch {
		case step.Binding.Adds:
			line = fmt.Sprintf("the addition %s %s", what, verb("goes", "went"))
		case step.Fate == bindingplan.LosesUse:
			line = fmt.Sprintf("%s %s its use in %s", what, verb("loses", "lost"), terminalSafe(v.envVar))
		default:
			line = fmt.Sprintf("%s %s", what, verb("goes", "went"))
		}
		fmt.Fprintf(w, "%s\t%s\n", label, line)
		label = ""
	}
	if raw := v.replaced.Raw.Or(""); raw != "" {
		fmt.Fprintf(w, "  RAW\t%s's value stays with processes already running, and is no longer watched for\n", terminalSafe(v.from.Name))
	}
	switch {
	case !v.replaced.Delegate:
		fmt.Fprintf(w, "  KEEPS\tnothing: %s only used %s\n", v.replaced.ID, terminalSafe(v.from.Name))
	case v.handedOn == 1:
		fmt.Fprintf(w, "  KEEPS\tits delegation of %s, and the 1 binding handed on under it\n", terminalSafe(v.from.Name))
	default:
		fmt.Fprintf(w, "  KEEPS\tits delegation of %s, and the %d bindings handed on under it\n", terminalSafe(v.from.Name), v.handedOn)
	}
	switch v.sentinel {
	case sentinelKept:
		fmt.Fprintf(w, "  SENTINEL\tkept; running processes switch to %s\n", terminalSafe(v.to.Name))
	case sentinelMinted:
		fmt.Fprintf(w, "  SENTINEL\ta new one, since %s's sentinels have another shape; processes holding %s now must restart\n", terminalSafe(v.to.Name), terminalSafe(v.envVar))
	default:
		fmt.Fprintf(w, "  SENTINEL\tkept only if both credentials share a shape, which %s does not record; otherwise processes holding %s must restart\n", terminalSafe(v.from.Name), terminalSafe(v.envVar))
	}
}

// What a rebind does to the variable's sentinel, as a preview can tell it.
const (
	sentinelKept    = "kept"
	sentinelMinted  = "minted"
	sentinelUnknown = "unknown"
)

// replacesFlagUsage is --replaces, on both doors.
const replacesFlagUsage = "Rebind the variable from the discobox's own binding of another secret that delivers it now, by ID: its use goes, a delegation it also holds stays"

// readCreateRebind is readRebind for admin binding create --replaces, from
// its flags: the discobox and secret as a person names them, and the routes
// as given.
func (a *App) readCreateRebind(ctx context.Context, client *apiclientgen.Client, projectID, target, envVar, replaces, secretRef string, routeFlags []string, policies credentialpolicy.References) (*rebindView, error) {
	box, err := a.resolveSandboxID(ctx, client, projectID, target)
	if err != nil {
		return nil, err
	}
	secretID, err := a.resolveSecretID(ctx, client, projectID, secretRef)
	if err != nil {
		return nil, err
	}
	toRoutes := approvedRoutesText(nil, routeFlags, "")
	if len(policies) > 0 {
		toRoutes = "the routes of the policy files given"
	}
	return a.readRebind(ctx, client, projectID, box, envVar, replaces, secretID, toRoutes)
}
