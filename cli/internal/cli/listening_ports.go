package cli

import (
	"encoding/json"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/portforward"
	"github.com/discobox-ai/discobox/cli/internal/tui"
)

// sandboxPortTargets is what the sandbox is serving, as its agent last reported
// it — the ports its own processes were seen listening on (ADR 0046) and the
// ones its services declare (ADR 0076). It rides the same push that carries the
// git state and the terminal titles, so a listing can show it for every row
// without reaching into anything, and `discobox proxy` can forward it without
// asking the sandbox anything else.
//
// The bind address is kept, because a forward has to name a host to dial:
// the tunnel dials from inside the sandbox's network namespace, and a process
// bound to one specific non-loopback address is no more on 127.0.0.1 there
// than it is here. A listing that only shows the number drops it again. A
// declared port reports no address — nothing visible is bound on it to report —
// and so is dialed at the default host, which is the right answer for one
// published by a nested container or a socket-activated unit.
//
// A UDP port is its own target, beside the TCP port of the same number if
// there is one (ADR 0109 §3).
func sandboxPortTargets(sb apimodel.Sandbox) []portforward.Target {
	reported := reportedPorts(sb)
	targets := make([]portforward.Target, 0, len(reported))
	for _, port := range reported {
		if port.Port <= 0 || port.Port > 65535 {
			continue
		}
		network := portNetwork(string(port.Protocol))
		targets = append(targets, portforward.Target{
			Network:  network,
			Host:     dialHostForPort(port.Addresses, network),
			Port:     int(port.Port),
			Protocol: string(port.Protocol),
		})
	}
	return targets
}

// portNetwork is the transport a reported port is forwarded over. The report
// says it through the protocol: udp is the one value that is not a TCP port
// (ADR 0109 §3), which is also what an agent older than UDP discovery never
// sends.
func portNetwork(protocol string) portforward.Network {
	if protocol == "udp" {
		return portforward.UDP
	}
	return portforward.TCP
}

// dialHostForPort picks the host the tunnel dials. A wildcard or loopback bind
// is reached on loopback — for TCP as a name, so both loopback families are
// tried, since a v6-only listener refuses 127.0.0.1 outright; anything else is
// dialed at the address it actually bound.
//
// UDP cannot use the name. A TCP dial of "localhost" tries each address until
// one connects, but connecting a UDP socket sends nothing and so never fails:
// whichever family resolved first would be the one used, right or not. So a
// UDP port is dialed at a literal loopback address in a family it is bound in.
func dialHostForPort(addresses []string, network portforward.Network) string {
	if network == portforward.UDP {
		return udpDialHost(addresses)
	}
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

// udpDialLoopback is where a UDP port with nothing better to go on is dialed:
// IPv4 loopback, which a wildcard IPv6 socket that is not v6-only answers too.
const udpDialLoopback = "127.0.0.1"

// udpDialHost is dialHostForPort for a UDP port: IPv4 loopback for an IPv4
// wildcard or loopback bind, IPv6 loopback for an IPv6-only one, and the
// address itself for anything else. A declared port with no address is dialed
// at IPv4 loopback, as the sandbox's own probe would be.
func udpDialHost(addresses []string) string {
	var v6Loopback bool
	for _, address := range addresses {
		switch address {
		case "0.0.0.0", "127.0.0.1":
			return udpDialLoopback
		case "::", "[::]", "::1", "[::1]":
			v6Loopback = true
		}
	}
	if v6Loopback {
		return "::1"
	}
	for _, address := range addresses {
		if address != "" {
			return address
		}
	}
	return udpDialLoopback
}

// sandboxListeningPorts is the same listing narrowed to what fits beside a
// sandbox on one line: the number and what it speaks.
func sandboxListeningPorts(sb apimodel.Sandbox) []tui.Port {
	reported := reportedPorts(sb)
	out := make([]tui.Port, 0, len(reported))
	for _, port := range reported {
		if port.Port <= 0 || port.Port > 65535 {
			continue
		}
		// Read from the report rather than from the forward targets: a target
		// is a host and a port, which is all a tunnel needs, and the service a
		// port came from is exactly the part it has no use for. The header
		// does.
		out = append(out, tui.Port{
			Number:      int(port.Port),
			UDP:         portNetwork(string(port.Protocol)) == portforward.UDP,
			Protocol:    string(port.Protocol),
			ServiceID:   port.ServiceId.Or(""),
			ServiceName: port.ServiceName.Or(""),
		})
	}
	return out
}

// reportedPorts is the agent's last port report, or nothing when it has not
// made one. Both the forward and the header are drawn from it, so they can
// never disagree about what the sandbox is serving.
func reportedPorts(sb apimodel.Sandbox) []apimodel.SandboxAgentListeningPort {
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
	return reported
}
