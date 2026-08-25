package sandboxcreate

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxgit"
	"github.com/discobox-ai/x/gitutil"
)

// sourceDeliveryClient is the control-plane surface a push needs: the phase to
// wait on, and the call that reports the push complete.
type sourceDeliveryClient interface {
	GetSandbox(context.Context, apiclientgen.GetSandboxParams) (apiclientgen.GetSandboxRes, error)
	CompleteSandboxSourcePush(context.Context, *apimodel.CompleteSandboxSourcePushBody, apiclientgen.CompleteSandboxSourcePushParams) (apiclientgen.CompleteSandboxSourcePushRes, error)
}

const (
	// awaitSourcePollInterval paces the wait for the sandbox to be provisioned
	// far enough to receive a push.
	//
	// It is also how often the phase this wait narrates is re-read. A pull
	// publishes twice a second, so a second's granularity is at most one report
	// behind — which is a byte counter that moves once a second rather than
	// twice, and half the requests over a wait that can run for minutes.
	awaitSourcePollInterval = time.Second
	// awaitSourceStall bounds that wait by silence rather than by total elapsed
	// time; see StallClock for why. A client that gives up leaves the sandbox
	// to the server's own timeout.
	awaitSourceStall = 5 * time.Minute
)

// pushDeliveredSource is one source of a sandbox the server expects this client
// to push, and the key it was created under: empty for the primary source, and
// the source code reference key for a reference.
type pushDeliveredSource struct {
	key    string
	source apimodel.GitSource
}

// pushDeliveredSources are the sandbox's sources the server expects this client
// to deliver by pushing them, primary first.
//
// The server decides this, not the client: it knows whether the sandbox's
// provider can reach the caller's filesystem, and it decides it per source — a
// reference can need a push while the primary source is bound, and the other
// way round. The client only reads the answers.
func pushDeliveredSources(sandbox *apimodel.Sandbox) []pushDeliveredSource {
	if sandbox == nil {
		return nil
	}
	var out []pushDeliveredSource
	if source, ok := sandbox.Config.Source.Get(); ok && sourceAwaitsPush(source) {
		out = append(out, pushDeliveredSource{source: source})
	}
	references, ok := sandbox.Config.SourceCodeReferences.Get()
	if !ok {
		return out
	}
	keys := make([]string, 0, len(references))
	for key := range references {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if source := references[key]; sourceAwaitsPush(source) {
			out = append(out, pushDeliveredSource{key: key, source: source})
		}
	}
	return out
}

func sourceAwaitsPush(source apimodel.GitSource) bool {
	delivery, ok := source.Delivery.Get()
	return ok && delivery == apiclientgen.GitSourceDeliveryPush
}

// DeliverSource pushes a sandbox's push-delivered sources into the origin
// repositories they will fetch from and reports the pushes complete, returning
// once the sandbox is free to start. Each push runs out of the local source the create resolved,
// which is the only place a throwaway repository's commits exist.
//
// Every push-delivered source is pushed before any of them is reported: the
// sandbox resumes on that report, and resuming it while a source is still
// missing would start the harness against an incomplete workspace.
//
// It is a no-op unless the server asked for a push, so callers can invoke it
// unconditionally after create.
func DeliverSource(ctx context.Context, client sourceDeliveryClient, projectID string, sandbox *apimodel.Sandbox, local *LocalSources, serverURL, token string, report Report) error {
	pending := pushDeliveredSources(sandbox)
	if len(pending) == 0 {
		return nil
	}
	// The origin repositories only exist once the sandbox is provisioned, so
	// there is nothing to push into until it parks.
	report.step(StepAwaitingSource)
	if err := awaitSourceRequested(ctx, client, projectID, sandbox.ID, report); err != nil {
		return err
	}
	// One step for the push as a whole rather than one per repository: a
	// sandbox cut from several sources pushes them back to back, and naming
	// each would be a line that changes faster than it can be read.
	report.step(StepPushingSource)
	pushed := make(apiclientgen.CompleteSandboxSourcePushBodySources, len(pending))
	for _, entry := range pending {
		commit, branch, snapshotRef, err := pushRefs(entry.source)
		if err != nil {
			return err
		}
		slug := strings.TrimSpace(entry.source.Slug.Or(""))
		if slug == "" {
			return fmt.Errorf("discobox source has no slug to address its repository")
		}
		repoRoot, err := local.pushRoot(entry.key)
		if err != nil {
			return err
		}
		originURL, err := sandboxgit.OriginURL(serverURL, projectID, sandbox.ID, entry.source)
		if err != nil {
			return err
		}
		if err := pushSource(ctx, repoRoot, originURL, token, commit, branch, snapshotRef); err != nil {
			return err
		}
		// The commit just delivered is the lease every later `discobox push` of this
		// source leases against (ADR 0058 §6). A failure to record it must not
		// fail the create: the source is delivered either way, and the only cost
		// is that the next push has no lease to hold.
		_ = gitutil.UpdateRef(ctx, repoRoot, sandboxgit.OriginLeaseRef(sandbox.ID, slug, pushBranch(branch)), commit)
		pushed[slug] = commit
	}
	return completeSourcePush(ctx, client, projectID, sandbox.ID, pushed)
}

