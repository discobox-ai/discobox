package poolagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// maxSandboxInventoryBytes bounds one sandbox's inventory. It holds every
// terminal's visible screen, so it is far larger than a status, but a sandbox
// answering more than this is not posted at all: what the control plane keeps
// must stay a thing it can afford to keep for every sandbox.
const maxSandboxInventoryBytes = 4 << 20

// maxInventoryReportBytes bounds the inventories one report carries. A pool
// has no bound on its sandboxes, and on its first tick, or after a control
// plane restart loses nothing but still asks, every one of them is sent in
// full: one request for all of them would be as large as the pool, and the
// control plane holds a request body in memory to decode it. An entry sent by
// digest alone weighs nothing here.
const maxInventoryReportBytes = 8 << 20

// reportInventories reads the inventory of each of running — what its list
// reads answer — and posts them to the control plane, which answers those
// reads from the last one while the sandbox is not running (ADR 26-10-11-657).
//
// An inventory is sent only when it changed. Each sandbox is asked with the
// digest the control plane last took from it and answers "unchanged" to that;
// the entry then carries the digest alone, which is what moves the stored
// inventory's observation time forward. A digest is recorded as taken only
// once the control plane has answered without asking for that sandbox again,
// so an inventory it lost, or never stored, is sent in full on the next tick.
//
// Like the status poll it never affects a sandbox's lifecycle, and one
// sandbox failing costs only its own entry.
func (p *sandboxAgentStatusPoller) reportInventories(ctx context.Context, running []string) {
	if len(running) == 0 {
		p.mu.Lock()
		clear(p.inventories)
		p.mu.Unlock()
		return
	}
	tokens := p.ensureTokens(ctx, running)

	p.mu.Lock()
	// A sandbox no longer running is forgotten, so this map stays the size
	// of what runs. One that comes back sends its inventory in full once.
	known := make(map[string]string, len(running))
	for _, id := range running {
		if digest, ok := p.inventories[id]; ok {
			known[id] = digest
		}
	}
	p.inventories = make(map[string]string, len(known))
	for id, digest := range known {
		p.inventories[id] = digest
	}
	p.mu.Unlock()

	var mu sync.Mutex
	var wg sync.WaitGroup
	entries := make([]SandboxInventoryEntry, 0, len(running))
	for _, sandboxID := range running {
		token, ok := tokens[sandboxID]
		if !ok {
			continue
		}
		wg.Add(1)
		go func(sandboxID, token string) {
			defer wg.Done()
			entry, ok, err := p.pollInventory(ctx, sandboxID, token, known[sandboxID])
			if err != nil {
				p.logger.Warn("poll sandbox-agent inventory", "sandboxId", sandboxID, "error", err)
				return
			}
			if !ok {
				return
			}
			mu.Lock()
			entries = append(entries, entry)
			mu.Unlock()
		}(sandboxID, token)
	}
	wg.Wait()
	if len(entries) == 0 {
		return
	}
	for _, batch := range inventoryReportBatches(entries) {
		resp, err := p.client.ReportSandboxInventories(ctx, SandboxInventoryReportRequest{
			ControlPlaneURL: p.bootstrap.ControlPlaneURL,
			ProjectID:       p.bootstrap.ProjectID,
			PoolID:          p.bootstrap.PoolID,
			PrivateKey:      p.identity.PrivateKey,
			Sandboxes:       batch,
		})
		if err != nil {
			// Nothing in this report is recorded as taken, so what changed
			// is sent again. The reports after it are still made.
			p.logger.Warn("push sandbox inventories", "error", err)
			continue
		}
		wanted := make(map[string]bool, len(resp.Wanted))
		for _, id := range resp.Wanted {
			wanted[id] = true
		}
		p.mu.Lock()
		for _, entry := range batch {
			if wanted[entry.SandboxID] {
				delete(p.inventories, entry.SandboxID)
				continue
			}
			p.inventories[entry.SandboxID] = entry.Digest
		}
		p.mu.Unlock()
	}
}

// inventoryReportBatches splits a tick's entries into reports that each carry
// at most maxInventoryReportBytes of inventory. One inventory is never split,
// and is never larger than a report may be.
func inventoryReportBatches(entries []SandboxInventoryEntry) [][]SandboxInventoryEntry {
	var batches [][]SandboxInventoryEntry
	var batch []SandboxInventoryEntry
	size := 0
	for _, entry := range entries {
		if len(batch) > 0 && size+len(entry.Inventory) > maxInventoryReportBytes {
			batches = append(batches, batch)
			batch, size = nil, 0
		}
		batch = append(batch, entry)
		size += len(entry.Inventory)
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	return batches
}

// pollInventory reads one sandbox's inventory, naming the digest already
// delivered. ok is false, with no error, for a sandbox agent that has no
// inventory to give: one older than the route, or one this token may not
// read because the control plane that minted it is.
func (p *sandboxAgentStatusPoller) pollInventory(ctx context.Context, sandboxID, token, known string) (SandboxInventoryEntry, bool, error) {
	callCtx, cancel := context.WithTimeout(ctx, sandboxAgentStatusCallTimeout)
	defer cancel()
	dial, err := p.runtime.SandboxDialer(callCtx, sandboxID, sandboxruntime.SandboxAgentPort)
	if err != nil {
		return SandboxInventoryEntry{}, false, err
	}
	inventoryURL := sandboxruntime.HTTPURL(sandboxruntime.SandboxAgentPort, fmt.Sprintf("/api/projects/%s/sandboxes/%s/inventory", p.bootstrap.ProjectID, sandboxID))
	if known != "" {
		inventoryURL.RawQuery = url.Values{"known": {known}}.Encode()
	}
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, inventoryURL.String(), nil)
	if err != nil {
		return SandboxInventoryEntry{}, false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Transport: dial.Transport()}).Do(req)
	if err != nil {
		return SandboxInventoryEntry{}, false, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSandboxInventoryBytes+1))
	if err != nil {
		return SandboxInventoryEntry{}, false, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusForbidden:
		// A sandbox agent from before the inventory has no such route, and a
		// control plane from before it minted this token without the scope
		// the route asks for. Either way there is no inventory to be had, the
		// sandbox is read as one that posted nothing, and saying so every
		// tick would be noise.
		return SandboxInventoryEntry{}, false, nil
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return SandboxInventoryEntry{}, false, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(data[:min(len(data), 4096)])))
	case len(data) > maxSandboxInventoryBytes:
		return SandboxInventoryEntry{}, false, fmt.Errorf("inventory is larger than %d bytes and is not posted", maxSandboxInventoryBytes)
	}
	// Three fields are read; the inventory itself is relayed as received, so
	// a part this agent does not model still reaches the control plane.
	var decoded struct {
		ObservedAt time.Time `json:"observedAt"`
		Digest     string    `json:"digest"`
		Unchanged  bool      `json:"unchanged"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return SandboxInventoryEntry{}, false, err
	}
	if decoded.Digest == "" {
		return SandboxInventoryEntry{}, false, fmt.Errorf("inventory carries no digest")
	}
	if decoded.ObservedAt.IsZero() {
		decoded.ObservedAt = time.Now().UTC()
	}
	entry := SandboxInventoryEntry{SandboxID: sandboxID, Digest: decoded.Digest, ObservedAt: decoded.ObservedAt}
	if !decoded.Unchanged {
		entry.Inventory = json.RawMessage(data)
	}
	return entry, true, nil
}
