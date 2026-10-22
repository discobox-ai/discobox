package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/policyview"
	"github.com/discobox-ai/discobox/policy"
)

// policyFromAPI is a policy as the API carries it, in the policy package's
// shape, which is what is rendered and what the window holds.
func policyFromAPI(p apimodel.AccessPolicy) (policy.Policy, error) {
	// The generated type encodes itself: encoding/json cannot write an
	// optional field it left unset.
	data, err := p.MarshalJSON()
	if err != nil {
		return policy.Policy{}, fmt.Errorf("encode policy: %w", err)
	}
	var out policy.Policy
	if err := json.Unmarshal(data, &out); err != nil {
		return policy.Policy{}, fmt.Errorf("decode policy: %w", err)
	}
	return out, nil
}

// apiPolicy is p as the API carries it, for an approval that sends it back.
func apiPolicy(p policy.Policy) (apimodel.AccessPolicy, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return apimodel.AccessPolicy{}, fmt.Errorf("encode policy: %w", err)
	}
	var out apimodel.AccessPolicy
	if err := out.UnmarshalJSON(data); err != nil {
		return apimodel.AccessPolicy{}, fmt.Errorf("decode policy: %w", err)
	}
	return out, nil
}

// safePolicyLines is p rendered for the window (policyview), made safe to
// draw: a definition is text a discobox wrote. The window draws each line's
// tokens and their notes, not its text, so every one of them is escaped, the
// text with them.
func safePolicyLines(p policy.Policy) []policyview.Line {
	lines := policyview.Lines(p)
	for i := range lines {
		lines[i].Text = terminalSafe(lines[i].Text)
		for j := range lines[i].Tokens {
			lines[i].Tokens[j].Text = terminalSafe(lines[i].Tokens[j].Text)
			lines[i].Tokens[j].Note = terminalSafe(lines[i].Tokens[j].Note)
		}
	}
	return lines
}

// policyRows writes p as the person approving it reads it (policyview), one
// row each, under label: a heading flush, what it heads set in beneath it.
// A definition is text a discobox wrote, so every line is made safe to print.
func policyRows(w io.Writer, label string, p apimodel.AccessPolicy) error {
	read, err := policyFromAPI(p)
	if err != nil {
		return err
	}
	for _, line := range policyview.Lines(read) {
		fmt.Fprintf(w, "%s\t%s%s\n", label, strings.Repeat("  ", line.Indent), terminalSafe(line.Text))
		label = ""
	}
	return nil
}

// writtenPolicy is the policy a person wrote with a rule's statement and
// define flags (policy.Written), parsed and validated before anything is
// sent, as discobox-access holds an agent's: what is wrong with a sentence is
// read here, with the grammar, rather than in the server's refusal. Nil when
// no statement was given.
func writtenPolicy(statements []string, definitions policy.Definitions, statementFlag, defineFlag string) (*apimodel.AccessPolicy, error) {
	written, err := policy.Written(statements, definitions)
	if err != nil {
		return nil, fmt.Errorf("%w\na statement is given with %s, and a word that is not a default is defined with %s WORD=DEFINITION", err, statementFlag, defineFlag)
	}
	if written == nil {
		return nil, nil
	}
	sent, err := apiPolicy(*written)
	if err != nil {
		return nil, err
	}
	return &sent, nil
}

// policySentenceRows writes p as the sentences a person writes one in — each
// statement whole, then each word that is not a default with its definition
// — one row each under label, so a rule read here is one that can be written
// again with --statement and --define. A rule's policy is text a model may
// have written from an agent's ask, so every row is made safe to print.
func policySentenceRows(w io.Writer, label string, p policy.Policy) {
	for _, sentence := range policy.FormatStatements(p.Statements) {
		fmt.Fprintf(w, "%s\t%s\n", label, terminalSafe(sentence))
		label = ""
	}
	for _, word := range slices.Sorted(maps.Keys(p.Definitions)) {
		fmt.Fprintf(w, "%s\t%s means: %s\n", label, strconv.QuoteToASCII(word), terminalSafe(p.Definitions[word]))
		label = ""
	}
}

// safePolicy is p made safe to draw as text: every word, name, condition and
// definition, and every value a variable resolved to, escaped as
// terminalSafe escapes it. It is for a card that edits a policy, which draws
// its sentences and definitions as lines it can change; what is sent for a
// line left as it was is p's own.
func safePolicy(p policy.Policy) policy.Policy {
	safeName := func(name *string) *string {
		if name == nil {
			return nil
		}
		safe := terminalSafe(*name)
		return &safe
	}
	safeResources := func(resources []policy.Resource) []policy.Resource {
		var out []policy.Resource
		for _, r := range resources {
			out = append(out, policy.Resource{Type: terminalSafe(r.Type), Name: safeName(r.Name), CreatedBy: terminalSafe(r.CreatedBy)})
		}
		return out
	}
	safeMap := func(m map[string]string) map[string]string {
		if m == nil {
			return nil
		}
		out := make(map[string]string, len(m))
		for k, v := range m {
			out[terminalSafe(k)] = terminalSafe(v)
		}
		return out
	}
	out := policy.Policy{Definitions: safeMap(p.Definitions), Resolved: safeMap(p.Resolved)}
	for _, s := range p.Statements {
		actions := make([]string, 0, len(s.Actions))
		for _, action := range s.Actions {
			actions = append(actions, terminalSafe(action))
		}
		out.Statements = append(out.Statements, policy.Statement{
			Effect:       policy.Effect(terminalSafe(string(s.Effect))),
			Actions:      actions,
			ResourceType: terminalSafe(s.ResourceType),
			Name:         safeName(s.Name),
			CreatedBy:    terminalSafe(s.CreatedBy),
			Within:       safeResources(s.Within),
			Scope:        policy.Scope{Type: terminalSafe(s.Scope.Type), Name: terminalSafe(s.Scope.Name), Within: safeResources(s.Scope.Within)},
			When:         safeName(s.When),
			Unless:       safeName(s.Unless),
		})
	}
	return out
}
