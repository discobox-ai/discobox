package irohd

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/endpoint"
)

// ListenerWatch keeps an eye on this server's iroh listener and reports what it
// sees, both to the log when it changes and to whoever asks.
//
// It exists because a server whose iroh listener has stopped working is
// indistinguishable, from outside, from one whose listener is fine: the process
// is up, its unix socket answers, /healthz says ready, and every client dialing
// its peer ID times out. The transport was reported once at startup and never
// again, so a relay that went away an hour later went away in silence.
//
// The watch is a poll rather than a subscription because that is what the
// transport offers. It is cheap: a bounded wait on a relay the endpoint either
// has or does not.
type ListenerWatch struct {
	interval time.Duration
	probe    time.Duration

	mu    sync.Mutex
	state endpoint.IrohListenerState
	// since is when the current Online value was first seen, so a report can
	// say how long a listener has been off its relay rather than only that it
	// is. "No relay for 4m" is a different sentence from "no relay", and only
	// one of them tells an operator whether it lines up with what they are
	// seeing.
	since   time.Time
	read    bool
	lastErr error
}

const (
	defaultListenerWatchInterval = 15 * time.Second
	// listenerProbeWait bounds the relay check. Short on purpose: this asks
	// whether a relay is there right now, and a probe that waits is a probe
	// that reports the past.
	listenerProbeWait = 2 * time.Second
)

// NewListenerWatch creates a watch. Start runs it.
func NewListenerWatch() *ListenerWatch {
	return &ListenerWatch{interval: defaultListenerWatchInterval, probe: listenerProbeWait}
}

// Start polls until ctx ends. It reports the first reading immediately so a
// server that comes up without a relay says so at startup rather than one
// interval later.
func (w *ListenerWatch) Start(ctx context.Context) {
	go func() {
		w.sample(ctx, true)
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				w.sample(ctx, false)
			}
		}
	}()
}

// State is the most recent reading, and whether there has been one.
func (w *ListenerWatch) State() (endpoint.IrohListenerState, time.Time, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state, w.since, w.read
}

func (w *ListenerWatch) sample(ctx context.Context, first bool) {
	probeCtx, cancel := context.WithTimeout(ctx, w.probe)
	state, err := endpoint.LocalIrohListenerState(probeCtx)
	cancel()

	w.mu.Lock()
	was := w.state
	wasRead := w.read
	w.state = state
	w.lastErr = err
	w.read = true
	changed := !wasRead || was.Online != state.Online
	if changed {
		w.since = time.Now()
	}
	since := w.since
	w.mu.Unlock()

	if err != nil {
		log.Printf("iroh: this server's listener could not be read: %v", err)
		return
	}
	if !changed {
		return
	}
	switch {
	case state.Online && first:
		log.Printf("iroh: reachable through relay %s", state.HomeRelay)
	case state.Online:
		log.Printf("iroh: back on a relay (%s) after %s without one",
			state.HomeRelay, time.Since(since).Round(time.Second))
	case first:
		log.Printf("iroh: no relay; this server is reachable only from networks that can route to it directly (%s)",
			describeSockets(state))
	default:
		// The transition that used to happen in silence, and the one an
		// operator is looking for when clients stop being able to dial in.
		log.Printf("iroh: lost its relay; until it returns this server is reachable only from networks that can route to it directly (%s)",
			describeSockets(state))
	}
}

func describeSockets(state endpoint.IrohListenerState) string {
	if len(state.DirectAddrs) == 0 {
		return "no direct addresses"
	}
	return "direct addresses " + strings.Join(state.DirectAddrs, " ")
}
