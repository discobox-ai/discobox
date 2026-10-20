// Package policy is what a credential grant authorizes (ADR 26-10-20-782 §1):
// a set of statements, the definitions of the words they use, and the values
// their variables resolved to when a person approved them.
//
// It holds the contract and the code that needs no reading: the shape, its
// validation, the two phases that combine decided parts into a verdict (§2),
// and the variables a policy may use, resolved from a discobox's typed facts
// (§4). Deciding whether a request is one of a statement's actions on its
// resource is not here; whoever decides a part hands the answer to Evaluate.
package policy

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

// Effect is what a statement does to the requests it matches.
type Effect string

const (
	// Allow covers what it matches. Every part of a request must be covered
	// by some allow.
	Allow Effect = "allow"
	// Deny refuses what it matches, whatever allows it.
	Deny Effect = "deny"
)

// The default actions. They carry no definition and a policy cannot give them
// one, or a definition could quietly widen read (§1).
const (
	ActionRead   = "read"
	ActionCreate = "create"
	ActionUpdate = "update"
	ActionDelete = "delete"
	// ActionManage is every action on the resource type.
	ActionManage = "manage"
)

const (
	// Anything is the resource type that covers every type in a scope.
	Anything = "anything"
	// Anywhere is the scope type of a deny that applies in every scope. An
	// allow cannot use it: an allow names where it applies.
	Anywhere = "anywhere"
)

// Bounds on what a policy may hold. A policy can arrive from inside a
// discobox, and a person has to read all of it to approve it.
const (
	MaxStatements  = 64
	MaxActions     = 16
	MaxParents     = 8
	MaxDefinitions = 128
	// MaxWordBytes bounds an action, a type, a name, and a variable.
	MaxWordBytes = 200
	// MaxTextBytes bounds a condition and a definition.
	MaxTextBytes = 1000
)

// Policy is a grant's authorization: statements, and a definition for every
// word they use that is not a default.
type Policy struct {
	Statements []Statement `json:"statements"`
	// Definitions maps each non-default word — an action, a resource type, a
	// parent's or a scope's type — to what it means. A person approves the
	// definitions with the statements.
	Definitions map[string]string `json:"definitions,omitempty"`
	// Resolved is the value each variable the statements use resolved to
	// when the policy was approved, against the discobox it was approved for
	// (§4). A variable used but absent did not resolve, and matches nothing.
	// Empty on a policy nobody has approved.
	Resolved map[string]string `json:"resolved,omitempty"`
}

// Statement is one `<effect> <actions> on <resource type> [<name>] [within
// <parents>] in <scope> [when <condition>] [unless <condition>]`.
type Statement struct {
	Effect       Effect   `json:"effect"`
	Actions      []string `json:"actions"`
	ResourceType string   `json:"resourceType"`
	// Name limits the statement to resources by that name: a literal, a glob
	// (path.Match syntax), or a variable. Nil is any name.
	Name *string `json:"name"`
	// CreatedBy limits the statement to resources the named discobox created.
	// The only value is "$this".
	CreatedBy string `json:"createdBy,omitempty"`
	// Within are the resources the resource sits in, outermost first, short of
	// the scope: the pull request a comment is on.
	Within []Resource `json:"within,omitempty"`
	Scope  Scope      `json:"scope"`
	// When and Unless are conditions a request can be checked against, never
	// an example or an intention. Nil is none; present, they are not empty.
	When   *string `json:"when"`
	Unless *string `json:"unless"`
}

// Resource is a parent a statement's resource sits in.
type Resource struct {
	Type string `json:"type"`
	// Name is a literal, a glob, or a variable; nil is any name.
	Name *string `json:"name"`
	// CreatedBy is "$this" or empty, as on a statement.
	CreatedBy string `json:"createdBy,omitempty"`
}

// Scope is where a statement applies: a typed name, or Anywhere.
type Scope struct {
	Type string `json:"type"`
	// Name is a literal, a glob, or a variable. Empty only for Anywhere.
	Name string `json:"name,omitempty"`
	// Within are the scopes this one sits in, outermost first: the cluster a
	// namespace is of.
	Within []Resource `json:"within,omitempty"`
}

// IsDefaultAction reports whether action is a default, which carries no
// definition.
func IsDefaultAction(action string) bool {
	switch action {
	case ActionRead, ActionCreate, ActionUpdate, ActionDelete, ActionManage:
		return true
	}
	return false
}

