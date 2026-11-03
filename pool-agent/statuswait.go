package poolagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// The pool waits on every running sandbox's ports, so a port a program binds
// reaches the control plane at once rather than at the next 15-second check-in
// (ADR 26-11-03-815 §3). The sandbox agent may not call out (ADR 0030), so the
// pool holds a request open instead: the agent answers it the moment its ports
// change, or with nothing new once its own hold is up, and the waiter asks
// again.
//
// A waiter only learns that something changed. The standing loop is what polls
// the sandbox's status and pushes it, so reports reach the control plane in
// the order they were observed whichever of the two asked for them.
const (
	// sandboxAgentStatusWaitTimeout bounds one wait from this side. The agent
	// answers within its own 25-second hold; this is that plus room for a
	// slow answer, so a dead connection is noticed without cutting a live one.
	sandboxAgentStatusWaitTimeout = 40 * time.Second
	// sandboxAgentStatusWaitRetry is the pause after a wait that failed: the
	// sandbox may be starting, stopping or briefly unreachable. It is short
	// because a box that has just started is the common case, and its agent
	// is up within a second or two.
	sandboxAgentStatusWaitRetry = 2 * time.Second
	// sandboxAgentStatusWaitUnsupportedRetry is the pause after an agent that
	// does not have the route: it predates it, and keeps the 15-second
	// check-in. It is asked again now and then, since an agent can be
	// upgraded in place.
	sandboxAgentStatusWaitUnsupportedRetry = 5 * time.Minute
	// sandboxAgentStatusWaitMinHold is how long a wait naming the current
	// version has to have been held to be taken as one. A quicker answer with
	// that same version is an agent that is not holding, and the waiter backs
	// off rather than spin.
	sandboxAgentStatusWaitMinHold = time.Second
	// sandboxAgentStatusWakeQueue is how many changed sandboxes can wait for
	// the loop. A waiter that finds it full drops its wake: the loop is behind
	// on a burst, and the next tick checks every sandbox in anyway.
	sandboxAgentStatusWakeQueue = 64
)

// errStatusWaitUnsupported is an agent answering the wait with 404, one older
// than the route, or with no version, one with no port watcher.
var errStatusWaitUnsupported = errors.New("sandbox agent does not answer the status wait")

// waitOn keeps exactly one waiter on each of running, starting the ones that
// are missing and stopping the ones for sandboxes no longer running. The tick
// calls it as the backstop; sandboxStateChanged starts and stops a waiter the
// moment the runtime sees a sandbox start or stop.
func (p *sandboxAgentStatusPoller) waitOn(ctx context.Context, running []string) {
	keep := make(map[string]bool, len(running))
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, sandboxID := range running {
		keep[sandboxID] = true
		p.startWaiterLocked(ctx, sandboxID)
	}
	for sandboxID := range p.waiters {
		if !keep[sandboxID] {
			p.stopWaiterLocked(sandboxID)
		}
	}
}

// sandboxStateChanged starts a waiter on a sandbox the runtime has just seen
// running and stops the one on a sandbox it has seen leave running. Starting at
// once rather than at the next tick is what catches a port bound in a new
// box's first seconds — a harness signing in as soon as it starts. ctx is the
// poller's, never a report's.
func (p *sandboxAgentStatusPoller) sandboxStateChanged(ctx context.Context, sandboxID, state string) {
	if p == nil || strings.TrimSpace(sandboxID) == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if state == sandboxruntime.StateRunning {
		p.startWaiterLocked(ctx, sandboxID)
		return
	}
	p.stopWaiterLocked(sandboxID)
}

func (p *sandboxAgentStatusPoller) startWaiterLocked(ctx context.Context, sandboxID string) {
	if _, ok := p.waiters[sandboxID]; ok {
		return
	}
	if p.waiters == nil {
		p.waiters = map[string]context.CancelFunc{}
	}
	waitCtx, cancel := context.WithCancel(ctx)
	p.waiters[sandboxID] = cancel
	go p.waitForPorts(waitCtx, sandboxID)
}

func (p *sandboxAgentStatusPoller) stopWaiterLocked(sandboxID string) {
	if cancel, ok := p.waiters[sandboxID]; ok {
		cancel()
		delete(p.waiters, sandboxID)
	}
}

// waitForPorts waits on one sandbox's ports until ctx ends, waking the loop
// each time they change. The first call names no version and is answered at
// once; each call after names the version last seen and is held until there
// is a newer one.
//
// The first answer wakes the loop too. The waiter may have started with the
// sandbox, before its agent was up, and a port bound while it was getting
// there would otherwise wait for the tick; one extra check-in per waiter is the
// price.
func (p *sandboxAgentStatusPoller) waitForPorts(ctx context.Context, sandboxID string) {
	var version string
	for ctx.Err() == nil {
		token, ok := p.ensureTokens(ctx, []string{sandboxID})[sandboxID]
		if !ok {
			sleepContext(ctx, sandboxAgentStatusWaitRetry)
			continue
		}
		asked := time.Now()
		next, err := p.waitOne(ctx, sandboxID, token, version)
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, errStatusWaitUnsupported):
			version = ""
			sleepContext(ctx, sandboxAgentStatusWaitUnsupportedRetry)
			continue
		case err != nil:
			p.logger.Debug("wait on sandbox-agent ports", "sandboxId", sandboxID, "error", err)
			sleepContext(ctx, sandboxAgentStatusWaitRetry)
			continue
		}
		if next != version {
			select {
			case p.wake <- sandboxID:
			default:
			}
		} else if time.Since(asked) < sandboxAgentStatusWaitMinHold {
			// The version it was named, answered without holding: an agent
			// that is not waiting, whatever the reason. Asking again at once
			// would spin.
			sleepContext(ctx, sandboxAgentStatusWaitRetry)
		}
		version = next
	}
}

// waitOne makes one wait: GET .../status/wait, naming after when there is
// one.
func (p *sandboxAgentStatusPoller) waitOne(ctx context.Context, sandboxID, token, after string) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, sandboxAgentStatusWaitTimeout)
	defer cancel()
	dial, err := p.runtime.SandboxDialer(callCtx, sandboxID, sandboxruntime.SandboxAgentPort)
	if err != nil {
		return "", err
	}
	waitURL := sandboxruntime.HTTPURL(sandboxruntime.SandboxAgentPort, fmt.Sprintf("/api/projects/%s/sandboxes/%s/status/wait", p.bootstrap.ProjectID, sandboxID))
	if after != "" {
		waitURL.RawQuery = url.Values{"after": {after}}.Encode()
	}
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, waitURL.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Transport: dial.Transport()}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusNotFound {
		return "", errStatusWaitUnsupported
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var body struct {
		PortsVersion string `json:"portsVersion"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return "", err
	}
	// An agent with no port watcher has no version to wait on: it is treated
	// as one without the route.
	if body.PortsVersion == "" {
		return "", errStatusWaitUnsupported
	}
	return body.PortsVersion, nil
}

// drainWakes is first and every other sandbox already queued, once each: a
// burst of changes is checked in as one batch.
func (p *sandboxAgentStatusPoller) drainWakes(first string) []string {
	seen := map[string]bool{first: true}
	out := []string{first}
	for {
		select {
		case sandboxID := <-p.wake:
			if !seen[sandboxID] {
				seen[sandboxID] = true
				out = append(out, sandboxID)
			}
		default:
			return out
		}
	}
}
