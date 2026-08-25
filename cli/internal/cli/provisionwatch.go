package cli

import (
	"context"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
)

// Narrating the wait for a sandbox that is not ready yet (ADR 0060).
//
// An attach blocks until every tier below it reports ready (ADR 0039), and the
// wait can legitimately run for minutes behind an image pull. Nothing is sent
// on the attach connection while that happens — the socket has not been
// upgraded yet, because the control plane is still waiting to proxy it — so the
// only way a client can say what it is waiting for is to read the sandbox and
// report what the pool agent recorded there.
//
// What that record says is `sandboxcreate.ProvisionStatus`, which a create's
// own wait for a source push narrates from as well. This is the loop around it:
// when to read, and when to stop.

// provisionPollInterval is how often a client waiting on a sandbox re-reads it.
// It is display only: nothing waits on this loop, and the attach it runs beside
// is already correct without it.
//
// This is not the poll ADR 0039 removed. That one gated readiness — the client
// could not attach until it completed — and cost a round trip per second of
// provisioning for an answer the server already knew. This one starts after the
// attach is issued, ends when the attach connects, and its worst failure is a
// status line that updates late.
const provisionPollInterval = 500 * time.Millisecond

// watchProvisioning reports what a sandbox is being made to do, until ctx ends.
//
// It only ever calls report with a line that differs from the last one, so a
// caller can render every call and a phase that persists does not flicker.
// Nothing is reported for a sandbox that is already usable: there is no
// provisioning left to describe, and saying so would overwrite whatever the
// caller has on the line.
func (a *App) watchProvisioning(ctx context.Context, projectID, sandboxID string, report func(string)) {
	if report == nil {
		return
	}
	// One client for the whole watch. Both reads below go through it, and
	// building one per poll would put an autolaunch probe on a loop that runs
	// twice a second.
	client, err := a.apiClient()
	if err != nil {
		return
	}
	last := ""
	for {
		// The wait is measured before it is described, so an attach onto a
		// discobox that is already up reads nothing at all: it connects inside
		// the first interval and this returns having asked the server nothing.
		// A wait worth narrating lasts seconds, and half of one costs it
		// nothing.
		select {
		case <-ctx.Done():
			return
		case <-time.After(provisionPollInterval):
		}
		res, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
		if err != nil {
			// An unreadable sandbox says nothing rather than saying something
			// wrong. The read can fail for reasons that have no bearing on
			// provisioning — a momentarily unavailable server above all — and
			// the attach is the thing entitled to report those.
			continue
		}
		sandbox, err := expectResponse[apimodel.Sandbox](res)
		if err != nil {
			continue
		}
		if line := string(sandboxcreate.Status(ctx, client, projectID, sandbox)); line != "" && line != last {
			last = line
			report(line)
		}
	}
}