// isReserved reports whether word means one thing everywhere, so a policy can
// neither need nor give it a definition.
func isReserved(word string) bool {
	return IsDefaultAction(word) || word == Anything || word == Anywhere
}

// Validate reports every way p is not a policy a person could approve, joined,
// or nil. It does not check Resolved against anything: that is Bind's.
func (p Policy) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if len(p.Statements) == 0 {
		fail("a policy has at least one statement")
	}
	if len(p.Statements) > MaxStatements {
		fail("a policy has at most %d statements, not %d", MaxStatements, len(p.Statements))
	}
	if len(p.Definitions) > MaxDefinitions {
		fail("a policy has at most %d definitions, not %d", MaxDefinitions, len(p.Definitions))
	}

	used := map[string]bool{}
	for i, s := range p.Statements {
		at := fmt.Sprintf("statement %d", i+1)
		for _, err := range s.validate(used) {
			fail("%s: %w", at, err)
		}
	}

	for _, word := range sortedKeys(p.Definitions) {
		switch definition := p.Definitions[word]; {
		case isReserved(word):
			fail("%q means one thing everywhere and cannot be given a definition", word)
		case !used[word]:
			fail("%q is defined but no statement uses it", word)
		case strings.TrimSpace(definition) == "":
			fail("the definition of %q is empty", word)
		case len(definition) > MaxTextBytes:
			fail("the definition of %q is longer than %d bytes", word, MaxTextBytes)
		case HasControl(definition):
			fail("the definition of %q has a control character", word)
		}
	}
	for _, word := range sortedKeys(used) {
		if !isReserved(word) {
			if _, ok := p.Definitions[word]; !ok {
				fail("%q is not a default word and has no definition", word)
			}
		}
	}

	for _, variable := range sortedKeys(p.Resolved) {
		if !slices.Contains(p.Variables(), variable) {
			fail("%q is resolved but no statement uses it", variable)
		}
	}
	return errors.Join(errs...)
}

// validate reports what is wrong with s, and adds each vocabulary word it uses
// to used.
func (s Statement) validate(used map[string]bool) []error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	word := func(what, w string) {
		if err := checkWord(w); err != nil {
			fail("%s: %w", what, err)
			return
		}
		used[w] = true
	}

	switch s.Effect {
	case Allow, Deny:
	default:
		fail("effect is %q, not allow or deny", s.Effect)
	}

	if len(s.Actions) == 0 {
		fail("a statement names at least one action")
	}
	if len(s.Actions) > MaxActions {
		fail("a statement names at most %d actions, not %d", MaxActions, len(s.Actions))
	}
	seen := map[string]bool{}
	for _, action := range s.Actions {
		if seen[action] {
			fail("action %q is named twice", action)
		}
		seen[action] = true
		if action == Anything || action == Anywhere {
			fail("action %q is not an action", action)
			continue
		}
		word("action", action)
	}

	if s.ResourceType == Anywhere {
		fail("resource type %q is not a resource type", Anywhere)
	} else {
		word("resource type", s.ResourceType)
	}
	if s.Name != nil {
		if err := checkName(*s.Name); err != nil {
			fail("name: %w", err)
		}
	}
	if err := checkCreatedBy(s.CreatedBy); err != nil {
		fail("created by: %w", err)
	}

	if len(s.Within) > MaxParents {
		fail("a statement sits within at most %d parents, not %d", MaxParents, len(s.Within))
	}
	for _, parent := range s.Within {
		for _, err := range parent.validate(word) {
			fail("within: %w", err)
		}
	}

	switch {
	case s.Scope.Type == Anywhere && s.Effect == Allow:
		fail("an allow names a scope; only a deny may apply anywhere")
	case s.Scope.Type == Anywhere:
		if s.Scope.Name != "" || len(s.Scope.Within) > 0 {
			fail("a scope of anywhere has no name and sits within nothing")
		}
	case s.Scope.Type == Anything:
		fail("scope type %q names no scope", Anything)
	default:
		word("scope type", s.Scope.Type)
		if s.Scope.Name == "" {
			fail("scope %q has no name", s.Scope.Type)
		} else if err := checkName(s.Scope.Name); err != nil {
			fail("scope name: %w", err)
		}
		if len(s.Scope.Within) > MaxParents {
			fail("a scope sits within at most %d parents, not %d", MaxParents, len(s.Scope.Within))
		}
		for _, parent := range s.Scope.Within {
			for _, err := range parent.validate(word) {
				fail("scope within: %w", err)
			}
		}
	}

	for _, c := range []struct {
		what string
		text *string
	}{{"when", s.When}, {"unless", s.Unless}} {
		switch {
		case c.text == nil:
		case strings.TrimSpace(*c.text) == "":
			fail("%s is empty; leave it out to state no condition", c.what)
		case len(*c.text) > MaxTextBytes:
			fail("%s is longer than %d bytes", c.what, MaxTextBytes)
		case HasControl(*c.text):
			fail("%s has a control character", c.what)
		}
	}
	return errs
}

