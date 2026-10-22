package judge

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/discobox-ai/discobox/pkg/policy"
)

// An approval rule's policy, and how a request's is held to it (ADR
// 26-10-22-689). Here, in the root module beside UnderPolicy, because the
// control plane holds requests to rules and the judge evals hold their cases
// the same way: one owner, never a copy that drifts.

// RuleProblem says why p could not be an approval rule's policy, or nil when
// it could: it validates as a grant's does, a judge can be shown it, and it
// allows something — a rule of denies alone would cover nothing.
func RuleProblem(p policy.Policy) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := PolicyFits(p); err != nil {
		return err
	}
	if !slices.ContainsFunc(p.Statements, func(s policy.Statement) bool { return s.Effect == policy.Allow }) {
		return errors.New("a rule approves what its allows allow, and this policy has none")
	}
	return nil
}

// RuleHold is what code found holding a request's policy to a rule's.
//
// Values are what the request's variables resolved to for the discobox that
// asked: a grant the rule mints is held to them. Reason says why the request
// is not covered, when code found it is not. Otherwise Questions are what
// only a judge can answer, one entry per allow asked for that code found
// covered but for its conditions: the rule jobs putting it against each allow
// of the rule that covers it otherwise. That allow is within when any one of
// them is allowed, and the request is covered when every entry is. No
// Questions and no Reason is covered by code alone.
type RuleHold struct {
	Values    map[string]string
	Reason    string
	Questions [][]Job
}

// Covered reports whether code alone found the request covered.
func (h RuleHold) Covered() bool { return h.Reason == "" && len(h.Questions) == 0 }

// HoldToRule holds asked, the policy a request asks for, to rule's, both
// resolved for the discobox facts describes: a rule's variables bind to
// whichever discobox asks. policy.Within decides all of it but the conditions
// of an allow it found covered otherwise, which become rule jobs, each
// statement written as it is bound for that discobox. credential and hosts
// are the rule's, which the request has already been held to. An error is a
// job that could not be put, never a request that is not covered.
func HoldToRule(asked, rule policy.Policy, facts policy.Facts, credential string, hosts []string) (RuleHold, error) {
	hold := RuleHold{Values: asked.Resolve(facts).Resolved}
	// An allow whose variable resolves to nothing matches nothing, so a
	// policy all of whose allows do is within any rule's and allows nothing:
	// no rule approves that for a person who would read it.
	if !allowsAnything(asked, hold.Values) {
		hold.Reason = "no allow asked for resolves for the discobox that asked, so it allows nothing a rule could approve"
		return hold, nil
	}
	unsettled, err := policy.Within(asked, hold.Values, rule, rule.Resolve(facts).Resolved)
	if err != nil {
		hold.Reason = err.Error()
		return hold, nil //nolint:nilerr // Not within is the hold's answer, not a failure to hold it.
	}
	for _, u := range unsettled {
		jobs := make([]Job, 0, len(u.Delegated))
		for _, theirs := range u.Delegated {
			job := Job{
				Kind:       KindRule,
				Purposes:   []string{policy.Format(theirs)},
				Host:       strings.Join(hosts, ", "),
				Credential: credential,
				Round:      1,
				Uses:       []string{policy.Format(u.Given)},
				Facts:      Facts(DiscoboxOf(facts)),
			}
			if err := job.Validate(); err != nil {
				return RuleHold{}, fmt.Errorf("%s: %w", policy.Format(u.Given), err)
			}
			jobs = append(jobs, job)
		}
		hold.Questions = append(hold.Questions, jobs)
	}
	return hold, nil
}

// allowsAnything reports whether some allow of p has every variable it names
// among values: one that matches something for the discobox they resolved
// for.
func allowsAnything(p policy.Policy, values map[string]string) bool {
	for _, s := range p.Statements {
		if s.Effect != policy.Allow {
			continue
		}
		resolves := true
		for _, variable := range (policy.Policy{Statements: []policy.Statement{s}}).Variables() {
			if _, ok := values[variable]; !ok {
				resolves = false
			}
		}
		if resolves {
			return true
		}
	}
	return false
}

// WrittenPolicy is the policy a proposal's sentences (Proposal) say, as a
// rule may have it. Its definitions are definitions', the source's, for the
// words it uses: a model defines no word, so one it brought in has none and
// is refused, as is a sentence that does not parse.
func WrittenPolicy(sentences []string, definitions map[string]string) (policy.Policy, error) {
	statements, err := policy.ParseStatements(sentences)
	if err != nil {
		return policy.Policy{}, err
	}
	written := policy.Policy{Statements: statements}
	for _, word := range written.Words() {
		if definition, ok := definitions[word]; ok {
			if written.Definitions == nil {
				written.Definitions = map[string]string{}
			}
			written.Definitions[word] = definition
		}
	}
	if err := RuleProblem(written); err != nil {
		return policy.Policy{}, err
	}
	return written, nil
}
