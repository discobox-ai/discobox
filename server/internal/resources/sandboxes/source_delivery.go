package sandboxes

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/discobox-ai/discobox/internal/originkey"
	"github.com/discobox-ai/discobox/pkg/platform"
	"github.com/discobox-ai/discobox/pkg/sandboxconfig"
	"github.com/discobox-ai/discobox/pkg/sandboxpath"
	"github.com/discobox-ai/discobox/pkg/sourcecache"
	"github.com/discobox-ai/discobox/server/internal/apperrors"
	"github.com/discobox-ai/discobox/server/internal/model"
	"github.com/discobox-ai/discobox/server/internal/reconcile"
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
// every later stage reads a stated intent instead of re-deriving it. Nothing
// is chosen: every Git source of a sandbox is delivered by a client into the
// pool's cache for its remote (ADR 26-10-10-545 §1), the primary one and each
// source code reference alike. What is decided here is which cache.
//
// A client may not name a delivery or a cache: both are the server's record.
func resolveSourceDelivery(sandboxID string, source *model.GitSource, refs model.SourceCodeReferences, origin *model.Origin) error {
	for _, entry := range sandboxGitSources(source, refs) {
		if entry.source.Delivery != "" {
			return apperrors.NewStatusError(http.StatusBadRequest,
				"source delivery is decided by the server and cannot be requested")
		}
		slug := ""
		if entry.source.Slug != nil {
			slug = strings.TrimSpace(*entry.source.Slug)
		}
		if slug == "" {
			return apperrors.NewStatusError(http.StatusBadRequest, "a source has no slug to name its cache namespace by")
		}
		key := sourceCacheKey(sandboxID, slug, origin, entry.source)
		entry.source.Delivery = model.GitSourceDeliveryCache
		entry.source.CacheKey = &key
		entry.store()
	}
	return nil
}

// validateSkillsSources refuses skills sources that do not match the sources
// they name (ADR 26-10-14-294 §2): the primary source is what the sandbox is
// worked on in and is never one; every skills directory names a source code
// reference whose purpose is skills, at a clean path inside it; and every such
// reference is named, since one with no directory would be checked out for
// nothing. A purpose is one of model.GitSourcePurposes: a create's is decoded
// against the API's enum, but an imported archive's is not. An explicit work
// purpose is recorded as absent, the one spelling every source created before
// purposes has.
func validateSkillsSources(source *model.GitSource, refs model.SourceCodeReferences, skillsSources []model.SkillsSource) error {
	for _, entry := range sandboxGitSources(source, refs) {
		if purpose := entry.source.Purpose; purpose != "" && !slices.Contains(model.GitSourcePurposes, purpose) {
			return apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("source purpose %q is not one of %v", purpose, model.GitSourcePurposes))
		}
	}
	if source != nil {
		if source.IsSkills() {
			return apperrors.NewStatusError(http.StatusBadRequest, "the primary source cannot be a skills source")
		}
		source.Purpose = ""
	}
	named := map[string]bool{}
	for _, skills := range skillsSources {
		ref, ok := refs[skills.Source]
		if !ok || !ref.IsSkills() {
			return apperrors.NewStatusError(http.StatusBadRequest,
				fmt.Sprintf("skills source %q is not a source code reference whose purpose is skills", skills.Source))
		}
		if err := sandboxconfig.ValidateSkillsDirectory(skills.Directory); err != nil {
			return apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("skills source %q: %v", skills.Source, err))
		}
		named[skills.Source] = true
	}
	for key, ref := range refs {
		if ref.IsSkills() && !named[key] {
			return apperrors.NewStatusError(http.StatusBadRequest,
				fmt.Sprintf("source %q is a skills source that no skills source directory names", key))
		}
		if ref.Purpose == model.GitSourcePurposeWork {
			ref.Purpose = ""
			refs[key] = ref
		}
	}
	return nil
}

// validateSourceWorkingDirectory refuses a primary source whose working
// directory is not an absolute path, by the sandbox's platform, at or below the
// directory the same request names for the source (ADR 26-10-15-417).
//
// The client hands the server's record to host tools: it is the folder an
// editor's remote URL opens (`ssh-remote+<host><workdir>`) and `{workdir}` in
// a tool's command. Anything not absolute could name a host there instead of a
// path, and the request is not trusted to be the client's own: a box can create
// boxes (ADR 26-10-14-320), and an archive is anyone's. The pool once refused a
// relative one, because Docker did as a WorkingDir; now it only ignores one
// outside the source when it places the start directory (ADR 26-10-15-664 §3),
// so the check lives where it is stored. The client guards the same sink for
// every path it hands a host tool (sandboxSSHFolder); this is the second line.
//
// It checks the record against itself, not against where the source lands:
// placement is the pool's to decide (normalizeSandboxConfig), and this does not
// restate it. Paths are compared cleaned, so a repository whose path has a
// space in it passes, as the client produces such paths.
//
// A sandbox whose pool has not declared its platform yet is judged by either
// rule set, so a create is not refused for its pool being new.
func validateSourceWorkingDirectory(source *model.GitSource, sandboxPlatform platform.Platform) error {
	if source == nil || source.Destination == nil || source.Destination.WorkingDirectory == nil {
		return nil
	}
	workingDirectory := *source.Destination.WorkingDirectory
	rules := sandboxPathRules(sandboxPlatform)
	for _, paths := range rules {
		if sourceWorkingDirectoryInside(paths, source.Destination.Directory, workingDirectory) {
			return nil
		}
	}
	return apperrors.NewStatusError(http.StatusBadRequest,
		fmt.Sprintf("source working directory %q is not an absolute path inside the source's directory", workingDirectory))
}

