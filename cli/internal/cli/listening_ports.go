package cli

import (
	"encoding/json"

	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/cli/internal/tui"
)

// sandboxListeningPorts is what the sandbox's own processes are serving, as its
// agent last reported it (ADR 0046) — the same push that carries the git state
// and the terminal titles, so a listing can show it for every row without
// reaching into anything.
//
// The address a port is bound on is dropped here. Every reported port is
// reachable from inside the sandbox's network namespace, which is where a
// forward would dial from, so what a caller can do with the port does not
// depend on which of its addresses it was found under — leaving only the number
// and what it speaks, which is what fits beside a sandbox on one line.
func sandboxListeningPorts(sb apimodel.Sandbox) []tui.Port {
	agentStatus, ok := sb.Runtime.AgentStatus.Get()
	if !ok {
		return nil
	}
	raw, ok := agentStatus["ports"]
	if !ok {
		return nil
	}
	var reported []apimodel.SandboxAgentListeningPort
	if err := json.Unmarshal(raw, &reported); err != nil {
		return nil
	}
	out := make([]tui.Port, 0, len(reported))
	for _, port := range reported {
		if port.Port <= 0 {
			continue
		}
		out = append(out, tui.Port{Number: int(port.Port), Protocol: string(port.Protocol)})
	}
	return out
}
