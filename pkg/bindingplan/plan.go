// Package bindingplan decides what approving one part of a credential
// request does to the bindings the asking discobox already holds of the
// secret (ADR 26-11-02-022 §5). The server applies the plan; the CLI and the
// console show it before a person approves, so access lost to a replacement
// is said, not found after. One rule, read alike by both.
package bindingplan

import (
	"sort"
	"time"
)

// Binding is one of the asking discobox's own sandbox-level bindings of the
// secret, as much of it as the plan reads. Bindings at the harness config
// and project levels are not the discobox's own and are never given here.
type Binding struct {
	ID       string
	Use      bool
	Delegate bool
	EnvName  string
	Created  time.Time
}

// Fate is what an approval does to a standing binding.
type Fate string

const (
	// Updated is the binding kept: it takes the approved part's routes and
	// lifetime (and, for a use part, its variable and delivery) in place.
	Updated Fate = "updated"
	// Deleted is a use-only binding of the secret beside the one kept.
	Deleted Fate = "deleted"
	// LosesUse is a binding that also delegates, approved over by a use
	// part: its use side is cleared and its delegation stays as it was.
	LosesUse Fate = "loses use"
	// Split is a binding that also uses, approved over by a delegate part:
	// it stays the delegation, taking the approval, and its use side moves
	// to a binding of its own in the same variable.
	Split Fate = "split"
)

// Step is one standing binding and its fate.
type Step struct {
	Binding Binding
	Fate    Fate
}

// Plan is what approving a part does. Steps name every standing binding the
// approval touches, oldest first; Keep is the ID of the one updated in
// place, empty when the approval makes a new binding.
type Plan struct {
	Steps []Step
	Keep  string
}

// Use plans an approved use part delivered in envName: every use binding of
// the secret becomes the one approved. The use-only one already in envName
// is kept, else the oldest use-only one; the other use-only ones go; one that
// also delegates loses only its use side.
func Use(standing []Binding, envName string) Plan {
	standing = oldestFirst(standing)
	var only []Binding
	for _, b := range standing {
		if b.Use && !b.Delegate {
			only = append(only, b)
		}
	}
	var plan Plan
	for _, b := range only {
		if b.EnvName == envName {
			plan.Keep = b.ID
			break
		}
	}
	if plan.Keep == "" && len(only) > 0 {
		plan.Keep = only[0].ID
	}
	for _, b := range standing {
		switch {
		case !b.Use:
		case b.Delegate:
			plan.Steps = append(plan.Steps, Step{b, LosesUse})
		case b.ID == plan.Keep:
			plan.Steps = append(plan.Steps, Step{b, Updated})
		default:
			plan.Steps = append(plan.Steps, Step{b, Deleted})
		}
	}
	return plan
}

// Delegate plans an approved delegate part: the delegate binding of the
// secret is updated in place, keeping its ID so what was handed on under it
// stays; one that also uses is split. A discobox holds at most one: the
// server's store refuses a second (secret_bindings' unique
// idx_secret_binding_delegate on target and secret, where delegate).
func Delegate(standing []Binding) Plan {
	for _, b := range oldestFirst(standing) {
		if !b.Delegate {
			continue
		}
		fate := Updated
		if b.Use {
			fate = Split
		}
		return Plan{Steps: []Step{{b, fate}}, Keep: b.ID}
	}
	return Plan{}
}

func oldestFirst(standing []Binding) []Binding {
	out := append([]Binding(nil), standing...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

// Answer plans a whole answer: the use part when useApproved, then the
// delegate part when delegateApproved, planned against what the use part
// leaves — the order the server applies them in, reading the bindings again
// between the two. Planning both from the same standing bindings would be
// wrong: a binding that both uses and delegates loses its use to the use
// part, and is then the delegation alone, updated rather than split.
// A part not approved has an empty plan.
func Answer(standing []Binding, envName string, useApproved, delegateApproved bool) (use, delegate Plan) {
	if useApproved {
		use = Use(standing, envName)
		standing = After(standing, use, envName)
	}
	if delegateApproved {
		delegate = Delegate(standing)
	}
	return use, delegate
}

// After is the standing bindings once a use part's plan is applied: the ones
// it deletes are gone, the ones that lose their use are delegations alone,
// and the one kept is in envName. A binding the plan makes is left out: it
// is a use binding alone, which no delegate part reads.
func After(standing []Binding, use Plan, envName string) []Binding {
	fates := map[string]Fate{}
	for _, step := range use.Steps {
		fates[step.Binding.ID] = step.Fate
	}
	var out []Binding
	for _, b := range standing {
		switch fates[b.ID] {
		case Deleted:
			continue
		case LosesUse:
			b.Use, b.EnvName = false, ""
		case Updated:
			b.EnvName = envName
		}
		out = append(out, b)
	}
	return out
}
