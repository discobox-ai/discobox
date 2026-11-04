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
	// Adds is an addition (ADR 26-11-03-329 §1): a use alone, never the
	// binding a replacement keeps.
	Adds    bool
	EnvName string
	Created time.Time
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
// place, empty when the approval makes a new binding. Adds is a use part
// granted as an addition (ADR 26-11-03-329 §4): it makes one beside what
// stands and touches nothing, so it has no steps and keeps nothing.
type Plan struct {
	Steps []Step
	Keep  string
	Adds  bool
}

// As is how an approved use part is granted (ADR 26-11-03-329 §4).
type As string

const (
	// Replace makes the part the discobox's whole use of the secret (Use).
	Replace As = "replace"
	// Add makes it an addition beside what the discobox holds (Add).
	Add As = "add"
)

// Use plans an approved use part delivered in envName, granted to replace:
// every use binding of the secret becomes the one approved. The plain
// use-only one already in envName is kept, else the oldest plain use-only
// one; the other use-only ones go, every addition among them (ADR
// 26-11-03-329 §4); one that also delegates loses only its use side.
func Use(standing []Binding, envName string) Plan {
	standing = oldestFirst(standing)
	var only []Binding
	for _, b := range standing {
		if b.Use && !b.Delegate && !b.Adds {
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

// AddBeside plans an approved use part granted as an addition: one is made
// beside the standing bindings, and none of them is touched.
func AddBeside() Plan {
	return Plan{Adds: true}
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
	// Compared at whole seconds, ties broken by ID. The server plans from
	// the stored time; the CLI and console plan from the API's createdAt,
	// which carries whole seconds (RFC 3339). Ordered at any finer
	// precision, two bindings made within one second would be kept one way
	// by the server and previewed the other.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Created.Truncate(time.Second), out[j].Created.Truncate(time.Second)
		if !a.Equal(b) {
			return a.Before(b)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Answer plans a whole answer: the use part as useAs grants it — empty when
// it is not approved — then the delegate part when delegateApproved, planned
// against what the use part leaves — the order the server applies them in,
// reading the bindings again between the two. Planning both from the same
// standing bindings would be wrong: a binding that both uses and delegates
// loses its use to a replacing use part, and is then the delegation alone,
// updated rather than split. A part not approved has an empty plan.
func Answer(standing []Binding, envName string, useAs As, delegateApproved bool) (use, delegate Plan) {
	switch useAs {
	case Replace:
		use = Use(standing, envName)
		standing = After(standing, use, envName)
	case Add:
		use = AddBeside()
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

// Held is one grant applying in the asked variable now, as a preview reads
// it (ADR 26-11-03-329 §5): the binding delivering the variable, or an
// addition stacking on it. Own is whether it is the discobox's own, on the
// sandbox, rather than its harness config's or project's; Expires is when it
// lapses, zero never.
type Held struct {
	ID      string
	Adds    bool
	Own     bool
	Expires time.Time
}

// HeldFate is what granting a use part as a replacement does to a grant
// held in its variable.
type HeldFate string

const (
	// Goes is an addition: a replacement is the discobox's whole use of the
	// secret, so every addition is deleted (Use).
	Goes HeldFate = "goes"
	// Shortened is the discobox's own binding, kept and taking the
	// approval's routes and lifetime, which is shorter than it had (Use).
	Shortened HeldFate = "shortened"
	// Shadowed is a harness config's or project's binding: not the
	// discobox's own, so not touched, but the approval's, on the sandbox, is
	// narrower and delivers the variable while it lasts.
	Shadowed HeldFate = "shadowed"
)

// HeldStep is one grant held and what a replacement does to it.
type HeldStep struct {
	Held Held
	Fate HeldFate
}

// Replacing says what granting a use part as a replacement, lasting until
// until (zero never), does to each grant held in its variable, by the rule
// Use applies, in the order held: an addition goes, the discobox's own
// binding is cut short when it had longer, and one from its harness config
// or project is shadowed. A grant left as it would be is not listed. An
// addition changes none of them (AddBeside).
func Replacing(held []Held, until time.Time) []HeldStep {
	var out []HeldStep
	for _, h := range held {
		switch {
		case h.Adds:
			out = append(out, HeldStep{h, Goes})
		case !h.Own:
			out = append(out, HeldStep{h, Shadowed})
		case !until.IsZero() && (h.Expires.IsZero() || h.Expires.After(until)):
			out = append(out, HeldStep{h, Shortened})
		}
	}
	return out
}

// NewRoutes are the routes asked that no grant held names, compared as the
// caller writes them: a comparison of routes as written, never a claim that
// the others are already allowed, since whether one route covers another is
// not decided (ADR 26-10-27-880). Each held entry is one grant's own routes,
// never the secret's, which bound every grant and are none of them.
func NewRoutes(asked []string, held [][]string) []string {
	named := map[string]bool{}
	for _, routes := range held {
		for _, route := range routes {
			named[route] = true
		}
	}
	var out []string
	for _, route := range asked {
		if !named[route] {
			out = append(out, route)
		}
	}
	return out
}
