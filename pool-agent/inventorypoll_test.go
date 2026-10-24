package poolagent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/pool-agent/sandboxruntime"
)

// inventoryAgent is a sandbox agent that serves a status and an inventory
// whose digest the test sets, answering "unchanged" to a caller that names it.
type inventoryAgent struct {
	mu     sync.Mutex
	digest string
	known  []string
	status int
}

func (a *inventoryAgent) set(digest string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.digest = digest
}

func (a *inventoryAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasSuffix(r.URL.Path, "/inventory") {
		statusOKHandler("2026-01-01T00:00:00Z")(w, r)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.status != 0 {
		w.WriteHeader(a.status)
		return
	}
	known := r.URL.Query().Get("known")
	a.known = append(a.known, known)
	w.Header().Set("Content-Type", "application/json")
	if known == a.digest {
		_, _ = w.Write([]byte(`{"observedAt":"2026-01-01T00:00:05Z","digest":"` + a.digest + `","unchanged":true}`))
		return
	}
	_, _ = w.Write([]byte(`{"observedAt":"2026-01-01T00:00:05Z","digest":"` + a.digest + `","execs":[{"id":"exec-1"}],"fieldThisAgentDoesNotModel":true}`))
}

func newInventoryPoller(t *testing.T, agent http.Handler) (*sandboxAgentStatusPoller, *fakeSandboxAgentStatusClient) {
	t.Helper()
	srv := httptest.NewServer(agent)
	t.Cleanup(srv.Close)
	runtime := &statusPollTestRuntime{
		MemorySandboxRuntime: sandboxruntime.NewMemorySandboxRuntime(),
		servers:              map[string]*httptest.Server{"sandbox-1": srv},
	}
	if _, err := runtime.CreateSandbox(context.Background(), &workerapimodel.PoolSandboxCreateRequest{SandboxId: "sandbox-1"}); err != nil {
		t.Fatal(err)
	}
	client := &fakeSandboxAgentStatusClient{}
	return &sandboxAgentStatusPoller{
		logger:      slog.New(slog.DiscardHandler),
		bootstrap:   Bootstrap{ControlPlaneURL: "http://control-plane.invalid", ProjectID: "project-1", PoolID: "pool-1"},
		identity:    statusPollTestIdentity(t),
		runtime:     runtime,
		client:      client,
		tokens:      map[string]cachedSandboxAgentToken{},
		samples:     map[string]sandboxResourceSample{},
		inventories: map[string]string{},
	}, client
}

func lastInventoryReport(t *testing.T, client *fakeSandboxAgentStatusClient, reports int) SandboxInventoryEntry {
	t.Helper()
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.inventories) != reports {
		t.Fatalf("%d inventory reports, want %d", len(client.inventories), reports)
	}
	last := client.inventories[reports-1]
	if len(last) != 1 || last[0].SandboxID != "sandbox-1" {
		t.Fatalf("report = %+v, want one entry for sandbox-1", last)
	}
	return last[0]
}

// An inventory is posted in full when it changes and by digest alone while it
// stands (ADR 26-10-11-657 §2), relayed as the sandbox agent sent it.
func TestInventoryIsPostedInFullOnlyWhenItChanges(t *testing.T) {
	agent := &inventoryAgent{digest: "sha256:one"}
	poller, client := newInventoryPoller(t, agent)
	ctx := context.Background()

	poller.tick(ctx)
	first := lastInventoryReport(t, client, 1)
	if first.Digest != "sha256:one" || first.Inventory == nil {
		t.Fatalf("first report = %+v, want the inventory in full", first)
	}
	var relayed map[string]json.RawMessage
	if err := json.Unmarshal(first.Inventory, &relayed); err != nil {
		t.Fatal(err)
	}
	if _, ok := relayed["fieldThisAgentDoesNotModel"]; !ok {
		t.Fatalf("the inventory was not relayed as received: %s", first.Inventory)
	}

	poller.tick(ctx)
	second := lastInventoryReport(t, client, 2)
	if second.Digest != "sha256:one" || second.Inventory != nil {
		t.Fatalf("second report = %+v, want the digest alone: nothing changed", second)
	}
	if second.ObservedAt.IsZero() {
		t.Fatal("an unchanged entry carries no observation time to move the stored one to")
	}

	agent.set("sha256:two")
	poller.tick(ctx)
	if third := lastInventoryReport(t, client, 3); third.Digest != "sha256:two" || third.Inventory == nil {
		t.Fatalf("third report = %+v, want the changed inventory in full", third)
	}
}

