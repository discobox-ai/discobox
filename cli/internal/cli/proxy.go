package cli

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/spf13/cobra"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	apimodel "github.com/obot-platform/discobox/api/model"
	"github.com/obot-platform/discobox/cli/internal/portforward"
)

// proxyPollInterval is how often the sandbox is asked what it is listening on.
// The listing is pushed to the control plane by the sandbox-agent's own
// watcher (ADR 0046), so this only bounds how late a new port shows up here.
const proxyPollInterval = 2 * time.Second

type proxyOptions struct {
	address  string
	interval time.Duration
	ports    []int
}

// newProxyCommand implements `disco proxy`: hold a local port open for every
// port the sandbox is listening on, for as long as the command runs.
func (a *App) newProxyCommand() *cobra.Command {
	var opts proxyOptions
	cmd := &cobra.Command{
		Use:   "proxy [SANDBOX_ID]",
		Short: "Forward a sandbox's listening ports to local ports",
		Long: `Forward every port a sandbox is listening on to a local port.

Without SANDBOX_ID the sandbox is taken from the ones "disco ls" shows for the
current project directory: the only one when there is one, otherwise you are
asked to pick.

The sandbox reports what its own processes are serving, and each port is bound
locally at the same number when it is free and at the nearest one above it when
it is not — a sandbox serving 8080 is http://localhost:8081 when something else
already has 8080. Ports that appear while the command runs are bound as they
appear, and the command prints every bind and every connection it forwards.

--port narrows that to the ports you name, and forwards them whether or not the
sandbox has reported them yet — the report is a poll behind, and a port you just
started is one you want now.

A local port stays bound once it has been given out, even if what was behind it
in the sandbox restarts and drops off the listing for a moment, so a URL you
have open keeps working. Forwarding stops when the command does.`,
		Example: `  disco proxy
  disco proxy sbx_01hq
  disco proxy --port 8080 --port 5432
  disco proxy --address 0.0.0.0`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: a.completeSandboxes,
		RunE: func(cmd *cobra.Command, args []string) error {
			var sandboxArg string
			if len(args) > 0 {
				sandboxArg = args[0]
			}
			projectID, sandboxID, client, err := a.selectSandbox(cmd, sandboxArg)
			if err != nil {
				return err
			}
			return a.runProxy(cmd.Context(), client, projectID, sandboxID, opts, cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&opts.address, "address", portforward.DefaultBindAddress, "Local address to bind forwarded ports on")
	cmd.Flags().DurationVar(&opts.interval, "interval", proxyPollInterval, "How often to ask the sandbox what it is listening on")
	cmd.Flags().IntSliceVar(&opts.ports, "port", nil, "Sandbox port to forward whether or not it has been reported; repeatable, and forwards every reported port when unset")
	return cmd
}

func (a *App) runProxy(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string, opts proxyOptions, status io.Writer) error {
	dialer, err := a.sandboxTCPDialer(projectID, sandboxID)
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
		if err == nil || len(opts.ports) > 0 {
			forwarder.Set(proxyTargets(reported, opts.ports))
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
func proxyTargets(reported []portforward.Target, requested []int) []portforward.Target {
	if len(requested) == 0 {
		return reported
	}
	byPort := make(map[int]portforward.Target, len(reported))
	for _, target := range reported {
		byPort[target.Port] = target
	}
	out := make([]portforward.Target, 0, len(requested))
	for _, port := range requested {
		if target, ok := byPort[port]; ok {
			out = append(out, target)
			continue
		}
		out = append(out, portforward.Target{Host: portforward.DefaultDialHost, Port: port})
	}
	return out
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
