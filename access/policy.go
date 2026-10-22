package access

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"

	"github.com/discobox-ai/discobox/agentcreds"
	"github.com/discobox-ai/discobox/policy"
)

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
