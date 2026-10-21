package cli

import (
	"encoding/json"
	"fmt"
	"io"
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

// safePolicyLines is p rendered for the window (policyview), every line made
// safe to draw: a definition is text a discobox wrote.
func safePolicyLines(p policy.Policy) []policyview.Line {
	lines := policyview.Lines(p)
	for i := range lines {
		lines[i].Text = terminalSafe(lines[i].Text)
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
