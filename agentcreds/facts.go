package agentcreds

import "github.com/discobox-ai/discobox/policy"

// The facts verb: what the implementation recorded about the caller, and what
// each variable a policy may use resolves to for it (ADR 26-10-20-782 §4). An
// agent writing a policy reads it to know what it is working on, so that a
// variable it writes, $primary.repo above all, means a value it has seen.

// PathFacts reads the caller's facts.
const PathFacts = "/" + Version + "/facts"

// FactsResponse is the facts read's body. Both halves are the
// implementation's record, never what the caller reports about itself; in
// Discobox they are the control plane's, the same record a policy resolves
// against when a person approves it.
type FactsResponse struct {
	Facts policy.Facts `json:"facts"`
	// Variables is every variable a policy may use, resolved or not.
	Variables []policy.Variable `json:"variables"`
}
