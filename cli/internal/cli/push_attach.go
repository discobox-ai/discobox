package cli

import (
	"context"
	"errors"
	"fmt"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxcreate"
	"github.com/discobox-ai/discobox/internal/hostid"
)

// Delivering on attach (ADR 0150): a discobox whose create failed after it
// parked in awaiting_source waits for a push nobody is making, and attaching to
// it is a person asking for it by name. So an attach performs the delivery
// `discobox push` would — the same code, with no overrides — before it dials,
// rather than joining a wait that can only end in a timeout.
//
// Before, not beside: the attach wait gives up after a stall budget, and
// nothing it watches moves while this client pushes, so a large first push run
// alongside it would fail the very attach it was rescuing.

// deliverableHere reports that the discobox is parked waiting for a source this
// machine can deliver: it has push-delivered sources, and this machine is the
// one it was created on. A discobox created elsewhere is left to its own
// machine, which may be delivering it right now and is the only one its
// recorded directories describe.
func deliverableHere(sb apimodel.Sandbox, hostID string) bool {
	if hostID == "" || !sandboxAwaitingSource(sb) {
		return false
	}
	origin, ok := sb.Origin.Get()
	if !ok || origin.HostId != hostID {
		return false
	}
	return len(sandboxcreate.PendingSourcePushes(&sb)) > 0
}

// deliverBeforeAttach delivers the source a discobox is still waiting for, when
// this machine can (deliverableHere), and does nothing otherwise. report is
// told each step as it begins.
//
// A delivery that fails because somebody else's finished first is not a
// failure: the create that parked the discobox may still be running, or another
// window attached at the same moment, and both push the same pinned commit. So
// a failure is re-read against the discobox, and one that is no longer parked
// lets the attach carry on.
func (a *App) deliverBeforeAttach(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string, report func(string)) error {
	sandbox, err := getSandbox(ctx, client, projectID, sandboxID)
	if err != nil {
		return err
	}
	// A machine with no resolvable identity delivers nothing, which is the
	// answer it gives for a discobox created somewhere else; see pushable.
	hostID, _ := hostid.Get()
	if !deliverableHere(*sandbox, hostID) {
		return nil
	}
	gitServerURL, releaseGitServerURL, err := a.gitServerURL(ctx)
	if err != nil {
		return err
	}
	defer releaseGitServerURL()
	step := func(step sandboxcreate.Step) {
		if report != nil {
			report(string(step))
		}
	}
	_, err = a.deliverParkedSource(ctx, client, projectID, sandbox, sandboxcreate.PendingSourcePushes(sandbox), hostID, gitServerURL, nil, step)
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	if again, readErr := getSandbox(ctx, client, projectID, sandboxID); readErr == nil && !sandboxAwaitingSource(*again) {
		return nil
	}
	return fmt.Errorf("the discobox is still waiting for its source, and it could not be delivered from here: %w; `discobox push --dir SLUG=PATH` delivers it from another directory", err)
}
