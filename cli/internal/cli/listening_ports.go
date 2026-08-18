package cli

import (
	"encoding/json"

	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/cli/internal/portforward"
	"github.com/obot-platform/discobox/cli/internal/tui"
)

// sandboxPortTargets is what the sandbox's own processes are serving, as its
// agent last reported it (ADR 0046) — the same push that carries the git state
// and the terminal titles, so a listing can show it for every row without
// reaching into anything, and `disco proxy` can forward it without asking the
// sandbox anything else.
//
// The bind address is kept, because a forward has to name a host to dial:
// the tunnel dials from inside the sandbox's network namespace, and a process
// bound to one specific non-loopback address is no more on 127.0.0.1 there
// than it is here. A listing that only shows the number drops it again.
func sandboxPortTargets(sb apimodel.Sandbox) []portforward.Target {
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
	targets := make([]portforward.Target, 0, len(reported))
	for _, port := range reported {
		if port.Port <= 0 || port.Port > 65535 {
			continue
		}
		targets = append(targets, portforward.Target{
			Host:     dialHostForPort(port.Addresses),
			Port:     int(port.Port),
			Protocol: string(port.Protocol),
		})
	}
	return targets
}

// dialHostForPort picks the address the tunnel dials. A wildcard or loopback
// bind is reached on loopback, which is the one address a process is sure to
// answer on; anything else is dialed at the address it actually bound.
func dialHostForPort(addresses []string) string {
	for _, address := range addresses {
		switch address {
		case "0.0.0.0", "::", "[::]", "*", "127.0.0.1", "::1", "[::1]", "localhost":
			return portforward.DefaultDialHost
		}
	}
	for _, address := range addresses {
		if address != "" {
			return address
		}
	}
	return portforward.DefaultDialHost
}

// sandboxListeningPorts is the same listing narrowed to what fits beside a
// sandbox on one line: the number and what it speaks.
func sandboxListeningPorts(sb apimodel.Sandbox) []tui.Port {
	targets := sandboxPortTargets(sb)
	out := make([]tui.Port, 0, len(targets))
	for _, target := range targets {
		out = append(out, tui.Port{Number: target.Port, Protocol: target.Protocol})
	}
	return out
}
