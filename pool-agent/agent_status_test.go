package poolagent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type recordingStatusClient struct {
	mu      sync.Mutex
	calls   []StatusRequest
	err     error
	changed chan struct{}
}

func newRecordingStatusClient() *recordingStatusClient {
	return &recordingStatusClient{changed: make(chan struct{}, 64)}
}

func (c *recordingStatusClient) UpdatePoolStatus(_ context.Context, req StatusRequest) error {
	c.mu.Lock()
	c.calls = append(c.calls, req)
	err := c.err
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
	return err
}

func (c *recordingStatusClient) setErr(err error) {
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
}

func (c *recordingStatusClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// waitForCalls blocks until the client has recorded at least want reports.
func (c *recordingStatusClient) waitForCalls(t *testing.T, want int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for c.count() < want {
		select {
		case <-c.changed:
		case <-deadline:
			t.Fatalf("recorded %d status reports, want at least %d", c.count(), want)
		}
	}
}

func testReporterArgs() (Bootstrap, *Registration, *slog.Logger) {
	bootstrap := Bootstrap{
		ProjectID:       "project-1",
		PoolID:          "pool-1",
		ControlPlaneURL: "http://control-plane.invalid",
	}
	return bootstrap, &Registration{Bootstrap: bootstrap}, slog.New(slog.DiscardHandler)
}

// The control plane clears the agent-reported ready flag on its own whenever a
// reconcile fails, and never sets it back. A boot-only report therefore left
// the pool unschedulable until the agent restarted, so the report has to
// repeat for as long as the agent runs.
func TestStatusReporterKeepsReporting(t *testing.T) {
	client := newRecordingStatusClient()
	bootstrap, registration, logger := testReporterArgs()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := startStatusReporter(ctx, logger, bootstrap, registration, client, time.Millisecond); err != nil {
		t.Fatalf("start status reporter: %v", err)
	}

	client.waitForCalls(t, 3)

	client.mu.Lock()
	defer client.mu.Unlock()
	for i, call := range client.calls {
		if !call.Ready || !call.Schedulable || call.Degraded {
			t.Fatalf("report %d = ready %t schedulable %t degraded %t, want a ready and schedulable pool",
				i, call.Ready, call.Schedulable, call.Degraded)
		}
		if call.ProjectID != "project-1" || call.PoolID != "pool-1" {
			t.Fatalf("report %d addressed project %q pool %q", i, call.ProjectID, call.PoolID)
		}
	}
}

// The first report gates the agent's boot: a pool that cannot mark itself ready
// must fail loudly rather than serve while the control plane believes it is
// unschedulable.
func TestStatusReporterFailsBootOnFirstReportError(t *testing.T) {
	client := newRecordingStatusClient()
	client.setErr(errors.New("control plane rejected status"))
	bootstrap, registration, logger := testReporterArgs()

	err := startStatusReporter(t.Context(), logger, bootstrap, registration, client, time.Millisecond)
	if err == nil {
		t.Fatal("start status reporter succeeded, want the boot report's error")
	}
	if client.count() != 1 {
		t.Fatalf("recorded %d reports, want exactly the failed boot report", client.count())
	}
}

// A failed beat is transient: the next tick re-reports. That retry is what
// recovers a pool whose readiness was cleared while the control plane was down.
func TestStatusReporterRetriesAfterFailedReport(t *testing.T) {
	client := newRecordingStatusClient()
	bootstrap, registration, logger := testReporterArgs()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := startStatusReporter(ctx, logger, bootstrap, registration, client, time.Millisecond); err != nil {
		t.Fatalf("start status reporter: %v", err)
	}

	client.setErr(errors.New("control plane is down"))
	client.waitForCalls(t, 3)
	client.setErr(nil)

	recovered := client.count()
	client.waitForCalls(t, recovered+2)
}

// Canceling the agent's context stops the heartbeat rather than leaking a
// goroutine that reports for a pool that is shutting down.
func TestStatusReporterStopsOnContextCancel(t *testing.T) {
	client := newRecordingStatusClient()
	bootstrap, registration, logger := testReporterArgs()

	ctx, cancel := context.WithCancel(t.Context())
	if err := startStatusReporter(ctx, logger, bootstrap, registration, client, time.Millisecond); err != nil {
		t.Fatalf("start status reporter: %v", err)
	}
	client.waitForCalls(t, 2)
	cancel()

	// Let any in-flight tick land, then confirm the count has settled.
	time.Sleep(50 * time.Millisecond)
	settled := client.count()
	time.Sleep(50 * time.Millisecond)
	if got := client.count(); got != settled {
		t.Fatalf("recorded %d reports after cancel, want the count to stop at %d", got, settled)
	}
}
