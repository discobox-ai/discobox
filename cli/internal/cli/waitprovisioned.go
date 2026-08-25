package cli

import (
	"context"
	"fmt"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
)

// provisionStallTimeout is how long this wait tolerates nothing happening. See
// sandboxcreate.StallClock for why it is silence that is bounded and not total
// elapsed time.
const provisionStallTimeout = 5 * time.Minute

// waitForProvisionedSandbox waits for a sandbox to come up, narrating what it
// is waiting for and giving up only once nothing has happened for
// provisionStallTimeout.
//
// One loop rather than a wait beside a narrator: the two would read the same
// sandbox at different moments to answer questions about the same thing, and
// the narration is what tells this loop the wait is still alive.
func (a *App) waitForProvisionedSandbox(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string, report func(string)) error {
	ticker := time.NewTicker(provisionPollInterval)
	defer ticker.Stop()
	stall := sandboxcreate.NewStallClock(provisionStallTimeout)
	last := ""
	for {
		sandbox, err := a.readSandbox(ctx, client, projectID, sandboxID)
		if err != nil {
			return err
		}
		// displayState is the single vocabulary the server exposes for this
		// (ADR 0017 §7); reading raw state plus generations here would be
		// re-deriving what it already computed.
		switch sandbox.Runtime.DisplayState.Or("") {
		case "running":
			return nil
		case "error":
			return fmt.Errorf("discobox failed: %s", sandboxFailureReason(sandbox))
		}
		if line := string(a.provisionStatus(ctx, client, projectID, sandbox)); line != "" && line != last {
			last = line
			if report != nil {
				report(line)
			}
			// Something happened, so the clock starts again. A pull restates
			// its byte counts twice a second, which is exactly the movement
			// this is asking about.
			stall.Progressed()
		}
		if stall.Expired() {
			if last == "" {
				return fmt.Errorf("gave up after %s with no sign of progress", provisionStallTimeout)
			}
			return fmt.Errorf("gave up after %s with no further progress (last: %s)", provisionStallTimeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (a *App) readSandbox(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string) (*apimodel.Sandbox, error) {
	res, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return nil, err
	}
	return expectResponse[apimodel.Sandbox](res)
}

// provisionStatus is what the wait says it is waiting for.
//
// A sandbox that no pool has taken yet knows only that it is waiting, and that
// is the least useful thing to say for the longest part of a cold start: the
// pool is off fetching a VM image and pulling the pool-agent image, and the
// driver doing it records that as it goes. So when the sandbox says it is
// waiting for a pool, the pool is asked what it is doing instead.
//
// One extra read per poll, and only while the sandbox is pending. A pool that
// has nothing current to report leaves the sandbox's own answer standing.
func (a *App) provisionStatus(ctx context.Context, client *apiclientgen.Client, projectID string, sandbox *apimodel.Sandbox) sandboxcreate.Step {
	step := sandboxcreate.ProvisionStatus(sandbox)
	if step != sandboxcreate.StepWaitingForPool {
		return step
	}
	poolID := sandbox.PoolId.Or("")
	if poolID == "" {
		return step
	}
	res, err := client.GetPool(ctx, apiclientgen.GetPoolParams{ProjectId: projectID, PoolId: poolID})
	if err != nil {
		// An unreadable pool says nothing rather than something wrong: the read
		// can fail for reasons that have no bearing on provisioning, and the
		// wait itself is entitled to report those.
		return step
	}
	pool, err := expectResponse[apimodel.Pool](res)
	if err != nil {
		return step
	}
	if poolStep := sandboxcreate.PoolProvisionStatus(pool); poolStep != "" {
		return poolStep
	}
	return step
}