// A digest counts as delivered only once the control plane has taken it: an
// inventory it asks for again, or a report that failed, is sent in full.
func TestInventoryTheControlPlaneDoesNotHoldIsSentAgain(t *testing.T) {
	agent := &inventoryAgent{digest: "sha256:one"}
	poller, client := newInventoryPoller(t, agent)
	ctx := context.Background()

	client.inventoryErr = errors.New("control plane unreachable")
	poller.tick(ctx)
	client.inventoryErr = nil
	poller.tick(ctx)
	if entry := lastInventoryReport(t, client, 2); entry.Inventory == nil {
		t.Fatalf("after a failed report the next one = %+v, want the inventory in full", entry)
	}

	poller.tick(ctx)
	if entry := lastInventoryReport(t, client, 3); entry.Inventory != nil {
		t.Fatalf("report = %+v, want the digest alone", entry)
	}
	// The control plane answers that it does not hold it.
	client.mu.Lock()
	client.wanted = []string{"sandbox-1"}
	client.mu.Unlock()
	poller.tick(ctx)
	poller.tick(ctx)
	if entry := lastInventoryReport(t, client, 5); entry.Inventory == nil {
		t.Fatalf("after the control plane asked for it the report = %+v, want the inventory in full", entry)
	}
}

// A sandbox agent from before the inventory has none to give. Its status is
// still reported, and nothing is posted for it.
func TestInventoryIsNotPostedForASandboxAgentWithoutTheRoute(t *testing.T) {
	poller, client := newInventoryPoller(t, &inventoryAgent{status: http.StatusNotFound})
	poller.tick(context.Background())
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.reported) != 1 {
		t.Fatalf("status entries = %d, want the status still reported", len(client.reported))
	}
	if len(client.inventories) != 0 {
		t.Fatalf("inventory reports = %+v, want none", client.inventories)
	}
}

// A configure sandbox is polled every second for its ports, and never for an
// inventory.
func TestInventoryIsNotReadOnTheConfigureTick(t *testing.T) {
	agent := &inventoryAgent{digest: "sha256:one"}
	poller, client := newInventoryPoller(t, agent)
	poller.pollAndReport(context.Background(), []string{"sandbox-1"})
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.reported) != 1 || len(client.inventories) != 0 {
		t.Fatalf("status entries = %d, inventory reports = %d; want 1 and 0", len(client.reported), len(client.inventories))
	}
}

// A pool's inventories do not all go in one request: a report is bounded,
// whatever the pool's size, and nothing is dropped or split to keep it so.
func TestInventoryReportsAreBoundedInSize(t *testing.T) {
	full := json.RawMessage(strings.Repeat("x", 3<<20))
	var entries []SandboxInventoryEntry
	for i := 0; i < 7; i++ {
		entries = append(entries, SandboxInventoryEntry{SandboxID: "full", Inventory: full})
		entries = append(entries, SandboxInventoryEntry{SandboxID: "digest-only"})
	}
	batches := inventoryReportBatches(entries)
	if len(batches) < 3 {
		t.Fatalf("%d reports for 21 MiB of inventories, want them split", len(batches))
	}
	total := 0
	for _, batch := range batches {
		size := 0
		for _, entry := range batch {
			size += len(entry.Inventory)
		}
		if size > maxInventoryReportBytes {
			t.Fatalf("a report carries %d bytes, more than %d", size, maxInventoryReportBytes)
		}
		total += len(batch)
	}
	if total != len(entries) {
		t.Fatalf("%d entries reported, want all %d", total, len(entries))
	}
	if got := inventoryReportBatches(nil); len(got) != 0 {
		t.Fatalf("no entries made %d reports", len(got))
	}
}