// sandboxPathRules are the path rules a sandbox of sandboxPlatform is judged
// by: its own, or, for a sandbox whose pool has not declared its platform yet,
// each it may turn out to be, so a create is not refused for its pool being
// new.
func sandboxPathRules(sandboxPlatform platform.Platform) []sandboxpath.Paths {
	if sandboxPlatform.IsZero() {
		return []sandboxpath.Paths{
			sandboxpath.For(platform.Platform{OS: "linux"}),
			sandboxpath.For(platform.Platform{OS: "windows"}),
		}
	}
	return []sandboxpath.Paths{sandboxpath.For(sandboxPlatform)}
}

// validateSourceDirectories refuses a source placed where the sandbox could
// not place it (ADR 26-11-01-692 §1): below home, unless it is a skills
// source, and a skills source anywhere but below home or at a clean absolute
// path. Every directory a source names is checked, its reference key and its
// destination's, since the pool reads both. The primary source is never a
// skills source (validateSkillsSources), so it is never below home.
func validateSourceDirectories(source *model.GitSource, refs model.SourceCodeReferences, sandboxPlatform platform.Platform) error {
	rules := sandboxPathRules(sandboxPlatform)
	if source != nil && source.Destination != nil && source.Destination.Directory != nil {
		if err := sandboxconfig.ValidateSourceDirectory(*source.Destination.Directory, false, rules...); err != nil {
			return apperrors.NewStatusError(http.StatusBadRequest, "source directory "+err.Error())
		}
	}
	for key, ref := range refs {
		skills := ref.IsSkills()
		if err := sandboxconfig.ValidateSourceDirectory(key, skills, rules...); err != nil {
			return apperrors.NewStatusError(http.StatusBadRequest, "source code reference "+err.Error())
		}
		if ref.Destination != nil && ref.Destination.Directory != nil {
			if err := sandboxconfig.ValidateSourceDirectory(*ref.Destination.Directory, skills, rules...); err != nil {
				return apperrors.NewStatusError(http.StatusBadRequest, fmt.Sprintf("source code reference %q directory %v", key, err))
			}
		}
	}
	return nil
}

// sourceWorkingDirectoryInside reports whether workingDirectory is absolute by
// paths, holds no control character, and lies at or below directory when the
// request names one.
func sourceWorkingDirectoryInside(paths sandboxpath.Paths, directory *string, workingDirectory string) bool {
	if !paths.IsAbs(workingDirectory) || strings.IndexFunc(workingDirectory, unicode.IsControl) >= 0 {
		return false
	}
	if directory == nil || strings.TrimSpace(*directory) == "" {
		return true
	}
	if !paths.IsAbs(*directory) {
		return false
	}
	root := paths.Clean(*directory)
	for p := paths.Clean(workingDirectory); ; {
		if p == root {
			return true
		}
		parent := paths.Dir(p)
		if parent == p {
			return false
		}
		p = parent
	}
}

// sourceCacheKey names the pool cache a source is delivered into (ADR
// 26-10-10-545 §2): its remote's, when it has one; else its directory's on
// its client's host; else one of its own.
//
// The host identity is deliberately not part of a remote's key. Members of a
// project are trusted with each other's source, and sharing a remote's cache
// between them is where a team's reuse is. The identity is a sandbox's own
// claim when a sandbox creates a discobox, and so is the remote; that is
// harmless, because a sandbox reaches nothing in a cache but what it sent
// (§7), whichever cache it names.
func sourceCacheKey(sandboxID, slug string, origin *model.Origin, source *model.GitSource) string {
	if key := sourcecache.RemoteKey(sourceRemote(source)); key != "" {
		return key
	}
	hostID := ""
	if origin != nil {
		hostID = strings.TrimSpace(origin.HostID)
	}
	if hostID != "" {
		if key := sourcecache.DirectoryKey(sourceDataKey(hostID, source)); key != "" {
			return key
		}
	}
	return sourcecache.SandboxKey(sandboxID, slug)
}

// sourceRemote is a source's remote: the URL of a remote-URL source, and the
// remote its client reported for a local checkout.
func sourceRemote(source *model.GitSource) string {
	if source == nil {
		return ""
	}
	if source.LocalDirectory == nil || strings.TrimSpace(*source.LocalDirectory) == "" {
		if source.URL != nil {
			return strings.TrimSpace(*source.URL)
		}
		return ""
	}
	if source.UpstreamURL != nil {
		return strings.TrimSpace(*source.UpstreamURL)
	}
	return ""
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
	if len(clientDeliveredSources(sb)) == 0 {
		return false
	}
	return sb.SourceDeliveredAt == nil
}

// clientDeliveredSources is every source of a sandbox a client delivers,
// primary first: one delivered into a pool cache, and one pushed into the
// repository an earlier pool made for its sandbox (ADR 0058). A sandbox with
// none of them never parks.
//
// A local source recorded as clone was served live from the developer's Git
// directory, which no pool does now (ADR 26-10-10-545 §8). A sandbox that ran
// has cloned it and is owed nothing. One created before that changed and not
// yet run has nothing to clone until a client pushes, so it waits for one as
// a pushed source does.
func clientDeliveredSources(sb *model.Sandbox) []gitSourceEntry {
	if sb == nil {
		return nil
	}
	var out []gitSourceEntry
	for _, entry := range sandboxGitSources(sb.Source, sb.SourceCodeReferences) {
		switch entry.source.Delivery {
		case model.GitSourceDeliveryPush, model.GitSourceDeliveryCache:
			out = append(out, entry)
		default:
			local := entry.source.LocalDirectory != nil && strings.TrimSpace(*entry.source.LocalDirectory) != ""
			if local && sandboxHasNeverRun(sb.State) {
				out = append(out, entry)
			}
		}
	}
	return out
}
