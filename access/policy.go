package access

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/discobox-ai/discobox/agentcreds"
	"github.com/discobox-ai/discobox/policy"
)

// requestedPolicy is the policy a request's statements and definitions ask
// for (ADR 26-10-20-782 §1), or nil when it names no statements: a request
// of uses alone. It is parsed and validated here, before anything is sent, so
// an agent reads what is wrong with what it wrote and writes it again, rather
// than waiting on a person to refuse a policy nobody could approve.
func requestedPolicy(statements []string, definitions map[string]string) (*policy.Policy, error) {
	if len(statements) == 0 {
		if len(definitions) > 0 {
			return nil, errors.New("definitions give the words of a policy's statements their meaning, and this request has no statements; add them, or leave the definitions out")
		}
		return nil, nil
	}
	parsed, err := policy.ParseStatements(statements)
	if err != nil {
		// Every SyntaxError already says what a statement reads as.
		return nil, fmt.Errorf("a statement does not parse: %w", err)
	}
	p := policy.Policy{Statements: parsed, Definitions: definitions}
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("the policy is not one a person could approve: %w\na statement reads: %s", err, policy.Grammar)
	}
	return &p, nil
}

// definitionList is --define WORD=DEFINITION, repeatable, one word each.
type definitionList map[string]string

func (d definitionList) String() string {
	return strings.Join(slices.Sorted(maps.Keys(d)), ", ")
}

func (d definitionList) Set(value string) error {
	word, definition, ok := strings.Cut(value, "=")
	word = strings.TrimSpace(word)
	if !ok || word == "" {
		return fmt.Errorf("%q is not WORD=DEFINITION, as push=\"update a branch's commits\"", value)
	}
	if _, seen := d[word]; seen {
		return fmt.Errorf("%q is defined twice", word)
	}
	d[word] = definition
	return nil
}

// policyOutput is a policy as --json prints it: its structure, which is what
// was approved, and each statement as the sentence it reads as, in the same
// order.
type policyOutput struct {
	policy.Policy
	Sentences []string `json:"sentences"`
}

func newPolicyOutput(p *policy.Policy) *policyOutput {
	if p == nil {
		return nil
	}
	return &policyOutput{Policy: *p, Sentences: policy.FormatStatements(p.Statements)}
}

// requestStatusOutput is a request's status as --json prints it, its policy
// with its sentences.
type requestStatusOutput struct {
	agentcreds.RequestStatus
	Policy *policyOutput `json:"policy,omitempty"`
}

// credentialOutput is a credential as list --json prints it, its policy with
// its sentences.
type credentialOutput struct {
	agentcreds.Credential
	Policy *policyOutput `json:"policy,omitempty"`
}

// writePolicy prints a policy as sentences, each non-default word it uses
// with its definition, and — once approved, when Resolved is the record of
// what each variable resolved to — every variable with its value or the
// note that it resolved to nothing, and so matches nothing.
func writePolicy(w io.Writer, p *policy.Policy, approved bool) {
	if p == nil {
		return
	}
	fmt.Fprintln(w, "  policy:")
	for _, sentence := range policy.FormatStatements(p.Statements) {
		fmt.Fprintf(w, "    %s\n", sentence)
	}
	for _, word := range slices.Sorted(maps.Keys(p.Definitions)) {
		fmt.Fprintf(w, "    %q means: %s\n", word, p.Definitions[word])
	}
	if !approved {
		return
	}
	for _, variable := range p.Variables() {
		if value, ok := p.Resolved[variable]; ok {
			fmt.Fprintf(w, "    %s is %s\n", variable, strconv.QuoteToASCII(value))
		} else {
			fmt.Fprintf(w, "    %s resolved to nothing, so it matches nothing\n", variable)
		}
	}
}
