package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/apiroute"
	"github.com/discobox-ai/discobox/cli/internal/lifetime"
	"github.com/discobox-ai/discobox/pkg/agentcreds"
	"github.com/discobox-ai/discobox/pkg/bindingplan"
	"github.com/discobox-ai/discobox/pkg/secretroute"
)

// useGrants is what applies in the variable a request's use part asks for,
// grant by grant, beside the routes it asks (ADR 26-11-03-329 §5): what a
// person reads to choose between granting it as an addition and replacing
// what the discobox holds.
type useGrants struct {
	envVar string
	// held are the grants that apply in the variable now: the binding that
	// delivers it, then each addition stacking on it.
	held  []apimodel.BoundSecret
	asked []apimodel.SecretRoute
}

// requestUseGrants reads what applies in the variable of a request's use
// part on the discobox that asked; nil for a request with no use part.
func (a *App) requestUseGrants(ctx context.Context, client *apiclientgen.Client, projectID string, request *apimodel.SecretRequest) (*useGrants, error) {
	use, ok := request.Use.Get()
	box, envVar := request.SandboxId.Or(""), request.EnvName.Or("")
	if !ok || box == "" || envVar == "" {
		return nil, nil
	}
	res, err := client.ListBoundSecrets(ctx, apiclientgen.ListBoundSecretsParams{
		ProjectId:  projectID,
		TargetKind: apiclientgen.ListBoundSecretsTargetKindSandbox,
		Target:     apiclientgen.NewOptString(box),
	})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListBoundSecretsBody](res)
	if err != nil {
		return nil, err
	}
	grants := &useGrants{envVar: envVar, asked: use.Routes}
	for _, bound := range body.BoundSecrets {
		if bound.Use && bound.EnvVar.Or("") == envVar && bound.NotApplied.Or("") == "" {
			grants.held = append(grants.held, bound)
		}
	}
	return grants, nil
}

// grantRoutes are the routes one grant was given: its own list when it has
// one — the one after the secret's — else the secret's.
func grantRoutes(bound apimodel.BoundSecret) []apimodel.SecretRoute {
	switch len(bound.Routes) {
	case 0:
		return nil
	case 1:
		return bound.Routes[0]
	}
	return bound.Routes[1]
}

// newRoutes are the asked routes no held grant was given, by
// bindingplan.NewRoutes over each grant's own routes (grantRoutes), as
// secretroute.Format writes them.
func (g *useGrants) newRoutes() []string {
	var held [][]string
	for _, bound := range g.held {
		held = append(held, formatRouteTexts(grantRoutes(bound)))
	}
	return bindingplan.NewRoutes(formatRouteTexts(g.asked), held)
}

// formatRouteTexts are routes as secretroute.Format writes them.
func formatRouteTexts(routes []apimodel.SecretRoute) []string {
	var out []string
	for _, route := range apiroute.Routes(routes) {
		out = append(out, secretroute.Format(route))
	}
	return out
}

// planHeld is the grants held as bindingplan reads them.
func (g *useGrants) planHeld() []bindingplan.Held {
	var out []bindingplan.Held
	for _, bound := range g.held {
		held := bindingplan.Held{ID: bound.BindingId, Adds: bound.Adds.Or(false), Own: bound.From == apiclientgen.BoundSecretFromSandbox}
		if at, ok := bound.ExpiresAt.Get(); ok {
			held.Expires = at
		}
		out = append(out, held)
	}
	return out
}

// approvalLifetime is how long an approval of the request would last when
// the approver names none, as the server starts from: what was asked, else
// agentcreds.DefaultGrantTTL. The secret's limit may shorten it.
func approvalLifetime(request *apimodel.SecretRequest) time.Duration {
	if asked := agentcreds.AskedGrantTTL(request.TtlSeconds.Or(0)); asked > 0 {
		return asked
	}
	return agentcreds.DefaultGrantTTL
}

// askedAs is how the request asks its use part to be granted.
func askedAs(request *apimodel.SecretRequest) bindingplan.As {
	if use, ok := request.Use.Get(); ok && use.Add.Or(false) {
		return bindingplan.Add
	}
	return bindingplan.Replace
}

// writeUseGrants writes what applies in the variable and what each way of
// granting the use part would do, as rows of the request's table: the
// grants held now with their lifetimes, the asked routes they do not name,
// and, for both ways, what lasts how long. A replacement that would put
// routes held for longer, or for good, on the approval's clock says so.
func writeUseGrants(w io.Writer, g *useGrants, as bindingplan.As, ttl time.Duration, now time.Time) {
	if g == nil {
		return
	}
	label := "USE HOLDS"
	if len(g.held) == 0 {
		fmt.Fprintf(w, "%s\tnothing in %s\n", label, terminalSafe(g.envVar))
	}
	for _, bound := range g.held {
		what := "binding"
		if bound.Adds.Or(false) {
			what = "addition"
		}
		fmt.Fprintf(w, "%s\t%s %s → %s, %s\n", label, what, bound.BindingId, routesText(grantRoutes(bound)), grantLifetime(bound.ExpiresAt))
		label = ""
	}
	added := g.newRoutes()
	if len(added) == 0 {
		fmt.Fprintf(w, "USE NEW ROUTES\tnone: every route asked is named, as written, by a grant above\n")
	} else {
		fmt.Fprintf(w, "USE NEW ROUTES\t%s (compared as written, not as what they allow)\n", terminalSafe(strings.Join(added, "; ")))
	}
	until := lifetime.Label(ttl)
	asLabel := func(way bindingplan.As) string {
		if way == as {
			return "USE AS " + strings.ToUpper(string(way)) + " (asked)"
		}
		return "USE AS " + strings.ToUpper(string(way))
	}
	fmt.Fprintf(w, "%s\tan addition: these routes for %s; what it holds in %s stays as it is\n", asLabel(bindingplan.Add), until, terminalSafe(g.envVar))
	fmt.Fprintf(w, "%s\tits whole use of the credential: these routes for %s, in place of what it holds\n", asLabel(bindingplan.Replace), until)
	// What a replacement does to each grant held is bindingplan's rule, the
	// one the server applies (bindingplan.Replacing).
	byID := map[string]apimodel.BoundSecret{}
	for _, bound := range g.held {
		byID[bound.BindingId] = bound
	}
	for _, step := range bindingplan.Replacing(g.planHeld(), now.Add(ttl)) {
		bound := byID[step.Held.ID]
		var fate string
		switch step.Fate {
		case bindingplan.Goes:
			fate = "goes"
		case bindingplan.Shadowed:
			fate = fmt.Sprintf("from its %s, is shadowed by the approval's binding for %s, and its routes do not apply while it is", bound.From, until)
		case bindingplan.Shortened:
			fate = fmt.Sprintf("held %s, takes the approval's routes and lasts only %s", grantLifetime(bound.ExpiresAt), until)
		}
		fmt.Fprintf(w, "  IF REPLACED\t%s %s\n", step.Held.ID, fate)
	}
}

// grantLifetime says how long a grant lasts, forever included.
func grantLifetime(expires apiclientgen.OptDateTime) string {
	if at, ok := expires.Get(); ok {
		return "until " + formatTime(at)
	}
	return "with no expiry"
}
