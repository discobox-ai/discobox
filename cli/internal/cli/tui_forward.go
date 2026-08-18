package cli

import (
	"context"
	"sync"
	"time"

	apiclientgen "github.com/obot-platform/discobox/api/gen"
	"github.com/obot-platform/discobox/cli/internal/portforward"
	"github.com/obot-platform/discobox/cli/internal/tui"
)

// Forward starts the launcher's port forward: the same forwarder `disco proxy`
// runs, over the same tunnel, with the command's printed status replaced by a
// wake-up for the window that draws it.
//
// The launcher gets a forward per open workspace rather than one for the
// project. A forward is bound local ports, and holding them for sandboxes
// nobody is looking at would take numbers from the machine for no one's
// benefit — the workspace is where the ports are shown, so it is where they
// are held.
func (d *apiDataSource) Forward(ctx context.Context, sandboxID string) (tui.Forward, error) {
	dialer, err := d.app.sandboxTCPDialer(d.projectID, sandboxID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	forward := &tuiForward{
		cancel:  cancel,
		changed: make(chan struct{}, 1),
	}
	// Loopback, and not the --address a command can widen: a window has no
	// place opening a sandbox's ports to the network on the strength of having
	// been attached to.
	forward.forwarder = portforward.New(ctx, portforward.Options{
		Dialer:  dialer,
		Observe: forward.observe,
	})
	go forward.follow(ctx, d.client, d.projectID, sandboxID)
	return forward, nil
}

// tuiForward adapts a portforward.Forwarder to the launcher's Forward seam.
type tuiForward struct {
	forwarder *portforward.Forwarder
	cancel    context.CancelFunc

	// changed is the window's wake-up, buffered by one and sent to without
	// blocking: the window redraws from Bindings, so a second signal queued
	// behind an unread first one would buy it nothing.
	changed chan struct{}
	// mu orders a send against the close, since the forwarder's own goroutines
	// keep reporting until it has finished shutting down.
	mu     sync.RWMutex
	closed bool
}

func (f *tuiForward) Bindings() []tui.Binding {
	bound := f.forwarder.Bindings()
	bindings := make([]tui.Binding, 0, len(bound))
	for _, binding := range bound {
		bindings = append(bindings, tui.Binding{Port: binding.Target.Port, Local: binding.Local})
	}
	return bindings
}

func (f *tuiForward) Events() <-chan struct{} { return f.changed }

func (f *tuiForward) Close() error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return nil
	}
	f.closed = true
	close(f.changed)
	f.mu.Unlock()

	f.cancel()
	return f.forwarder.Close()
}

// observe wakes the window for the events that change what it draws, and only
// those. A forwarded connection being accepted and ending is most of what a
// busy page produces and none of it is on screen, so waking on it would repaint
// the window once per request for nothing.
func (f *tuiForward) observe(event portforward.Event) {
	if event.Kind != portforward.Bound {
		return
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.closed {
		return
	}
	select {
	case f.changed <- struct{}{}:
	default:
	}
}

// follow keeps the forwarder's targets on what the sandbox announces, until the
// workspace closes the forward.
//
// A listing that failed is not reported. The window has nowhere to put an error
// that recurs on a timer, the ports already bound are still good, and the
// workspace's exec poll swallows its own listing errors for the same reason.
func (f *tuiForward) follow(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string) {
	ticker := time.NewTicker(proxyPollInterval)
	defer ticker.Stop()
	for {
		if targets, err := fetchSandboxPortTargets(ctx, client, projectID, sandboxID); err == nil {
			f.forwarder.Set(targets)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
