package sandbox

import (
	"context"
	"log/slog"

	poolagentauth "github.com/discobox-ai/discobox/server/internal/auth/poolagent"
	"github.com/discobox-ai/discobox/server/internal/model"
)

// PoolManager is the control-plane surface pool-backed providers need.
// Providers own runtime mechanics; the manager owns persistence, credentials,
// and lifecycle intent. A pool is its own runtime host (ADR-0006).
type PoolManager interface {
	GetPool(ctx context.Context, projectID, poolID string) (*model.Pool, error)
	ListPoolsForProviderInstance(ctx context.Context, projectID, providerID string) ([]model.Pool, error)
	// ListPools returns every pool in the project, across provider instances.
	// pool-sync needs this wider set: a pool agent reaps by scanning
	// project-scoped host trees, so anything narrower would report another
	// provider instance's live pools as orphans.
	ListPools(ctx context.Context, projectID string) ([]model.Pool, error)
	// SchedulablePoolForSandbox gates placement: the sandbox's pool must be
	// ready, schedulable, and fit the request within its reported capacity.
	SchedulablePoolForSandbox(ctx context.Context, sandbox *model.Sandbox) (*model.Pool, error)
	GetProject(ctx context.Context, projectID string) (*model.Project, error)
	GetSandboxProviderInstance(ctx context.Context, projectID, providerID string) (*model.SandboxProviderInstance, error)
	CountSandboxesForPool(ctx context.Context, projectID, poolID string) (int64, error)
	CreatePoolBootstrapToken(ctx context.Context, token *model.PoolBootstrapToken) error
	EnsureAgentTrustKey(ctx context.Context) (string, error)
	CreateAgentToken(ctx context.Context, claims poolagentauth.TokenClaims) (string, error)
	CreateSandboxAgentToken(ctx context.Context, claims poolagentauth.TokenClaims) (string, error)
	SchedulePoolReconciliation(ctx context.Context, projectID, poolID string) error
	// SchedulePoolRepair re-drives a failed pool as new intent (generation bump
	// plus dirty mark), so schedulers can tell a pending retry from a settled
	// failure.
	SchedulePoolRepair(ctx context.Context, poolID, reason string) error
	// ReportPoolProvisionProgress records what the driver is doing to bring a
	// pool host up. It is display only: nothing waits on it, nothing decides
	// anything from it, and a report that fails to store is not worth failing a
	// reconcile over.
	ReportPoolProvisionProgress(ctx context.Context, poolID string, progress PoolProvisionProgress) error
}

// PoolProvisionPhase is what a provider driver is doing to bring a pool host
// up, named for a client whose sandbox is waiting for a pool to take it.
//
// The work is the driver's, so the phases are the driver's. A VM backend
// fetches a disk image and boots a machine before anything can run containers,
// and on a cold start that is where the minutes actually go — the phase a
// client used to spend them staring at was "waiting for a pool to take it",
// which says only that the wait exists.
//
// An observation, never a state: it decides nothing, and it is history the
// moment the phase ends (ADR 0060).
type PoolProvisionPhase string

const (
	PoolPhaseFetchingVMImage     PoolProvisionPhase = "fetching_vm_image"
	PoolPhaseStartingVM          PoolProvisionPhase = "starting_vm"
	PoolPhaseWaitingForDocker    PoolProvisionPhase = "waiting_for_docker"
	PoolPhasePullingPoolImage    PoolProvisionPhase = "pulling_pool_image"
	PoolPhaseStartingPoolAgent   PoolProvisionPhase = "starting_pool_agent"
	PoolPhaseWaitingForPoolAgent PoolProvisionPhase = "waiting_for_pool_agent"
)

// PoolProvisionProgress is one report about a pool host being brought up.
//
// Phase always says what is happening. Pull refines the two phases that can say
// how far in they are, because fetching a VM image and pulling the pool-agent
// image are the two long ones and the only ones with a denominator.
type PoolProvisionProgress struct {
	Phase PoolProvisionPhase `json:"phase"`
	Pull  *PoolPullProgress  `json:"pull,omitempty"`
}

// PoolPullProgress is an image fetch as a status line wants it.
//
// Both totals grow while the manifest is walked, so current against total is a
// ratio at a moment rather than progress toward a fixed target: a client must
// not render it as a bar that can only move forward.
type PoolPullProgress struct {
	Image          string `json:"image,omitempty"`
	Current        int64  `json:"current,omitempty"`
	Total          int64  `json:"total,omitempty"`
	Layers         int    `json:"layers,omitempty"`
	LayersComplete int    `json:"layersComplete,omitempty"`
	Done           bool   `json:"done,omitempty"`
}

// PoolProgressReporter is how a driver reports its progress without holding the
// pool manager itself. Engines take one of these; a nil reporter is a driver
// that says nothing, which every code path must tolerate because half of them
// run in tests with no control plane behind them.
type PoolProgressReporter func(ctx context.Context, poolID string, progress PoolProvisionProgress)

// PoolProgressReporterFor adapts a pool manager into the reporter a driver
// takes, swallowing the error because there is nothing useful to do with it: a
// report that did not store leaves a status line on its previous phase while
// the work it describes carries on.
func PoolProgressReporterFor(manager PoolManager) PoolProgressReporter {
	if manager == nil {
		return nil
	}
	return func(ctx context.Context, poolID string, progress PoolProvisionProgress) {
		if err := manager.ReportPoolProvisionProgress(ctx, poolID, progress); err != nil {
			slog.DebugContext(ctx, "report pool provision progress", "pool", poolID, "phase", progress.Phase, "error", err)
		}
	}
}

// Report is a nil-safe call: a driver with no reporter says nothing, which
// every path has to tolerate because half of them run in tests with no control
// plane behind them.
func (r PoolProgressReporter) Report(ctx context.Context, poolID string, phase PoolProvisionPhase) {
	r.ReportProgress(ctx, poolID, PoolProvisionProgress{Phase: phase})
}

// ReportProgress is Report with the detail a phase can carry.
func (r PoolProgressReporter) ReportProgress(ctx context.Context, poolID string, progress PoolProvisionProgress) {
	if r == nil || poolID == "" || progress.Phase == "" {
		return
	}
	r(ctx, poolID, progress)
}