// pushRefs reads what to push from the source the server recorded. The commit
// is the one this client resolved at create, so pushing anything else would
// deliver something the sandbox is not configured to check out.
func pushRefs(source apimodel.GitSource) (commit, branch, snapshotRef string, err error) {
	checkout, ok := source.Checkout.Get()
	if !ok {
		return "", "", "", fmt.Errorf("discobox source does not name a commit to push")
	}
	commit = strings.TrimSpace(checkout.Commit.Or(""))
	if commit == "" {
		return "", "", "", fmt.Errorf("discobox source does not name a commit to push")
	}
	if strings.TrimSpace(checkout.RefType.Or("")) == runSourceRefTypeBranch {
		branch = strings.TrimSpace(checkout.RefName.Or(""))
	}
	if workspace, ok := source.Workspace.Get(); ok {
		if workspace.Mode.Or(apiclientgen.GitSourceWorkspaceModeClean) == apiclientgen.GitSourceWorkspaceModeDirty {
			snapshotRef = strings.TrimSpace(workspace.SnapshotRef.Or(""))
			if snapshotRef == "" {
				return "", "", "", fmt.Errorf("dirty workspace does not name a snapshot ref to push")
			}
		}
	}
	return commit, branch, snapshotRef, nil
}

// pushRoot is the repository a push of the source filed under key runs out of.
// Only a local source is ever pushed — a remote one is cloned by the sandbox
// itself and resolves no repository here — so a source without one is a sandbox
// that should never have been asked for a push.
func (s *LocalSources) pushRoot(key string) (string, error) {
	if s != nil {
		for _, source := range s.sources {
			if source.key == key && strings.TrimSpace(source.repoRoot) != "" {
				return source.repoRoot, nil
			}
		}
	}
	if key == "" {
		return "", fmt.Errorf("the discobox expects a source push, but its source was not resolved from a local repository")
	}
	return "", fmt.Errorf("the discobox expects a push for source %s, but it was not resolved from a local repository", key)
}

