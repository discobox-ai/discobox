package tui

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/cli/internal/policyview"
	"github.com/discobox-ai/discobox/pkg/policy"
)

// An approval rule's policy as a card edits it (ADR 26-10-12-330): one line
// each in the list a rule is written in (askLines) — every statement as its
// sentence, then every word that is not a default as `word = what it means` —
// the two things discobox-access writes a policy with, --statement and
// --define. The list is read back into a policy as it is written, so what is
// wrong with it is said on the card while it can still be changed.

// policyDefines is how a definition's line joins its word to its meaning.
const policyDefines = " = "

// policyText is p as the lines it is edited in: its statements in order, then
// its definitions by word.
func policyText(p *policy.Policy) []string {
	if p == nil {
		return nil
	}
	out := policy.FormatStatements(p.Statements)
	for _, word := range slices.Sorted(maps.Keys(p.Definitions)) {
		out = append(out, word+policyDefines+p.Definitions[word])
	}
	return out
}

// policyEditLines are a policy the server holds as the list opens on it: each
// line drawn from shown, made safe, and sent as stored until the person
// changes it.
func policyEditLines(shown, stored *policy.Policy) []listLine {
	return storedLines(policyText(shown), policyText(stored))
}

// samePolicy reports whether two policies say the same: the same statements
// and the same definitions. What a variable resolved to is no part of what a
// rule says.
func samePolicy(a, b *policy.Policy) bool {
	return (a == nil) == (b == nil) && slices.Equal(policyText(a), policyText(b))
}

// parsedPolicy reads a rule's policy from its lines, with every reason it is
// not one a rule can have, each a line a card can show: a line that is
// neither a statement nor a definition, a word defined twice, and each way
// Validate refuses the whole, and one that allows nothing. A policy with
// problems is not sent. A grant's policy is read the same way.
func parsedPolicy(lines []string) (*policy.Policy, []string) {
	p := &policy.Policy{}
	var problems []string
	for i, text := range lines {
		text = strings.TrimSpace(text)
		if word, definition, ok := definitionLine(text); ok {
			if _, seen := p.Definitions[word]; seen {
				problems = append(problems, fmt.Sprintf("line %d: %q is defined twice", i+1, word))
				continue
			}
			if p.Definitions == nil {
				p.Definitions = map[string]string{}
			}
			p.Definitions[word] = definition
			continue
		}
		statement, err := policy.Parse(text)
		var syntax *policy.SyntaxError
		switch {
		case errors.As(err, &syntax):
			// The grammar is said once under the list, not with each line.
			problems = append(problems, fmt.Sprintf("line %d: at %s (byte %d): expected %s", i+1, syntax.Found, syntax.Offset, syntax.Expected))
		case err != nil:
			problems = append(problems, fmt.Sprintf("line %d: %v", i+1, err))
		default:
			p.Statements = append(p.Statements, statement)
		}
	}
	if len(problems) > 0 {
		return nil, problems
	}
	if err := p.Validate(); err != nil {
		return nil, strings.Split(err.Error(), "\n")
	}
	if !slices.ContainsFunc(p.Statements, func(s policy.Statement) bool { return s.Effect == policy.Allow }) {
		return nil, []string{"a policy allows only what its allows allow, and these are all denies"}
	}
	return p, nil
}

// definitionLine reads `word = what it means`. A statement opens with its
// effect, so a line that does is a statement whatever follows, an "=" in a
// name or a condition included.
func definitionLine(text string) (word, definition string, ok bool) {
	if first, _, _ := strings.Cut(text, " "); first == string(policy.Allow) || first == string(policy.Deny) {
		return "", "", false
	}
	word, definition, ok = strings.Cut(text, "=")
	word = strings.TrimSpace(word)
	return word, strings.TrimSpace(definition), ok && word != ""
}

// policyProblemsSection says what keeps the list from being a policy a rule
// can have, as it stands, with the grammar a statement is written in; an
// empty list, or one that is a policy, has nothing to say. It is drawn from
// the lines as they are drawn, which are safe.
func policyProblemsSection(list lineList) []section {
	shown := list.shown()
	if len(shown) == 0 {
		return nil
	}
	if _, problems := parsedPolicy(shown); len(problems) > 0 {
		sec := section{label: "not a policy yet"}
		for _, problem := range problems {
			sec.lines = append(sec.lines, line{text: problem, bullet: true, tone: toneAlert})
		}
		sec.lines = append(sec.lines,
			line{text: "a statement reads: " + policy.Grammar, tone: toneDim},
			line{text: "a word that is not a default (read, create, update, delete, manage, anything, anywhere) is defined on a line of its own: word = what it means", tone: toneDim},
		)
		return []section{sec}
	}
	return nil
}

// rulePolicyLines is a rule's policy as a card draws it, as the request card
// draws the policy it asks for (policyLines), each variable bare: a rule binds
// it to whichever discobox asks. A tone other than plain draws it in that
// tone rather than as syntax: a policy a rule no longer says, or one that
// is only read.
func rulePolicyLines(p *policy.Policy, t tone) []line {
	if p == nil {
		return nil
	}
	out := policyLines(policyview.Unbound(*p))
	if t != tonePlain {
		for i := range out {
			out[i].spans, out[i].tone = nil, t
		}
	}
	return out
}

// policySize is how much a policy says, for a field that cannot hold it.
func policySize(p *policy.Policy) string {
	if p == nil {
		return "nothing yet"
	}
	size := plural(len(p.Statements), "statement", "statements")
	if len(p.Definitions) > 0 {
		size += ", " + plural(len(p.Definitions), "definition", "definitions")
	}
	return size
}

// listPolicy is the policy a finished list says: sent, read from its lines as
// they are sent, and shown, read from them as they are drawn — which differ
// only where a line the server holds had to be made safe to draw. problems
// are the sent one's, which is nil when there are any.
func listPolicy(list lineList) (sent, shown *policy.Policy, problems []string) {
	sent, problems = parsedPolicy(list.values())
	if shown, _ = parsedPolicy(list.shown()); shown == nil {
		shown = sent
	}
	return sent, shown, problems
}

// notAPolicyYet is what a list finished while it is not a policy is told.
const notAPolicyYet = "not a policy yet: what is wrong is said above"
