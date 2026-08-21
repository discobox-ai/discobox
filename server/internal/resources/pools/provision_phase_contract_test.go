package pools

import (
	"sort"
	"testing"

	serverapi "github.com/discobox-ai/discobox/api/gen"
)

// TestProvisionPhaseVocabulariesMatch pins the agent-facing provisioning phase
// enum to the client-facing one.
//
// ReportSandboxStates converts between the two schemas. The pull struct crosses
// as a struct conversion, which stops compiling the moment the shapes diverge —
// deliberately, and the comment there says so. The phase cannot: ogen generates
// both enums as `type X string`, so `SandboxProvisionPhase(entry.Phase)`
// compiles no matter how far apart the two value sets drift, and a phase the
// client schema omits would be stored, served, and rejected only when the CLI
// decodes it. That is the same failure mode TestModelEnumsMatchAPISchema exists
// to prevent one layer down.
//
// When this fails: the two enums in api/openapi/server.yaml have diverged. They
// are separate schemas because they are separate contracts, but they name the
// same set of things a pool agent can be doing, so a value belongs in both or
// in neither.
func TestProvisionPhaseVocabulariesMatch(t *testing.T) {
	agent := phaseValues(serverapi.PoolSandboxProvisionPhase("").AllValues())
	client := phaseValues(serverapi.SandboxProvisionPhase("").AllValues())

	if len(agent) == 0 {
		t.Fatal("the agent-facing provision phase enum is empty; the schema lost its values")
	}
	agentOnly := missing(agent, client)
	clientOnly := missing(client, agent)
	if len(agentOnly) > 0 {
		t.Errorf("phases a pool agent may report that a client cannot decode: %v\n"+
			"add them to SandboxProvisionPhase in api/openapi/server.yaml and regenerate", agentOnly)
	}
	if len(clientOnly) > 0 {
		t.Errorf("phases the client schema admits that no pool agent can report: %v\n"+
			"add them to PoolSandboxProvisionPhase in api/openapi/server.yaml, or drop them", clientOnly)
	}
}

func phaseValues[T ~string](values []T) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	sort.Strings(out)
	return out
}

// missing returns the values in have that want does not contain.
func missing(have, want []string) []string {
	index := make(map[string]struct{}, len(want))
	for _, value := range want {
		index[value] = struct{}{}
	}
	var out []string
	for _, value := range have {
		if _, ok := index[value]; !ok {
			out = append(out, value)
		}
	}
	return out
}
