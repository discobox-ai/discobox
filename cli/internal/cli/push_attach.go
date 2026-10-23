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

// Delivering on attach (ADR 26-09-24-005): a discobox whose create failed after it
// parked in awaiting_source waits for a push nobody is making, and attaching to
// it is a person asking for it by name. So an attach performs the delivery
// `discobox push` would — the same code, with no overrides — before it dials,
// rather than joining a wait that can only end in a timeout.
//
// Before, not beside: the attach wait gives up after a stall budget, and
// nothing it watches moves while this client pushes, so a large first push run
// alongside it would fail the very attach it was rescuing.

// deliverableHere reports that the discobox is parked with a delivery still
// owed that this machine can make: it has push-delivered sources, nobody has
// reported them delivered, and this machine is the one it was created on.
//
// Parked is not enough on its own. A discobox stays in awaiting_source after
// its delivery is reported, until the reconciler acts on the report — which is
// exactly the discobox `discobox run --raw` attaches to the moment its own
// delivery returns. A discobox created elsewhere is left to its own machine,
// which may be delivering it right now and is the only one its recorded
// directories describe.
//
// Two things changed that with ADR 26-10-10-545. A source that is a remote
// is on no machine, so a discobox waiting only for remotes is delivered by
// whichever client attaches (§1). And a discobox that a discobox created is
// never delivered by an attach: its origin is the creating sandbox's own
// claim, a delivery from here is made with this person's reach — their
// directories, their Git credentials, everything a project member may pin —
// and what lands in it can be read by the sandbox that created it. That
// sandbox delivers it, held to what it can reach itself (§7).
func deliverableHere(sb apimodel.Sandbox, hostID string) bool {
	if !sandboxAwaitingSource(sb) || sb.Runtime.SourceDeliveredAt.IsSet() {
		return false
	}
	if sb.CreatedBySandboxId.Or("") != "" {
		return false
	}
	pending := sandboxcreate.PendingSourcePushes(&sb)
	if len(pending) == 0 {
		return false
	}
	remotesOnly := true
	for _, source := range pending {
		remotesOnly = remotesOnly && source.Remote != ""
	}
	if remotesOnly {
		return true
	}
	// A source from a directory is this machine's to deliver only when this
	// machine is known to be the one that holds it.
	origin, ok := sb.Origin.Get()
	return hostID != "" && ok && origin.HostId == hostID
}

// delivery is one delivery to a parked discobox in flight: done closes when it
// has finished, and err is its answer from then on.
type delivery struct {
	done chan struct{}
	err  error
}

// deliverBeforeAttach delivers the source a discobox is still waiting for, when
// this machine can (deliverableHere), and does nothing otherwise. report is
// told each step as it begins.
//
// Within this process there is one attach's delivery per discobox: a second
// attach — a launcher row opened again while the first delivery is still
// pushing — waits for that one rather than pushing the same refs beside it. The
// delivery runs on the context of the attach that started it. A create's own
// delivery is not joined here; one overlapping it is DeliverSource's to
// tolerate.
func (a *App) deliverBeforeAttach(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string, report func(string)) error {
	a.pushMu.Lock()
	if running := a.deliveries[sandboxID]; running != nil {
		a.pushMu.Unlock()
		if report != nil {
			report(string(sandboxcreate.StepPushingSource))
		}
		select {
		case <-running.done:
			return running.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	running := &delivery{done: make(chan struct{})}
	if a.deliveries == nil {
		a.deliveries = map[string]*delivery{}
	}
	a.deliveries[sandboxID] = running
	a.pushMu.Unlock()

	running.err = a.deliverOwedSource(ctx, client, projectID, sandboxID, report)
	a.pushMu.Lock()
	delete(a.deliveries, sandboxID)
	a.pushMu.Unlock()
	close(running.done)
	return running.err
}

// deliverOwedSource is one attach's delivery, once it is this attach's to make.
// Another client delivering the same discobox at the same moment is
// DeliverSource's to tolerate (ADR 26-09-24-005 §3), for this delivery and the create's
// alike.
func (a *App) deliverOwedSource(ctx context.Context, client *apiclientgen.Client, projectID, sandboxID string, report func(string)) error {
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
	return fmt.Errorf("the discobox is still waiting for its source, and it could not be delivered from here: %w; `discobox push --dir SLUG=PATH` delivers it from another directory", err)
}
