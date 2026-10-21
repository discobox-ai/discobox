// Package policyview is a credential policy as the person approving it reads
// it (ADR 26-10-20-782 §3): its statements grouped by the scope they apply
// in, the denies that apply anywhere once, as the policy's own, each
// condition under its statement, every word that is not a default with its
// definition, and every variable with the value it resolved to.
//
// It is display only. The rendering may change as Discobox improves without
// changing what is approved: what is sent back is the policy, never these
// lines. Each statement is policy.Format's sentence less the scope its group
// names and its conditions, so a line reads as the sentence an agent wrote.
package policyview

import (
	"slices"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/policy"
)

// Kind is what a line of the rendering is, for a surface that sets each kind
// apart.
type Kind int

const (
	// Heading opens a group: the scope its statements apply in, the
	// policy-wide denies, the words, or the variables.
	Heading Kind = iota
	// Statement is one statement, less its group's scope and its conditions.
	Statement
	// Condition is a when or unless, under its statement.
	Condition
	// Definition is a word and what it means.
	Definition
	// Variable is a variable and the value it resolved to.
	Variable
	// Unresolved is a variable, or a heading naming one, that resolved to
	// nothing and so matches nothing.
	Unresolved
)

// Line is one line of the rendering.
type Line struct {
	Text string
	// Indent is the depth under its heading: 0 for a heading, 1 for what is
	// under it, 2 for a statement's condition.
	Indent int
	Kind   Kind
}

// Everywhere heads the denies that apply in every scope.
const Everywhere = "everywhere, whatever else is allowed"

// unresolved is what a variable that did not resolve is shown as.
const unresolved = "does not resolve, so it matches nothing"

// Lines renders p. Groups are in the order their first statement is, the
// policy-wide denies after them; statements keep their order within a
// group. p.Resolved is taken as what each variable resolved to: one used and
// absent from it did not resolve.
func Lines(p policy.Policy) []Line {
	type group struct {
		scope      policy.Scope
		statements []policy.Statement
	}
	var groups []*group
	byScope := map[string]*group{}
	var everywhere []policy.Statement
	for _, s := range p.Statements {
		if s.Scope.Type == policy.Anywhere {
			everywhere = append(everywhere, s)
			continue
		}
		key := policy.FormatScope(s.Scope)
		g, ok := byScope[key]
		if !ok {
			g = &group{scope: s.Scope}
			byScope[key] = g
			groups = append(groups, g)
		}
		g.statements = append(g.statements, s)
	}

	var out []Line
	for _, g := range groups {
		out = append(out, scopeHeading(g.scope, p.Resolved))
		out = appendStatements(out, g.statements)
	}
	if len(everywhere) > 0 {
		out = append(out, Line{Text: Everywhere, Kind: Heading})
		out = appendStatements(out, everywhere)
	}
	if len(p.Definitions) > 0 {
		out = append(out, Line{Text: "words", Kind: Heading})
		for _, word := range sortedKeys(p.Definitions) {
			out = append(out, Line{Text: word + ": " + p.Definitions[word], Indent: 1, Kind: Definition})
		}
	}
	if variables := p.Variables(); len(variables) > 0 {
		out = append(out, Line{Text: "variables", Kind: Heading})
		for _, variable := range variables {
			if value, ok := p.Resolved[variable]; ok {
				out = append(out, Line{Text: variable + ": " + value, Indent: 1, Kind: Variable})
			} else {
				out = append(out, Line{Text: variable + ": " + unresolved, Indent: 1, Kind: Unresolved})
			}
		}
	}
	return out
}

// scopeHeading is the heading of the statements in scope: the scope as a
// sentence writes it, then what each variable in it — its name, and its
// parents' — resolved to. One that did not resolve makes the heading
// Unresolved: the scope matches nothing, whatever it was meant to name.
func scopeHeading(scope policy.Scope, resolved map[string]string) Line {
	var variables []string
	add := func(name string) {
		if policy.IsVariable(name) && !slices.Contains(variables, name) {
			variables = append(variables, name)
		}
	}
	add(scope.Name)
	for _, parent := range slices.Backward(scope.Within) {
		if parent.Name != nil {
			add(*parent.Name)
		}
		add(parent.CreatedBy)
	}
	heading := Line{Text: "in " + policy.FormatScope(scope), Kind: Heading}
	var values []string
	for _, variable := range variables {
		value, ok := resolved[variable]
		if !ok {
			value, heading.Kind = unresolved, Unresolved
		}
		if len(variables) > 1 {
			value = variable + ": " + value
		}
		values = append(values, value)
	}
	if len(values) > 0 {
		heading.Text += " — " + strings.Join(values, "; ")
	}
	return heading
}

// appendStatements renders each statement under its group's heading, its
// conditions under it. Conditions are quoted as Format quotes them, in ASCII,
// so a condition cannot seem to end early and start another.
func appendStatements(out []Line, statements []policy.Statement) []Line {
	for _, s := range statements {
		bare := s
		bare.Scope, bare.When, bare.Unless = policy.Scope{}, nil, nil
		out = append(out, Line{Text: policy.Format(bare), Indent: 1, Kind: Statement})
		if s.When != nil {
			out = append(out, Line{Text: "when " + strconv.QuoteToASCII(*s.When), Indent: 2, Kind: Condition})
		}
		if s.Unless != nil {
			out = append(out, Line{Text: "unless " + strconv.QuoteToASCII(*s.Unless), Indent: 2, Kind: Condition})
		}
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