// awaitSourceRequested waits until the sandbox is parked waiting for its
// source. Pushing earlier would race the repository into existence.
//
// The wait narrates itself out of the reads it is already making. What it is
// waiting for is the provisioning that has to finish before the sandbox can
// park — the image pull above all — and the pool agent records that on the
// sandbox as it happens (ADR 0060). Every poll already holds the whole record,
// so saying what the wait is for costs nothing but reading a field that arrived
// anyway; without it, the longest wait in a create is the one that says the
// least about itself.
func awaitSourceRequested(ctx context.Context, client sourceDeliveryClient, projectID, sandboxID string, report Report) error {
	stall := NewStallClock(awaitSourceStall)
	// last is what the caller's line says, which starting out is the step
	// reported just above. Comparing against that rather than against nothing is
	// what makes a report mean the line changed.
	last := StepAwaitingSource
	for {
		res, err := client.GetSandbox(ctx, apiclientgen.GetSandboxParams{ProjectId: projectID, SandboxId: sandboxID})
		if err != nil {
			return err
		}
		sandbox, err := expectSandbox(res)
		if err != nil {
			return err
		}
		switch sandbox.Runtime.State {
		case apiclientgen.SandboxRuntimeStateAwaitingSource:
			return nil
		case apiclientgen.SandboxRuntimeStateFailed:
			return fmt.Errorf("discobox failed before it could receive its source: %s", sandbox.Runtime.ErrorMessage.Or("unknown error"))
		}
		// A sandbox with nothing left to provision reports no phase, which
		// leaves the previous line standing rather than blanking it: the wait is
		// still on, and the client's own step is the truest thing left to say.
		if phase := ProvisionStatus(sandbox); phase != "" && phase != last {
			last = phase
			report.step(phase)
			// Something happened, so the clock starts again.
			stall.Progressed()
		}
		if stall.Expired() {
			return fmt.Errorf("gave up after %s with no further progress toward the discobox being ready for its source (last: %s)", awaitSourceStall, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(awaitSourcePollInterval):
		}
	}
}

// pushSource sends the commit, and any workspace snapshot, into the source's
// origin repository, which the sandbox then clones (ADR 0058 §4).
//
// The commit is pushed to its branch by explicit refspec rather than by pushing
// the local branch: the local branch may have moved on since create, and the
// sandbox must receive the commit its source names. The snapshot ref carries a
// dirty workspace's uncommitted changes, which the sandbox re-applies on top.
func pushSource(ctx context.Context, repoRoot, repoURL, token, commit, branch, snapshotRef string) error {
	refspecs := make([]string, 0, 2)
	refspecs = append(refspecs, commit+":refs/heads/"+pushBranch(branch))
	if snapshotRef != "" {
		refspecs = append(refspecs, "+"+snapshotRef+":"+snapshotRef)
	}
	args := []string{"push", repoURL}
	args = append(args, refspecs...)
	if _, err := gitutil.Output(ctx, repoRoot, nil, nil, sandboxgit.AuthArgs(token, args)...); err != nil {
		return fmt.Errorf("push source to discobox: %w", err)
	}
	return nil
}

// pushBranch is the branch a commit lands on in the origin repository: the
// source's own branch, or the conventional one for a source that names none —
// checked out at a bare commit or tag. The sandbox checks out the commit itself;
// this only gives the push somewhere to land, and gives the origin a HEAD.
func pushBranch(branch string) string {
	if branch != "" {
		return branch
	}
	return sandboxgit.SourcePushBranch
}

func completeSourcePush(ctx context.Context, client sourceDeliveryClient, projectID, sandboxID string, pushed apiclientgen.CompleteSandboxSourcePushBodySources) error {
	res, err := client.CompleteSandboxSourcePush(ctx,
		&apimodel.CompleteSandboxSourcePushBody{Sources: pushed},
		apiclientgen.CompleteSandboxSourcePushParams{ProjectId: projectID, SandboxId: sandboxID})
	if err != nil {
		return err
	}
	if _, ok := res.(*apimodel.Sandbox); ok {
		return nil
	}
	return createResponseError(res)
}

func expectSandbox(res any) (*apimodel.Sandbox, error) {
	if sandbox, ok := res.(*apimodel.Sandbox); ok {
		return sandbox, nil
	}
	return nil, createResponseError(res)
}

// PendingSourcePush is one source a discobox is waiting to be given: the slug
// that addresses its repository, and the key its local repository has to be
// filed under for DeliverSource to find it.
type PendingSourcePush struct {
	// Key files this source's repository in a LocalSources: empty for the
	// primary source, and the source code reference key for a reference.
	Key string
	// Slug names the source's repository in the discobox, and is what a --dir
	// override addresses.
	Slug string
	// Source is what the discobox was created against: the commit it will check
	// out, and the workspace snapshot it will restore on top of it.
	Source apimodel.GitSource
}

// PendingSourcePushes are the sources a discobox expects this client to deliver
// by pushing them, primary first. It is empty for a discobox that reads or
// clones everything it needs, which is every discobox with nothing to wait for.
//
// A create delivers these itself, out of the repositories it just resolved.
// This is for the other way in: a discobox parked in awaiting_source long after
// that create — because the push failed, or the pool was wedged when it ran —
// which can be delivered again from the repositories still on this machine.
func PendingSourcePushes(sandbox *apimodel.Sandbox) []PendingSourcePush {
	pending := pushDeliveredSources(sandbox)
	if len(pending) == 0 {
		return nil
	}
	out := make([]PendingSourcePush, 0, len(pending))
	for _, entry := range pending {
		out = append(out, PendingSourcePush{
			Key:    entry.key,
			Slug:   strings.TrimSpace(entry.source.Slug.Or("")),
			Source: entry.source,
		})
	}
	return out
}

// NewLocalSources files repositories that already exist on this machine as the
// sources a delivery pushes out of, keyed as PendingSourcePush.Key gives them.
//
// Unlike the LocalSources a create builds, these own nothing: every repository
// here was on this machine before the call and stays after it, so Close
// releases nothing.
func NewLocalSources(roots map[string]string) *LocalSources {
	local := &LocalSources{}
	keys := make([]string, 0, len(roots))
	for key := range roots {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		local.sources = append(local.sources, localSource{key: key, repoRoot: roots[key]})
	}
	return local
}

// CheckDeliverable reports whether source can still be delivered out of
// repoRoot, so a delivery that cannot finish is refused before any of it is
// pushed rather than halfway through.
//
// What a discobox checks out was fixed when it was created, so delivering it
// later means finding those exact objects here: the commit, and for a discobox
// created from a dirty working tree the snapshot commit holding those
// uncommitted changes. Either can be gone — a branch deleted, a snapshot ref
// pruned — and the source that was created from a directory with no repository
// of its own is always gone, because the repository built over it existed only
// for that run (ADR 0045).
func CheckDeliverable(ctx context.Context, repoRoot string, source apimodel.GitSource) error {
	slug := strings.TrimSpace(source.Slug.Or(""))
	if source.NoLocalRepository.Or(false) {
		return fmt.Errorf("source %q was created from a directory that is not a Git repository, so it was delivered out of a repository built for that run and deleted afterwards; nothing on this machine holds those commits now", slug)
	}
	commit, _, snapshotRef, err := pushRefs(source)
	if err != nil {
		return err
	}
	if _, err := gitutil.Output(ctx, repoRoot, nil, nil, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return fmt.Errorf("source %q is pinned to commit %s, which is not in %s anymore", slug, commit, repoRoot)
	}
	if snapshotRef == "" {
		return nil
	}
	if _, err := gitutil.Output(ctx, repoRoot, nil, nil, "rev-parse", "--verify", "--quiet", snapshotRef+"^{commit}"); err != nil {
		return fmt.Errorf("source %q carried uncommitted changes, and the snapshot holding them (%s) is gone from %s", slug, snapshotRef, repoRoot)
	}
	return nil
}
