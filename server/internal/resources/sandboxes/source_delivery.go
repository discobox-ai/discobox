package sandboxes

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/discobox-ai/discobox/internal/originkey"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
	"github.com/discobox-ai/discobox/server/internal/sandbox"
)

// sourceDataKey identifies the durable pool-local data shared by every sandbox
// using one source. Local paths and remote URLs are already normalized by
// GitSource.Root; the client host identity disambiguates identical paths on
// different machines. An incomplete identity opts the source out rather than
// risking a collision.
func sourceDataKey(hostID string, source *model.GitSource) string {
	if source == nil {
		return ""
	}
	return originkey.Of(hostID, source.Root())
}

// resolveSourceDelivery records how each of a sandbox's sources reaches it, so
// every later stage reads a stated intent instead of re-deriving it. The
// decision needs the provider, the client's origin, and this server's identity
// together, and only create has all three. The provider instance is the one
// backing the sandbox's pool, resolved by the caller.
//
// Every source is decided the same way: the primary one and each source code
// reference alike are local directories the sandbox either binds or cannot see,
// and a reference the sandbox cannot reach is exactly as undeliverable as a
// primary one. The provider is resolved once for all of them.
//
// A client may not ask for push delivery: whether a bind is possible is the
// server's to know, and a client claiming otherwise would either force a
// needless push or assert a reachability it cannot verify.
func (s *Service) resolveSourceDelivery(ctx context.Context, source *model.GitSource, refs model.SourceCodeReferences, origin *model.Origin, providerInstance *model.SandboxProviderInstance) error {
	sources := sandboxGitSources(source, refs)
	if len(sources) == 0 {
		return nil
	}
	for _, entry := range sources {
		if entry.source.Delivery == model.GitSourceDeliveryPush {
			return apperrors.NewStatusError(http.StatusBadRequest,
				"source delivery is decided by the server and cannot be requested")
		}
	}
	provider, err := s.sandboxProviders.ResolveInstance(ctx, providerInstance)
	if err != nil {
		// The instance exists in the store but has not been instantiated, so its
		// reachability is unknown. Leaving delivery at clone is safe only
		// because a sandbox whose provider never instantiates cannot run at all,
		// so no bind is ever attempted. Nothing here can be concluded about a
		// provider that does come up later.
		for _, entry := range sources {
			entry.source.Delivery = model.GitSourceDeliveryClone
			entry.store()
		}
		return nil //nolint:nilerr // an uninstantiated provider is not a create failure
	}
	for _, entry := range sources {
		if sourceNeedsPush(provider.Definition(), s.hostID, origin, entry.source) {
			entry.source.Delivery = model.GitSourceDeliveryPush
		} else {
			entry.source.Delivery = model.GitSourceDeliveryClone
		}
		entry.store()
	}
	return nil
}

// gitSourceEntry is one of a sandbox's sources, addressable for mutation. A
// source code reference lives in a map, whose values cannot be taken the
// address of, so store writes the mutated copy back under its key; for the
// primary source, which is already a pointer, it does nothing.
type gitSourceEntry struct {
	source *model.GitSource
	store  func()
}

