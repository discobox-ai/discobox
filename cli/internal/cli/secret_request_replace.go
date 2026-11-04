package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pkg/bindingplan"
)

// partReplacement is what approving one part of a request with a secret does
// to the bindings the asking discobox already holds of it (ADR 26-11-02-022
// §5): replacing them, or, for a use part granted as an addition, nothing
// (ADR 26-11-03-329 §4), as bindingplan decides it — the rule the server
// applies.
type partReplacement struct {
	label string
	plan  bindingplan.Plan
	held  map[string]apimodel.SecretBinding
}

// requestReplacements reads the asking discobox's own sandbox-level bindings
// of secretID and plans the answer against them: the use part as useAs
// grants it, empty when it is not approved, and the delegate part when
// delegateApproved, against what the use part leaves, as the server applies
// them (bindingplan.Answer).
func (a *App) requestReplacements(ctx context.Context, client *apiclientgen.Client, projectID string, request *apimodel.SecretRequest, secretID string, useAs bindingplan.As, delegateApproved bool) ([]partReplacement, error) {
	box := request.SandboxId.Or("")
	if box == "" {
		return nil, nil
	}
	res, err := client.ListSecretBindings(ctx, apiclientgen.ListSecretBindingsParams{
		ProjectId:  projectID,
		Secret:     apiclientgen.NewOptString(secretID),
		TargetKind: apiclientgen.NewOptListSecretBindingsTargetKind(apiclientgen.ListSecretBindingsTargetKindSandbox),
		Target:     apiclientgen.NewOptString(box),
	})
	if err != nil {
		return nil, err
	}
	body, err := expectResponse[apimodel.ListSecretBindingsBody](res)
	if err != nil {
		return nil, err
	}
	held := map[string]apimodel.SecretBinding{}
	var standing []bindingplan.Binding
	for _, b := range body.GetSecretBindings() {
		if b.SecretId != secretID || b.TargetKind != apiclientgen.SecretBindingTargetKindSandbox || b.TargetId != box {
			continue
		}
		held[b.ID] = b
		standing = append(standing, bindingplan.Binding{ID: b.ID, Use: b.Use, Delegate: b.Delegate, Adds: b.Adds.Or(false), EnvName: b.EnvVar.Or(""), Created: b.CreatedAt})
	}
	_, asksUse := request.Use.Get()
	_, asksDelegate := request.Delegate.Get()
	if !asksUse {
		useAs = ""
	}
	use, delegate := bindingplan.Answer(standing, request.EnvName.Or(""), useAs, asksDelegate && delegateApproved)
	var out []partReplacement
	if useAs != "" {
		out = append(out, partReplacement{label: "use", plan: use, held: held})
	}
	if asksDelegate && delegateApproved {
		out = append(out, partReplacement{label: "delegate", plan: delegate, held: held})
	}
	return out, nil
}

// lines says, of one part, what its approval would do (or did) to each
// binding the discobox held, and whether it makes a new one.
func (r partReplacement) lines(done bool) []string {
	verb := func(would, did string) string {
		if done {
			return did
		}
		return would
	}
	var out []string
	if r.plan.Adds {
		return []string{verb("an addition is made beside them; none of them is touched", "an addition was made beside them; none of them was touched")}
	}
	for _, step := range r.plan.Steps {
		// Said as the binding stands when this part is applied: a delegate
		// part after a use part reads what the use part left.
		b := r.held[step.Binding.ID]
		what := "the delegation " + b.ID
		switch {
		case step.Binding.Adds:
			what = "the addition " + b.ID + " in " + step.Binding.EnvName
		case step.Binding.Use:
			what = b.ID + " in " + step.Binding.EnvName
		}
		where := "the secret's routes"
		if len(b.Routes) > 0 {
			where = routesText(b.Routes)
		}
		var fate string
		switch step.Fate {
		case bindingplan.Updated:
			fate = verb("is kept and takes the approval", "was kept and took the approval")
		case bindingplan.Deleted:
			fate = verb("goes", "went")
		case bindingplan.LosesUse:
			fate = verb("loses its use; its delegation stays", "lost its use; its delegation stayed")
		case bindingplan.Split:
			fate = verb("stays the delegation; its use moves to a binding of its own in the same variable", "stayed the delegation; its use moved to a binding of its own in the same variable")
		}
		out = append(out, fmt.Sprintf("%s (sent to %s) %s", terminalSafe(what), where, fate))
	}
	if r.plan.Keep == "" {
		out = append(out, verb("a new binding is made", "a new binding was made"))
	}
	return out
}

// writeReplacements writes each part's replacement as rows of the request's
// table: "USE REPLACES", one line per binding it touches.
func writeReplacements(w io.Writer, replacements []partReplacement, done bool) {
	for _, r := range replacements {
		label := strings.ToUpper(r.label) + " REPLACES"
		for _, line := range r.lines(done) {
			fmt.Fprintf(w, "%s\t%s\n", label, line)
			label = ""
		}
	}
}
