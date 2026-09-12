package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/portforward"
)

// proxyPollInterval is how often the sandbox is asked what it is listening on.
// The listing is pushed to the control plane by the sandbox-agent's own
// watcher (ADR 0046), so this only bounds how late a new port shows up here.
const proxyPollInterval = 2 * time.Second

type proxyOptions struct {
	address  string
	interval time.Duration
	ports    []string
}

// newProxyCommand implements `discobox proxy`: hold a local port open for every
// port the sandbox is listening on, for as long as the command runs.
func (a *App) newProxyCommand() *cobra.Command {
	var opts proxyOptions
	cmd := &cobra.Command{
		Use:   "proxy [DISCOBOX_ID]",
		Short: "Forward a discobox's listening ports to local ports",
		Long: `Forward every port a discobox is listening on to a local port — its TCP
ports, and the UDP ports its processes have bound.

Without DISCOBOX_ID the discobox is taken from the ones "discobox ls" shows for the
current project directory: the only one when there is one, otherwise you are
asked to pick.

The discobox reports what its own processes are serving, and each port is bound
locally at the same number when it is free and at the nearest one above it when
it is not — a discobox serving 8080 is http://localhost:8081 when something else
already has 8080. Ports that appear while the command runs are bound as they
appear, and the command prints every bind and every connection it forwards.

A UDP port is bound locally by the same rule, independently of the TCP port of
the same number. Each local address that sends it datagrams gets its own tunnel,
closed after a minute with nothing sent either way.

--port narrows that to the ports you name, and forwards them whether or not the
discobox has reported them yet — the report is a poll behind, and a port you just
started is one you want now. A bare number is a TCP port; write 5353/udp for a
UDP one.

A local port stays bound once it has been given out, even if what was behind it
in the discobox restarts and drops off the listing for a moment, so a URL you
have open keeps working. Forwarding stops when the command does.`,
		Example: `  discobox proxy
  discobox proxy sbx_01hq
  discobox proxy --port 8080 --port 5432
  discobox proxy --port 5353/udp
  discobox proxy --address 0.0.0.0`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			requested, err := parseProxyPorts(opts.ports)
			if err != nil {
				return err
			}
			var sandboxArg string
			if len(args) > 0 {
				sandboxArg = args[0]
			}
			// Forwarded from the server the discobox is on.
			a, projectID, sandboxID, client, err := a.selectSandbox(cmd, sandboxArg)
			if err != nil {
				return err
			}
			return a.runProxy(cmd.Context(), client, projectID, sandboxID, opts, requested, cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&opts.address, "address", portforward.DefaultBindAddress, "Local address to bind forwarded ports on")
	cmd.Flags().DurationVar(&opts.interval, "interval", proxyPollInterval, "How often to ask the discobox what it is listening on")
	cmd.Flags().StringSliceVar(&opts.ports, "port", nil, "Discobox port to forward whether or not it has been reported, as 8080 or 5353/udp; repeatable, and forwards every reported port when unset")
	return cmd
}

func (a *App) runProxy(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string, opts proxyOptions, requested []portforward.Target, status io.Writer) error {
	dialer, err := a.sandboxPortDialer(projectID, sandboxID)
	if err != nil {
		return err
	}

	// Events arrive from every accept loop and every forwarded connection at
	// once; the writer behind them is one terminal.
	var writeMu sync.Mutex
	forwarder := portforward.New(ctx, portforward.Options{
		Dialer:      dialer,
		BindAddress: opts.address,
		Observe: func(event portforward.Event) {
			writeMu.Lock()
			defer writeMu.Unlock()
			fmt.Fprintln(status, event)
		},
	})
	defer forwarder.Close()

	fmt.Fprintf(status, "Forwarding ports from %s (Ctrl-C to stop)\n", sandboxID)

	interval := opts.interval
	if interval <= 0 {
		interval = proxyPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		reported, err := fetchSandboxPortTargets(ctx, client, projectID, sandboxID)
		if err != nil {
			// The sandbox is asked again on the next tick. A listing that
			// failed is not a reason to drop the ports already bound: the
			// tunnels through them are still good, and named ports do not
			// depend on the listing at all.
			writeMu.Lock()
			fmt.Fprintf(status, "listing ports: %v\n", err)
			writeMu.Unlock()
		}
		if err == nil || len(requested) > 0 {
			forwarder.Set(proxyTargets(reported, requested))
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// proxyTargets is what to forward: everything the sandbox reports, or exactly
// the ports --port named.
//
// A named port is forwarded whether or not it has been reported yet. Naming
// one is a statement that it is there, and the report is behind by up to the
// agent's own poll (ADR 0046) — waiting for the listing to agree would make
// the flag useless in the minute after a server starts, which is the minute
// someone reaches for it. What is reported about a named port is still used:
// it carries the address to dial and what the port speaks.
func proxyTargets(reported, requested []portforward.Target) []portforward.Target {
	if len(requested) == 0 {
		return reported
	}
	type key struct {
		network portforward.Network
		port    int
	}
	byPort := make(map[key]portforward.Target, len(reported))
	for _, target := range reported {
		byPort[key{target.Network, target.Port}] = target
	}
	out := make([]portforward.Target, 0, len(requested))
	for _, named := range requested {
		if target, ok := byPort[key{named.Network, named.Port}]; ok {
			out = append(out, target)
			continue
		}
		out = append(out, named)
	}
	return out
}

// parseProxyPorts reads --port: a port number for a TCP port, and the number
// with a /udp or /tcp suffix to say which, the way a Docker publish does. Each
// comes back as a target with the host a port nothing has reported is dialed
// at.
func parseProxyPorts(values []string) ([]portforward.Target, error) {
	out := make([]portforward.Target, 0, len(values))
	for _, value := range values {
		number, suffix, _ := strings.Cut(strings.TrimSpace(value), "/")
		network := portforward.TCP
		switch strings.ToLower(suffix) {
		case "", "tcp":
		case "udp":
			network = portforward.UDP
		default:
			return nil, fmt.Errorf("--port %q: %q is not tcp or udp", value, suffix)
		}
		port, err := strconv.Atoi(number)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("--port %q: %q is not a port number", value, number)
		}
		out = append(out, portforward.Target{Network: network, Host: dialHostForPort(nil, network), Port: port})
	}
	return out, nil
}

// fetchSandboxPortTargets reads the sandbox and returns what it says its own
// processes are serving, as forward targets.
func fetchSandboxPortTargets(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string) ([]portforward.Target, error) {
	res, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return nil, err
	}
	sandbox, err := expectResponse[apimodel.Sandbox](res)
	if err != nil {
		return nil, err
	}
	if sandbox == nil {
		return nil, nil
	}
	return sandboxPortTargets(*sandbox), nil
}
