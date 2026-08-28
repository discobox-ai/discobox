package poolagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

const (
	// sandboxAgentStatusPollInterval is how often the standing poller checks
	// every hosted sandbox's status and pushes a batch to the control plane.
	// Distinct from sandboxruntime's 100ms poll interval, which is a one-shot
	// readiness wait, not a standing loop.
	sandboxAgentStatusPollInterval = 15 * time.Second
	sandboxAgentStatusCallTimeout  = 5 * time.Second
	// sandboxAgentStatusTokenRefreshMargin mints a fresh token once less than
	// this much of its TTL remains, well inside the 15-minute TTL the control
	// plane issues.
	sandboxAgentStatusTokenRefreshMargin = 5 * time.Minute
)

// startSandboxAgentStatusPoller runs the standing loop that polls every
// hosted sandbox's sandbox-agent status endpoint and pushes a batch of
// results to the control plane (ADR 0030). It never affects sandbox
// lifecycle: a poll or push failure is logged and skipped for this tick,
// never treated as a signal to stop or recreate a sandbox.
//
// It returns the poller so the resource reporter can read the counters it
// collects, or nil when there is nothing to poll.
func startSandboxAgentStatusPoller(ctx context.Context, logger *slog.Logger, bootstrap Bootstrap, registration *Registration, runtime sandboxruntime.Runtime, client SandboxAgentStatusClient) *sandboxAgentStatusPoller {
	if runtime == nil || client == nil || registration == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	poller := &sandboxAgentStatusPoller{
		logger:       logger,
		bootstrap:    bootstrap,
		registration: registration,
		runtime:      runtime,
		client:       client,
		tokens:       map[string]cachedSandboxAgentToken{},
		samples:      map[string]sandboxResourceSample{},
	}
	go func() {
		ticker := time.NewTicker(sandboxAgentStatusPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				poller.tick(ctx)
			}
		}
	}()
	return poller
}

type cachedSandboxAgentToken struct {
	token     string
	expiresAt time.Time
}

type sandboxAgentStatusPoller struct {
	logger       *slog.Logger
	bootstrap    Bootstrap
	registration *Registration
	runtime      sandboxruntime.Runtime
	client       SandboxAgentStatusClient

	mu     sync.Mutex
	tokens map[string]cachedSandboxAgentToken
	// samples is the newest resource counters seen for each sandbox, handed to
	// the resource reporter so the two loops share one poll of each sandbox
	// rather than each making its own (ADR 0071 §2).
	samples map[string]sandboxResourceSample
}

// ResourceSamples is the newest counters this poller has seen, by sandbox ID.
// The map is a copy, so the caller can hold it across ticks.
func (p *sandboxAgentStatusPoller) ResourceSamples() map[string]sandboxResourceSample {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]sandboxResourceSample, len(p.samples))
	for id, sample := range p.samples {
		out[id] = sample
	}
	return out
}

// tick polls every currently-running hosted sandbox independently, so one
// unreachable or erroring sandbox never blocks or fails the batch, then
// pushes whatever was successfully collected this tick.
func (p *sandboxAgentStatusPoller) tick(ctx context.Context) {
	sandboxes, err := p.runtime.ListSandboxes(ctx)
	if err != nil {
		p.logger.Warn("list sandboxes for status poll", "error", err)
		return
	}
	var running []string
	for _, sb := range sandboxes {
		if sb != nil && sb.Status == sandboxruntime.StatusRunning && strings.TrimSpace(sb.SandboxID) != "" {
			running = append(running, sb.SandboxID)
		}
	}
	if len(running) == 0 {
		return
	}
	tokens := p.ensureTokens(ctx, running)

	var mu sync.Mutex
	var wg sync.WaitGroup
	entries := make([]SandboxAgentStatusEntry, 0, len(running))
	for _, sandboxID := range running {
		token, ok := tokens[sandboxID]
		if !ok {
			continue
		}
		wg.Add(1)
		go func(sandboxID, token string) {
			defer wg.Done()
			entry, err := p.pollOne(ctx, sandboxID, token)
			if err != nil {
				p.logger.Warn("poll sandbox-agent status", "sandboxId", sandboxID, "error", err)
				return
			}
			mu.Lock()
			entries = append(entries, entry)
			mu.Unlock()
			if entry.Resources != nil {
				p.mu.Lock()
				p.samples[sandboxID] = sandboxResourceSample{Usage: *entry.Resources}
				p.mu.Unlock()
			}
		}(sandboxID, token)
	}
	wg.Wait()

	if len(entries) == 0 {
		return
	}
	if err := p.client.ReportSandboxAgentStatus(ctx, SandboxAgentStatusReportRequest{
		ControlPlaneURL: p.bootstrap.ControlPlaneURL,
		ProjectID:       p.bootstrap.ProjectID,
		PoolID:          p.bootstrap.PoolID,
		PrivateKey:      p.registration.PrivateKey,
		Sandboxes:       entries,
	}); err != nil {
		p.logger.Warn("push sandbox-agent status batch", "error", err)
	}
}