func (r Resource) validate(word func(what, w string)) []error {
	var errs []error
	if r.Type == Anything || r.Type == Anywhere {
		errs = append(errs, fmt.Errorf("parent type %q names no resource", r.Type))
	} else {
		word("parent type", r.Type)
	}
	if r.Name != nil {
		if err := checkName(*r.Name); err != nil {
			errs = append(errs, fmt.Errorf("parent name: %w", err))
		}
	}
	if err := checkCreatedBy(r.CreatedBy); err != nil {
		errs = append(errs, fmt.Errorf("parent created by: %w", err))
	}
	return errs
}

// checkWord reports whether w can be an action or a type: printable, trimmed,
// and short.
func checkWord(w string) error {
	switch {
	case w == "":
		return errors.New("empty")
	case w != strings.TrimSpace(w):
		return fmt.Errorf("%q has space around it", w)
	case len(w) > MaxWordBytes:
		return fmt.Errorf("longer than %d bytes", MaxWordBytes)
	case strings.HasPrefix(w, "$"):
		return fmt.Errorf("%q is a variable, not a word", w)
	}
	if HasControl(w) {
		return fmt.Errorf("%q has a control character", w)
	}
	return nil
}

// HasControl reports whether text holds a control character. Every word and
// text in a policy is shown to the person approving it, and one that can move
// a terminal's cursor can hide what they approve; whatever else states a
// policy's values, such as a judge's prompt, holds them to the same.
func HasControl(text string) bool {
	for _, r := range text {
		if r < ' ' || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return true
		}
	}
	return false
}

// checkName reports whether name is a literal, a glob, or a variable a policy
// may use.
func checkName(name string) error {
	if strings.HasPrefix(name, "$") {
		if !IsVariable(name) {
			return fmt.Errorf("%q is not a variable a policy may use", name)
		}
		return nil
	}
	if err := checkWord(name); err != nil {
		return err
	}
	if strings.Contains(name, "$") {
		return fmt.Errorf("%q has a variable inside it; a variable is a whole name", name)
	}
	if _, err := path.Match(name, ""); err != nil {
		return fmt.Errorf("%q is not a glob: %w", name, err)
	}
	return nil
}

func checkCreatedBy(createdBy string) error {
	if createdBy != "" && createdBy != VarThis {
		return fmt.Errorf("%q is not %s", createdBy, VarThis)
	}
	return nil
}

// Words lists the words s uses in the slots that take one — its actions, its
// resource type, and the types of its parents, its scope and the scope's
// parents — in that order, each as often as it is used. A judge is given the
// definitions of the words a question uses.
func (s Statement) Words() []string {
	words := append(slices.Clone(s.Actions), s.ResourceType)
	for _, r := range s.Within {
		words = append(words, r.Type)
	}
	words = append(words, s.Scope.Type)
	for _, r := range s.Scope.Within {
		words = append(words, r.Type)
	}
	return words
}

// Variables lists every variable p's statements use, sorted, each once.
func (p Policy) Variables() []string {
	set := map[string]bool{}
	add := func(name string) {
		if IsVariable(name) {
			set[name] = true
		}
	}
	addResource := func(r Resource) {
		if r.Name != nil {
			add(*r.Name)
		}
		add(r.CreatedBy)
	}
	for _, s := range p.Statements {
		if s.Name != nil {
			add(*s.Name)
		}
		add(s.CreatedBy)
		for _, r := range s.Within {
			addResource(r)
		}
		add(s.Scope.Name)
		for _, r := range s.Scope.Within {
			addResource(r)
		}
	}
	return sortedKeys(set)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
