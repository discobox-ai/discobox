package sandboxes

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/obot-platform/discobox/server/internal/apperrors"
	"github.com/obot-platform/discobox/server/internal/model"
	"github.com/obot-platform/discobox/server/internal/sandbox"
)

// resolveSourceDelivery records how a sandbox's source reaches it, so every
// later stage reads a stated intent instead of re-deriving it. The decision
// needs the provider, the client's origin, and this server's identity together,
// and only create has all three.
//
// A client may not ask for push delivery: whether a bind is possible is the
// server's to know, and a client claiming otherwise would either force a
// needless push or assert a reachability it cannot verify.
func (s *Service) resolveSourceDelivery(source *model.GitSource, origin *model.Origin, providerInstanceID string) error {
	if source == nil {
		return nil
	}
	if source.Delivery == model.GitSourceDeliveryPush {
		return apperrors.NewStatusError(http.StatusBadRequest,
			"source delivery is decided by the server and cannot be requested")
	}
	provider, err := s.sandboxProviders.GetProvider(providerInstanceID)
	if err != nil {
		// The instance exists in the store but has not been instantiated, so its
		// reachability is unknown. Leaving delivery at clone is safe only
		// because a sandbox whose provider never instantiates cannot run at all,
		// so no bind is ever attempted. Nothing here can be concluded about a
		// provider that does come up later.
		source.Delivery = model.GitSourceDeliveryClone
		return nil //nolint:nilerr // an uninstantiated provider is not a create failure
	}
	if sourceNeedsPush(provider.Definition(), s.hostID, origin, source) {
		source.Delivery = model.GitSourceDeliveryPush
		return nil
	}
	source.Delivery = model.GitSourceDeliveryClone
	return nil
}

// sourcePushTimeout bounds how long a sandbox waits for a client to push its
// source. Waiting is the one phase that depends on an actor the server cannot
// see: a client that is killed mid-push never reports anything, and the sandbox
// would otherwise hold its provider resources indefinitely. It is generous
// because the first push of a large repository sends its whole history.
const sourcePushTimeout = 30 * time.Minute

// parkForSourcePush holds a provisioned sandbox until its client reports the
// push complete. Its repository exists and can receive the push; starting the
// harness now would run it against an empty workspace.
//
// Parking is a normal state rather than a failure, so it reports success and
// the caller reads the phase. An expired wait is the exception: it returns an
// error, which the caller records as a failed operation.
func parkForSourcePush(sb *model.Sandbox) error {
	// Check before parking: once SetPhase is a no-op for an already-parked
	// sandbox, the deadline below is the one set when it first parked.
	if sb.Phase == model.SandboxPhaseAwaitingSource && !time.Now().Before(sourceAwaitDeadline(sb)) {
		return fmt.Errorf("timed out after %s waiting for the client to push the source", sourcePushTimeout)
	}
	sb.SetPhase(model.SandboxPhaseAwaitingSource)
	status := "waiting for the client to push the source"
	sb.StatusMessage = &status
	return nil
}

// sourceAwaitDeadline is when waiting for a client's push stops and the sandbox
// fails.
//
// It is derived, not stored: PhaseChangedAt is stamped once, when the sandbox
// parks, so every reconcile computes the same deadline and can re-arm the wake
// for it. A stored deadline would instead be lost if the process died between
// persisting it and scheduling the wake, and the sandbox would park forever.
func sourceAwaitDeadline(sb *model.Sandbox) time.Time {
	return sb.PhaseChangedAt.Add(sourcePushTimeout)
}

// scheduleSourceAwaitTimeout arranges the reconcile that enforces the deadline.
// A parked sandbox has no other trigger: no server-side state changes while a
// client is pushing, so without this the sandbox would wait forever. Waking
// exactly at the deadline is why the engine's future-dated mark is used instead
// of a periodic sweep over every sandbox.
func (r *SandboxReconciler) scheduleSourceAwaitTimeout(ctx context.Context, sb *model.Sandbox) error {
	if r.engine == nil || sb.PhaseChangedAt.IsZero() {
		return nil
	}
	return r.engine.MarkDirtyAt(ctx, SandboxResourceType, SandboxDirtyID(sb.ProjectID, sb.ID), sourceAwaitDeadline(sb))
}

// awaitingSourcePush reports whether a sandbox is provisioned but cannot start
// yet, because its source arrives by a client push that has not been reported
// complete. Starting the harness now would run it against an empty workspace.
//
// This asks only whether the push has been reported, not what was pushed: the
// commit to check out was fixed at create and never changes.
func awaitingSourcePush(sb *model.Sandbox) bool {
	if sb == nil || sb.Source == nil {
		return false
	}
	if sb.Source.Delivery != model.GitSourceDeliveryPush {
		return false
	}
	return sb.SourceDeliveredAt == nil
}

// sourceNeedsPush reports whether a sandbox's source must be delivered by the
// client pushing into the sandbox's repository, rather than by the sandbox
// cloning the client's directory through a bind mount.
//
// A local source directory is only reachable when the provider runs sandboxes
// on this filesystem *and* the client is on this machine. Neither condition
// implies the other: a Docker provider on a remote server binds happily, just
// not to the caller's files. Comparing host IDs is what separates the two, and
// it works because a co-located CLI and server resolve the same identity from
// the same file.
//
// Unknowns resolve to true. A needless push is slow; a bind of a path the
// sandbox cannot see fails outright.
func sourceNeedsPush(definition sandbox.ProviderDefinition, serverHostID string, origin *model.Origin, source *model.GitSource) bool {
	if source == nil {
		return false
	}
	if source.LocalDirectory == nil || strings.TrimSpace(*source.LocalDirectory) == "" {
		// A remote URL is reachable from anywhere the sandbox has network, so
		// the sandbox clones it directly and no client is involved.
		return false
	}
	if !definition.LocalSourceBind {
		return true
	}
	if strings.TrimSpace(serverHostID) == "" {
		return true
	}
	if origin == nil || strings.TrimSpace(origin.HostID) == "" {
		// A client that reported no origin cannot be placed on this machine,
		// so its paths cannot be assumed to resolve here.
		return true
	}
	return strings.TrimSpace(origin.HostID) != strings.TrimSpace(serverHostID)
}