func (p *sandboxAgentStatusPoller) pollOne(ctx context.Context, sandboxID, token string) (SandboxAgentStatusEntry, error) {
	callCtx, cancel := context.WithTimeout(ctx, sandboxAgentStatusCallTimeout)
	defer cancel()
	base, err := p.runtime.HTTPBaseURL(callCtx, sandboxID, sandboxruntime.SandboxAgentPort)
	if err != nil {
		return SandboxAgentStatusEntry{}, err
	}
	statusURL := *base
	statusURL.Path = fmt.Sprintf("/api/projects/%s/sandboxes/%s/status", p.bootstrap.ProjectID, sandboxID)
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, statusURL.String(), nil)
	if err != nil {
		return SandboxAgentStatusEntry{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return SandboxAgentStatusEntry{}, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return SandboxAgentStatusEntry{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return SandboxAgentStatusEntry{}, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	// Two fields are read out of the payload; the payload itself is still
	// relayed as received below, never re-serialized from a parsed structure:
	// a field this agent does not model still reaches the control plane.
	// observedAt orders the report, and resources
	// carries the cumulative counters the resource reporter differences.
	var decoded struct {
		ObservedAt time.Time                           `json:"observedAt"`
		Resources  *apimodel.SandboxAgentResourceUsage `json:"resources"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return SandboxAgentStatusEntry{}, err
	}
	observedAt := decoded.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	// A sandbox-agent old enough to send no resources, or new enough to run on
	// a platform where neither its cgroup nor procfs could be read, reports
	// none: the rest of its status is unaffected.
	if decoded.Resources != nil && decoded.Resources.ObservedAt.IsZero() {
		decoded.Resources.ObservedAt = observedAt
	}
	return SandboxAgentStatusEntry{
		SandboxID:  sandboxID,
		Status:     json.RawMessage(data),
		ObservedAt: observedAt,
		Resources:  decoded.Resources,
	}, nil
}

// ensureTokens mints tokens for any sandboxID missing from the cache or
// within sandboxAgentStatusTokenRefreshMargin of expiry, and returns the
// current token for every sandboxID it has one for. A mint failure this tick
// just means those sandboxes are skipped this tick — the next tick tries
// again, since the ticker itself is the retry.
func (p *sandboxAgentStatusPoller) ensureTokens(ctx context.Context, sandboxIDs []string) map[string]string {
	now := time.Now()
	p.mu.Lock()
	var need []string
	for _, id := range sandboxIDs {
		cached, ok := p.tokens[id]
		if !ok || now.Add(sandboxAgentStatusTokenRefreshMargin).After(cached.expiresAt) {
			need = append(need, id)
		}
	}
	p.mu.Unlock()

	if len(need) > 0 {
		resp, err := p.client.MintSandboxAgentStatusTokens(ctx, MintSandboxAgentStatusTokensRequest{
			ControlPlaneURL: p.bootstrap.ControlPlaneURL,
			ProjectID:       p.bootstrap.ProjectID,
			PoolID:          p.bootstrap.PoolID,
			PrivateKey:      p.registration.PrivateKey,
			SandboxIDs:      need,
		})
		if err != nil {
			p.logger.Warn("mint sandbox-agent status tokens", "error", err)
		} else {
			p.mu.Lock()
			for _, tok := range resp.Tokens {
				p.tokens[tok.SandboxID] = cachedSandboxAgentToken{token: tok.Token, expiresAt: tok.ExpiresAt}
			}
			p.mu.Unlock()
		}
	}

	out := make(map[string]string, len(sandboxIDs))
	p.mu.Lock()
	for _, id := range sandboxIDs {
		if cached, ok := p.tokens[id]; ok {
			out[id] = cached.token
		}
	}
	p.mu.Unlock()
	return out
}
