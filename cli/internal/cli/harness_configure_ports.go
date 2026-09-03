package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/portforward"
)

// forwardConfigurePorts binds the ports a harness's configure flow declares
// (harness.ConfigPort) on this machine and forwards them into the configure
// sandbox, for as long as the returned forward is open.
//
// It is the workspace's port forward with one difference: the local number is
// not this side's to choose. A declared port is where the harness's sign-in
// sends the user's browser back to — Codex's ChatGPT login redirects to
// localhost:1455 and nowhere else — so it is bound exactly or not at all
// (portforward.Options.Exact), and a port that could not be bound is reported
// on status in the words the image chose for it, since only the image knows
// what its harness can still do without the callback.
//
// The forward is opened before the sandbox is waited for. The wait is minutes
// behind a cold image pull, and both things the bind decides — that the port
// is held for the flow, and that the user hears it is not — are better decided
// before that wait than after it.
//
// The targets are static: nothing polls the sandbox's listing, because the
// listing is not what decides these ports. The callback server inside the
// sandbox comes up only when the user picks the browser sign-in, and the local
// port has to be there already when it does.
func (a *App) forwardConfigurePorts(ctx context.Context, projectID, sandboxID string, ports []apimodel.HarnessConfigPort, status io.Writer) (io.Closer, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	dialer, err := a.sandboxTCPDialer(projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	forwarder := portforward.New(ctx, portforward.Options{Dialer: dialer, Exact: true})
	targets := make([]portforward.Target, 0, len(ports))
	for _, port := range ports {
		targets = append(targets, portforward.Target{Port: int(port.Port)})
	}
	forwarder.Set(targets)

	bound := map[int]bool{}
	for _, binding := range forwarder.Bindings() {
		bound[binding.Target.Port] = true
	}
	if forwarded := forwardedConfigurePorts(ports, bound); forwarded != "" {
		fmt.Fprintf(status, "Forwarding %s into the configure discobox\n", forwarded)
	}
	for _, port := range ports {
		if !bound[int(port.Port)] {
			fmt.Fprintf(status, "warning: %s\n", configPortUnavailable(port))
		}
	}
	return forwarder, nil
}

// forwardedConfigurePorts names the declared ports that were bound, as the
// addresses a browser will be sent to.
func forwardedConfigurePorts(ports []apimodel.HarnessConfigPort, bound map[int]bool) string {
	var addresses []string
	for _, port := range ports {
		if bound[int(port.Port)] {
			addresses = append(addresses, net.JoinHostPort("localhost", strconv.Itoa(int(port.Port))))
		}
	}
	return strings.Join(addresses, ", ")
}

// configPortUnavailable is what to tell the user about a declared port that
// could not be bound here: the image's own words, which say what to do
// instead, or failing those the fact.
func configPortUnavailable(port apimodel.HarnessConfigPort) string {
	if message := strings.TrimSpace(port.Unavailable.Or("")); message != "" {
		return message
	}
	return fmt.Sprintf("port %d is already in use on this machine, so it was not forwarded into the configure discobox", port.Port)
}
