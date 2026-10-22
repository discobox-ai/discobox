// Package policyview is a credential policy as the person approving it reads
// it (ADR 26-10-20-782 §3): its statements grouped by the scope they apply
// in, the denies that apply anywhere once under "global", each condition
// under its statement, each variable followed by the value it resolved to,
// and each word that is not a default followed by its definition the first
// time it is used.
//
// It is display only. The rendering may change as Discobox improves without
// changing what is approved: what is sent back is the policy, never these
// lines. A line is policy.Tokens' sentence — less the scope its group names
// and its conditions, for a statement — so it reads as the sentence an agent
// wrote, and a surface can highlight it by the tokens' kinds.
package policyview

import (
	"strconv"

	"github.com/discobox-ai/discobox/policy"
)

// Kind is what a line of the rendering is, for a surface that sets each kind
// apart.
type Kind int

const (
	// Heading opens a group: the scope its statements apply in, or Global.
	Heading Kind = iota
	// Statement is one statement, less its group's scope and its conditions.
	Statement
	// Condition is a when or unless, under its statement.
	Condition
	// Unresolved is a heading whose scope names a variable that resolved to
	// nothing, so it matches nothing.
	Unresolved
)

// Line is one line of the rendering.
type Line struct {
	// Text is the line as plain text, each note in parentheses after its
	// token.
	Text   string
	Tokens []Token
	// Indent is the depth under its heading: 0 for a heading, 1 for what is
	// under it, 2 for a statement's condition.
	Indent int
	Kind   Kind
}

// Token is a token of a sentence, with what is shown after it: a variable's
// value, or that it does not resolve, or a word's definition where the word
// is first used. Note is empty for every other token.
type Token struct {
	policy.Token
	Note string
}

// Global heads the denies that apply in every scope.
const Global = "global"

// unresolved is what a variable that did not resolve is shown as.
const unresolved = "does not resolve, so it matches nothing"

// Lines renders p. Groups are in the order their first statement is, the
// global denies after them; statements keep their order within a group.
// p.Resolved is taken as what each variable resolved to: one used and absent
// from it did not resolve.
func Lines(p policy.Policy) []Line { return renderPolicy(p, false) }

// Unbound renders p as Lines does, for a policy whose variables are bound
// later rather than resolved: an approval rule's, which binds them to
// whichever discobox asks (ADR 26-10-12-330). A variable is drawn bare, since
// no value it could be shown with is the one a later request is held to.
func Unbound(p policy.Policy) []Line { return renderPolicy(p, true) }

func renderPolicy(p policy.Policy, unbound bool) []Line {
	type group struct {
		scope      policy.Scope
		statements []policy.Statement
	}
	var groups []*group
	byScope := map[string]*group{}
	var global []policy.Statement
	for _, s := range p.Statements {
		if s.Scope.Type == policy.Anywhere {
			global = append(global, s)
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

	r := renderer{policy: p, unbound: unbound, defined: map[string]bool{}}
	var out []Line
	for _, g := range groups {
		heading := r.line(append([]policy.Token{{Kind: policy.TokenKeyword, Text: "in"}}, policy.ScopeTokens(g.scope)...), 0, Heading)
		for _, t := range heading.Tokens {
			if t.Kind == policy.TokenVariable && t.Note == unresolved {
				heading.Kind = Unresolved
			}
		}
		out = append(out, heading)
		out = r.statements(out, g.statements)
	}
	if len(global) > 0 {
		out = append(out, r.line([]policy.Token{{Kind: policy.TokenKeyword, Text: Global}}, 0, Heading))
		out = r.statements(out, global)
	}
	return out
}

// renderer notes tokens as the rendering reaches them, so a word is defined
// where it is first used.
type renderer struct {
	policy policy.Policy
	// unbound notes no variable: it has no value yet.
	unbound bool
	defined map[string]bool
}

// statements renders each statement under its group's heading, its
// conditions under it. Conditions are quoted as Format quotes them, in ASCII,
// so a condition cannot seem to end early and start another.
func (r *renderer) statements(out []Line, statements []policy.Statement) []Line {
	for _, s := range statements {
		bare := s
		bare.Scope, bare.When, bare.Unless = policy.Scope{}, nil, nil
		out = append(out, r.line(policy.Tokens(bare), 1, Statement))
		if s.When != nil {
			out = append(out, r.line([]policy.Token{{Kind: policy.TokenKeyword, Text: "when"}, {Kind: policy.TokenCondition, Text: strconv.QuoteToASCII(*s.When)}}, 2, Condition))
		}
		if s.Unless != nil {
			out = append(out, r.line([]policy.Token{{Kind: policy.TokenKeyword, Text: "unless"}, {Kind: policy.TokenCondition, Text: strconv.QuoteToASCII(*s.Unless)}}, 2, Condition))
		}
	}
	return out
}

// line is tokens noted, and as plain text.
func (r *renderer) line(tokens []policy.Token, indent int, kind Kind) Line {
	l := Line{Indent: indent, Kind: kind}
	var plain []policy.Token
	for _, t := range tokens {
		noted := Token{Token: t, Note: r.note(t)}
		l.Tokens = append(l.Tokens, noted)
		plain = append(plain, t)
		if noted.Note != "" {
			plain = append(plain, policy.Token{Kind: policy.TokenPunct, Text: "(" + noted.Note + ")"})
		}
	}
	l.Text = policy.Join(plain)
	return l
}

// note is what is shown after t: a variable's value, or that it does not
// resolve, every time; a word's definition, the first time it is used.
func (r *renderer) note(t policy.Token) string {
	switch t.Kind {
	case policy.TokenVariable:
		if r.unbound {
			return ""
		}
		if value, ok := r.policy.Resolved[t.Text]; ok {
			return value
		}
		return unresolved
	case policy.TokenAction, policy.TokenType:
		definition, ok := r.policy.Definitions[t.Text]
		if !ok || r.defined[t.Text] {
			return ""
		}
		r.defined[t.Text] = true
		return definition
	}
	return ""
}