// sandboxGitSources is every source a sandbox materializes, primary first.
func sandboxGitSources(source *model.GitSource, refs model.SourceCodeReferences) []gitSourceEntry {
	var out []gitSourceEntry
	if source != nil {
		out = append(out, gitSourceEntry{source: source, store: func() {}})
	}
	keys := make([]string, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		ref := refs[key]
		out = append(out, gitSourceEntry{source: &ref, store: func() { refs[key] = ref }})
	}
	return out
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
// the caller reads the state. An expired wait is the exception: it returns an
// error, which the caller records as a failure.
func parkForSourcePush(sb *model.Sandbox) error {
	// Check before parking: once SetState is a no-op for an already-parked
	// sandbox, the deadline below is the one set when it first parked.
	if sb.State == model.SandboxStateAwaitingSource && !time.Now().Before(sourceAwaitDeadline(sb)) {
		return fmt.Errorf("timed out after %s waiting for the client to push the source", sourcePushTimeout)
	}
	sb.SetState(model.SandboxStateAwaitingSource)
	return nil
}

// sourceAwaitDeadline is when waiting for a client's push stops and the sandbox
// fails.
//
// It is derived, not stored: StateChangedAt is stamped once, when the sandbox
// parks, so every reconcile computes the same deadline and can re-arm the wake
// for it. A stored deadline would instead be lost if the process died between
// persisting it and scheduling the wake, and the sandbox would park forever.
func sourceAwaitDeadline(sb *model.Sandbox) time.Time {
	return sb.StateChangedAt.Add(sourcePushTimeout)
}

// armSourceAwaitTimeout is the reconcile that enforces the deadline, expressed
// as the engine's timer. A parked sandbox has no other trigger: no server-side
// state changes while a client is pushing, so without this the sandbox would
// wait forever. Waking exactly at the deadline is why the timer is used instead
// of a periodic sweep over every sandbox.
func armSourceAwaitTimeout(sb *model.Sandbox) reconcile.Result {
	if sb.StateChangedAt.IsZero() {
		return reconcile.Result{}
	}
	return reconcile.RequeueAt(sourceAwaitDeadline(sb))
}

// awaitingSourcePush reports whether a sandbox is provisioned but cannot start
// yet, because at least one of its sources arrives by a client push that has
// not been reported complete. Starting the harness now would run it against a
// workspace missing that source.
//
// Delivery is reported once for the whole sandbox, not per source: the client
// pushes every push-delivered source and then reports them together
// (CompleteSandboxSourcePush), so one timestamp answers for all of them.
//
// This asks only whether the push has been reported, not what was pushed: the
// commit to check out was fixed at create and never changes.
func awaitingSourcePush(sb *model.Sandbox) bool {
	if sb == nil {
		return false
	}
	if len(pushDeliveredSources(sb)) == 0 {
		return false
	}
	return sb.SourceDeliveredAt == nil
}

// pushDeliveredSources is every source of a sandbox the client must push,
// primary first. A sandbox with none of them never parks.
func pushDeliveredSources(sb *model.Sandbox) []gitSourceEntry {
	if sb == nil {
		return nil
	}
	var out []gitSourceEntry
	for _, entry := range sandboxGitSources(sb.Source, sb.SourceCodeReferences) {
		if entry.source.Delivery == model.GitSourceDeliveryPush {
			out = append(out, entry)
		}
	}
	return out
}

// sourceNeedsPush reports whether a sandbox's source must be delivered by the
// client pushing into the sandbox's repository, rather than by the sandbox
// cloning the client's directory through a bind mount.
//
// A local source directory is only reachable when the provider exposes that
// path to its sandboxes *and* the client is on this machine. Neither condition
// implies the other: a Docker provider on a remote server binds happily, just
// not to the caller's files, and one on this machine binds only the host
// directories it was configured with. Comparing host IDs is what separates the
// first pair, and it works because a co-located CLI and server resolve the same
// identity from the same file; the provider's local source roots settle the
// second.
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
	if source.NoLocalRepository {
		// The directory is in no repository, so there is nothing at that path
		// to clone even from this machine. Only the client holds this source,
		// in a repository of its own, and only a push can deliver it. This is
		// not the client asking for a push: it reported what its filesystem
		// holds, which the server cannot see, and the conclusion is drawn here.
		return true
	}
	if !localSourceRootsCover(definition.LocalSourceRoots, *source.LocalDirectory) {
		// Either the provider reaches none of this filesystem, or it reaches it
		// somewhere other than where this directory lives. A provider that
		// mounts /home cannot clone /workspace/source, and the clone fails in
		// the pool agent long after this decision if it is made anyway.
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

// localSourceRootsCover reports whether a local source directory lies under one
// of the host paths a provider exposes to its sandboxes.
//
// Containment is by path element, so /home covers /home/darren/src but /home-old
// is not covered by /home. A root of "/" covers everything, which is how a
// provider that shares its whole filesystem says so. Anything that is not an
// absolute path — on either side — is covered by nothing: the comparison is
// between two host paths, and a relative one names no place.
func localSourceRootsCover(roots []string, directory string) bool {
	directory = filepath.Clean(strings.TrimSpace(directory))
	if !filepath.IsAbs(directory) {
		return false
	}
	for _, root := range roots {
		root = filepath.Clean(strings.TrimSpace(root))
		if !filepath.IsAbs(root) {
			continue
		}
		// A root is the whole filesystem when it is a volume's root: "/" on
		// POSIX, where VolumeName is empty, and "C:\\" on Windows, where a bare
		// separator is not a root at all.
		if root == filepath.VolumeName(root)+string(filepath.Separator) || directory == root ||
			strings.HasPrefix(directory, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
