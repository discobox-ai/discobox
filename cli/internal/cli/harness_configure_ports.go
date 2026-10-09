package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
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
// A numbered port is a static target: the callback server inside the sandbox
// comes up only when the user picks the browser sign-in, and the local port
// has to be there already when it does. An ephemeral entry has no number to
// hold ahead of time — Copilot's sign-in listens on a port it picks each run —
// so the forward follows the sandbox's port listing instead, as the workspace
// does, and binds each TCP port it finds at that same number. Still exact: the
// browser was handed a redirect URI naming the sandbox's number, so a forward
// moved to the next free one would answer nothing.
func (a *App) forwardConfigurePorts(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string, ports []apimodel.HarnessConfigPort, status io.Writer) (io.Closer, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	dialer, err := a.sandboxPortDialer(projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	list := func(ctx context.Context) ([]portforward.Target, error) {
		return fetchSandboxPortTargets(ctx, client, projectID, sandboxID)
	}
	return startConfigureForward(ctx, dialer, list, proxyPollInterval, ports, status), nil
}

// configureForward is the forward a configure flow holds open: the forwarder,
// and the listing poll that feeds it when an ephemeral port is declared.
type configureForward struct {
	forwarder *portforward.Forwarder
	cancel    context.CancelFunc
	following sync.WaitGroup
}

func (f *configureForward) Close() error {
	f.cancel()
	f.following.Wait()
	return f.forwarder.Close()
}

// startConfigureForward is forwardConfigurePorts after the sandbox's dialer
// and listing are in hand.
func startConfigureForward(ctx context.Context, dialer portforward.Dialer, list func(context.Context) ([]portforward.Target, error),
	interval time.Duration, ports []apimodel.HarnessConfigPort, status io.Writer,
) *configureForward {
	fixed := map[int]bool{}
	var ephemeral *apimodel.HarnessConfigPort
	var targets []portforward.Target
	for i, port := range ports {
		if port.Ephemeral.Or(false) {
			ephemeral = &ports[i]
			continue
		}
		number := int(port.Port)
		fixed[number] = true
		targets = append(targets, portforward.Target{Port: number})
	}

	// A discovered port that cannot be bound is only learned of mid-flow, from
	// the forwarder's own goroutines; a numbered one is reported below, once,
	// before the flow starts. Mid-flow the configure terminal holds the user's
	// terminal raw, as attachSandboxTerminal always does, so the warning ends
	// its lines the way a raw terminal needs and starts on a line of its own.
	var statusMu sync.Mutex
	observe := func(event portforward.Event) {
		if ephemeral == nil || event.Kind != portforward.BindFailed || fixed[event.Target.Port] {
			return
		}
		statusMu.Lock()
		defer statusMu.Unlock()
		fmt.Fprintf(status, "\r\nwarning: %s\r\n", discoveredPortUnavailable(*ephemeral, event.Target.Port))
	}

	ctx, cancel := context.WithCancel(ctx)
	forward := &configureForward{
		forwarder: portforward.New(ctx, portforward.Options{Dialer: dialer, Exact: true, Observe: observe}),
		cancel:    cancel,
	}
	forward.forwarder.Set(targets)

	bound := map[int]bool{}
	for _, binding := range forward.forwarder.Bindings() {
		bound[binding.Target.Port] = true
	}
	statusMu.Lock()
	if forwarded := forwardedConfigurePorts(ports, bound); forwarded != "" {
		fmt.Fprintf(status, "Forwarding %s into the configure discobox\n", forwarded)
	}
	if ephemeral != nil {
		fmt.Fprintln(status, "Forwarding each port the configure discobox listens on, at the same number")
	}
	for _, port := range ports {
		if !port.Ephemeral.Or(false) && !bound[int(port.Port)] {
			fmt.Fprintf(status, "warning: %s\n", configPortUnavailable(port))
		}
	}
	statusMu.Unlock()

	if ephemeral != nil {
		forward.following.Add(1)
		go func() {
			defer forward.following.Done()
			followConfigurePorts(ctx, forward.forwarder, list, interval, targets)
		}()
	}
	return forward
}

// followConfigurePorts keeps the forwarder on the numbered ports plus every TCP
// port the sandbox is discovered listening on, until ctx ends. A failed listing
// is skipped, as the workspace's is: the ports already bound are still good.
//
// Only TCP: a browser's callback is an HTTP request, and binding the sandbox's
// UDP ports at their own numbers would take them from this machine for nothing.
// Only discovered ports: the listing also carries the image's declared
// services (the desktop on 6900), which no sign-in redirects to, and binding
// those would take them from this machine — and warn that the sign-in cannot
// complete when the workspace's own forward already holds one.
func followConfigurePorts(ctx context.Context, forwarder *portforward.Forwarder, list func(context.Context) ([]portforward.Target, error),
	interval time.Duration, fixed []portforward.Target,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if listed, err := list(ctx); err == nil {
			targets := append([]portforward.Target(nil), fixed...)
			for _, target := range listed {
				if target.ServiceID == "" && (target.Network == "" || target.Network == portforward.TCP) {
					targets = append(targets, target)
				}
			}
			forwarder.Set(targets)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// forwardedConfigurePorts names the declared ports that were bound, as the
// addresses a browser will be sent to.
func forwardedConfigurePorts(ports []apimodel.HarnessConfigPort, bound map[int]bool) string {
	var addresses []string
	for _, port := range ports {
		if number := int(port.Port); !port.Ephemeral.Or(false) && bound[number] {
			addresses = append(addresses, net.JoinHostPort("localhost", strconv.Itoa(number)))
		}
	}
	return strings.Join(addresses, ", ")
}

// configPortUnavailable is what to tell the user about a numbered port that
// could not be bound here: the image's own words, which say what to do
// instead, or failing those the fact. An image's words are escaped like any
// text from inside a discobox: the warning can reach a raw terminal.
func configPortUnavailable(port apimodel.HarnessConfigPort) string {
	if message := strings.TrimSpace(port.Unavailable.Or("")); message != "" {
		return terminalSafe(message)
	}
	return fmt.Sprintf("port %d is already in use on this machine, so it was not forwarded into the configure discobox", port.Port)
}

// discoveredPortUnavailable is what to tell the user about a port an ephemeral
// entry found the sandbox listening on that could not be bound here. The image
// cannot name the number, so this does, and the image's words follow as what
// to do about it.
func discoveredPortUnavailable(port apimodel.HarnessConfigPort, number int) string {
	fact := fmt.Sprintf("port %d is already in use on this machine, so it was not forwarded into the configure discobox", number)
	if advice := strings.TrimSpace(port.Unavailable.Or("")); advice != "" {
		return fact + ". " + terminalSafe(advice)
	}
	return fact
}
