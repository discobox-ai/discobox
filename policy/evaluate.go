package policy

import "fmt"

// Match is a decided answer to whether something fits: yes, no, or not known.
// Unknown is what a fast path that has no rule, or a judge that is unsure,
// hands on; it never becomes yes or no here.
type Match int

const (
	Unknown Match = iota
	No
	Yes
)

func (m Match) String() string {
	switch m {
	case Yes:
		return "yes"
	case No:
		return "no"
	}
	return "unknown"
}

// Parts is how each part of one statement fits one atom of a request (§8):
// an action on a resource in a scope. When and Unless are whether each
// condition holds; for a statement with no such condition they are ignored.
type Parts struct {
	Action   Match
	Resource Match
	Scope    Match
	When     Match
	Unless   Match
}

// Matches is whether the statement s fits the atom these parts describe: its
// action, resource and scope fit, its when holds and its unless does not. Any
// part that does not fit makes it No, whatever is unknown; otherwise anything
// unknown makes it Unknown.
func (pt Parts) Matches(s Statement) Match {
	parts := []Match{pt.Action, pt.Resource, pt.Scope}
	if s.When != nil {
		parts = append(parts, pt.When)
	}
	if s.Unless != nil {
		parts = append(parts, pt.Unless.not())
	}
	result := Yes
	for _, m := range parts {
		switch m {
		case No:
			return No
		case Unknown:
			result = Unknown
		}
	}
	return result
}

func (m Match) not() Match {
	switch m {
	case Yes:
		return No
	case No:
		return Yes
	}
	return Unknown
}

// Verdict is what Evaluate decides.
type Verdict int

const (
	// Undecided is a request some part of which is not known well enough to
	// decide: it falls through to whatever decides next.
	Undecided Verdict = iota
	Allowed
	Refused
)

func (v Verdict) String() string {
	switch v {
	case Allowed:
		return "allowed"
	case Refused:
		return "refused"
	}
	return "undecided"
}

// Outcome is a verdict and what decided it.
type Outcome struct {
	Verdict Verdict
	// Atom is the atom that decided a refusal or left the request undecided,
	// or -1 when every atom was decided the same way.
	Atom int
	// Statement is the deny that refused the request, or -1: an atom no
	// allow covers is refused by no statement. On an undecided request it is
	// the statement left unknown on Atom — a deny that might match before an
	// allow that might cover — or -1 when only whether it is a supporting read
	// is unknown.
	Statement int
	// Covered is, on an allowed request, the allow that covered each atom,
	// the first in the policy's order that did, or -1 for an atom allowed as
	// a supporting read. Which one is named changes nothing about the verdict.
	Covered []int
}

// Reason is the outcome in words, naming the atom and statement that decided
// it.
func (o Outcome) Reason() string {
	switch {
	case o.Verdict == Allowed:
		return "every part of the request is allowed and none is denied"
	case o.Verdict == Refused && o.Statement >= 0:
		return fmt.Sprintf("part %d of the request is denied by statement %d", o.Atom+1, o.Statement+1)
	case o.Verdict == Refused:
		return fmt.Sprintf("part %d of the request is allowed by no statement", o.Atom+1)
	case o.Statement >= 0:
		return fmt.Sprintf("part %d of the request could not be decided against statement %d", o.Atom+1, o.Statement+1)
	}
	return fmt.Sprintf("part %d of the request could not be decided", o.Atom+1)
}

// Atom is one part of a request — an action on a resource in a scope; a push
// of two refs is two — as decided against a policy.
type Atom struct {
	// Parts is how each statement fits the atom, in the policy's order.
	Parts []Parts
	// Supporting is whether the atom is a read that changes nothing and is an
	// ordinary step toward an allowed statement, on that statement's target:
	// looking a repository up, listing its refs before a push (§2). It is the
	// one way an atom no allow covers is not refused, and only a judge may
	// answer Yes. A fast path answers No for a write, which is never a
	// supporting step, and leaves Unknown a read it cannot place, so it falls
	// through rather than being refused.
	Supporting Match
}

// Evaluate combines decided parts into a verdict in two phases (§2): a request
// is allowed when every atom matches some allow, or is a supporting read, and
// no atom matches any deny. The order of the statements changes nothing.
//
// A deny that matches refuses, whatever else is known. A deny that might
// match leaves the request undecided unless something else refuses it, as
// does an atom that only some allow might cover, or that no allow covers and
// might be a supporting read. An atom every allow is known not to cover, and
// known not to be a supporting read, is refused. A request of no atoms is
// undecided: nothing about it has been recognized.
func Evaluate(p Policy, atoms []Atom) (Outcome, error) {
	if len(atoms) == 0 {
		return Outcome{Verdict: Undecided, Atom: -1, Statement: -1}, nil
	}
	for a, atom := range atoms {
		if len(atom.Parts) != len(p.Statements) {
			return Outcome{}, fmt.Errorf("atom %d has parts for %d statements, the policy has %d", a+1, len(atom.Parts), len(p.Statements))
		}
	}

	// Phase one: a deny that matches any atom refuses outright.
	for a, atom := range atoms {
		for s, statement := range p.Statements {
			if statement.Effect == Deny && atom.Parts[s].Matches(statement) == Yes {
				return Outcome{Verdict: Refused, Atom: a, Statement: s}, nil
			}
		}
	}
	// Phase two: every atom needs an allow, or to be a supporting read. One
	// that has neither is refused before anything unknown is weighed.
	undecided := Outcome{Verdict: Undecided, Atom: -1, Statement: -1}
	covered := make([]int, len(atoms))
	for a, atom := range atoms {
		cover, maybe := -1, -1
		for s, statement := range p.Statements {
			if statement.Effect != Allow {
				continue
			}
			switch atom.Parts[s].Matches(statement) {
			case Yes:
				if cover < 0 {
					cover = s
				}
			case Unknown:
				if maybe < 0 {
					maybe = s
				}
			}
		}
		covered[a] = cover
		known := cover >= 0 || atom.Supporting == Yes
		if !known && maybe < 0 && atom.Supporting == No {
			return Outcome{Verdict: Refused, Atom: a, Statement: -1}, nil
		}
		if undecided.Atom >= 0 {
			continue
		}
		// A deny that might match keeps the request undecided however the
		// atom is covered, and is the statement named first.
		for s, statement := range p.Statements {
			if statement.Effect == Deny && atom.Parts[s].Matches(statement) == Unknown {
				undecided = Outcome{Verdict: Undecided, Atom: a, Statement: s}
				break
			}
		}
		if undecided.Atom < 0 && !known {
			undecided = Outcome{Verdict: Undecided, Atom: a, Statement: maybe}
		}
	}
	if undecided.Atom >= 0 {
		return undecided, nil
	}
	return Outcome{Verdict: Allowed, Atom: -1, Statement: -1, Covered: covered}, nil
}
