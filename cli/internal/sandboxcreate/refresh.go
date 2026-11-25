package sandboxcreate

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxgit"
	"github.com/discobox-ai/discobox/pkg/sourcecache"
	"github.com/discobox-ai/x/gitutil"
)

// A refresh brings a delivered remote source's pool copy of its remote — its
// namespace's refs/remotes/origin/* and tags — up to the remote, and moves
// nothing else (ADR 26-11-24-142). It takes the order a first delivery takes
// (ADR 26-10-23-948): the pool pulls anonymously, and only when the remote
// refuses that does the client's Git credential policy decide between
// lending a credential for the pool's pull and fetching on this machine to
// push what it fetched.

// RefreshCommand is what the box runs to take a refresh into its checkout's
// origin/* (§1). The default refspec of its discobox remote is left as it
// is, so a plain `git fetch discobox` never rewinds an origin/* the box
// fetched from the remote itself.
const RefreshCommand = "git fetch discobox '+refs/remotes/origin/*:refs/remotes/origin/*'"

// Who refreshed a source.
const (
	RefreshedByPool      = "pool"
	RefreshedByPoolLent  = "pool, with a lent credential"
	RefreshedThisMachine = "this machine"
)

// RefreshOptions are what a refresh is narrowed to and how it may ask.
type RefreshOptions struct {
	// Branch narrows the refresh to the remote's one branch, and its tags.
	Branch string
	// Delivery is the credential policy and what this frontend can ask.
	Delivery DeliveryOptions
	Report   Report
}

// RefreshResult is what a refresh did.
type RefreshResult struct {
	// By is who reached the remote: RefreshedByPool, RefreshedByPoolLent or
	// RefreshedThisMachine.
	By string
	// Unreachable reports that the pool could not reach the remote, so this
	// machine refreshed it.
	Unreachable bool
	// Updates are the refs moved, by their names in the namespace; none when
	// it was already current.
	Updates []sourcecache.RefUpdate
}

// IsRemoteSource reports whether a source is a remote this client refreshes
// rather than pushes to: one that names a remote URL and no directory on any
// client's machine, delivered into its pool's cache.
func IsRemoteSource(source apimodel.GitSource) bool {
	return remoteURLSource(source) != "" && deliveredToCache(source)
}

// RefreshSource refreshes one delivered remote source of a sandbox.
func RefreshSource(ctx context.Context, client sourceDeliveryClient, serverURL, token, projectID, sandboxID string, source apimodel.GitSource, options RefreshOptions) (RefreshResult, error) {
	remote := remoteURLSource(source)
	slug := strings.TrimSpace(source.Slug.Or(""))
	if !IsRemoteSource(source) || slug == "" {
		return RefreshResult{}, fmt.Errorf("the source is not a remote delivered into its pool's cache, so there is nothing to refresh")
	}
	r := refresher{client: client, serverURL: serverURL, token: token, projectID: projectID, sandboxID: sandboxID, source: source, slug: slug, remote: remote, options: options}
	report := options.Report
	if report != nil {
		report(Step("asking the pool to refresh " + displaySafe(remote)))
	}
	answer, err := r.pull(ctx, nil)
	if err != nil {
		return RefreshResult{}, err
	}
	switch {
	case answer.Status == apiclientgen.SourceRefreshStatusRefreshed:
		return RefreshResult{By: RefreshedByPool, Updates: refUpdates(answer)}, nil
	case answer.Status == apiclientgen.SourceRefreshStatusMissingBranch:
		return RefreshResult{}, fmt.Errorf("%s has no branch %q", displaySafe(remote), options.Branch)
	case answer.Status == apiclientgen.SourceRefreshStatusUnreachable:
		return r.local(ctx, true)
	}
	// needs_credential: the client's policy decides, as for a delivery.
	lend, err := chooseCredentialDelivery(ctx, options.Delivery, remote, answer.Host.Or("")+"/"+answer.Path.Or(""))
	if err != nil {
		return RefreshResult{}, err
	}
	if lend {
		result, lent, err := r.lend(ctx, credentialTarget{Protocol: answer.Protocol.Or("https"), Host: answer.Host.Or(""), Path: answer.Path.Or("")})
		if err != nil || lent {
			return result, err
		}
		if report != nil {
			report(Step("no credential to lend; fetching on this machine"))
		}
	}
	return r.local(ctx, false)
}

// refresher is one source's refresh.
type refresher struct {
	client                                 sourceDeliveryClient
	serverURL, token, projectID, sandboxID string
	source                                 apimodel.GitSource
	slug, remote                           string
	options                                RefreshOptions
}

// pull asks the pool to refresh, lending credential when there is one.
func (r refresher) pull(ctx context.Context, credential *filledCredential) (*apiclientgen.SourceRefresh, error) {
	body := &apimodel.PullSandboxSourceBody{}
	if credential != nil {
		body = credential.body()
	}
	if r.options.Branch != "" {
		body.Branch = apiclientgen.NewOptString(r.options.Branch)
	}
	res, err := r.client.PullSandboxSource(ctx, body, apiclientgen.PullSandboxSourceParams{ProjectId: r.projectID, SandboxId: r.sandboxID, Slug: r.slug})
	if err != nil {
		return nil, fmt.Errorf("ask the pool to refresh %s: %w", displaySafe(r.remote), err)
	}
	switch res := res.(type) {
	case *apiclientgen.SourceRefresh:
		return res, nil
	case *apimodel.Sandbox:
		// What a server answers that took the pull for a delivery: the
		// source was not delivered when it read it.
		return nil, fmt.Errorf("the discobox was still waiting for %s, so there was nothing to refresh yet", displaySafe(r.remote))
	case *apiclientgen.ErrorModelStatusCode:
		return nil, fmt.Errorf("the server refused to refresh %s (%d): %s", displaySafe(r.remote), res.StatusCode, displaySafe(res.Response.Detail.Or(res.Response.Title.Or(""))))
	}
	return nil, fmt.Errorf("the server answered a refresh of %s with %T", displaySafe(r.remote), res)
}

// lend has the pool refresh with this machine's credential for target. lent
// is false when there was no credential to lend, or the remote refused every
// one the helper had.
func (r refresher) lend(ctx context.Context, target credentialTarget) (result RefreshResult, lent bool, err error) {
	for attempt := 0; attempt < 3; attempt++ {
		credential, err := fillCredential(ctx, r.options.Delivery, target)
		if err != nil || credential == nil {
			return RefreshResult{}, false, err
		}
		answer, err := r.pull(ctx, credential)
		if err != nil {
			return RefreshResult{}, false, err
		}
		switch answer.Status {
		case apiclientgen.SourceRefreshStatusRefreshed:
			credential.settle(ctx, true)
			return RefreshResult{By: RefreshedByPoolLent, Updates: refUpdates(answer)}, true, nil
		case apiclientgen.SourceRefreshStatusMissingBranch:
			credential.settle(ctx, true)
			return RefreshResult{}, false, fmt.Errorf("%s has no branch %q", displaySafe(r.remote), r.options.Branch)
		case apiclientgen.SourceRefreshStatusNeedsCredential:
			credential.settle(ctx, false)
		default:
			return RefreshResult{}, false, nil
		}
	}
	return RefreshResult{}, false, nil
}

// local refreshes on this machine (§3, local): it fetches the remote into
// this client's cache of it, with this machine's Git configuration — first
// what the pool already has, so the fetch carries only what is new — and
// pushes what moved into the source's namespace as a refresh.
func (r refresher) local(ctx context.Context, unreachable bool) (RefreshResult, error) {
	report := r.options.Report
	cache, err := clientCache(ctx, r.remote)
	if err != nil {
		return RefreshResult{}, err
	}
	if report != nil {
		report(StepFetchingSource)
	}
	if poolURL, err := sandboxgit.CacheRemoteURL(r.serverURL, r.projectID, r.sandboxID, r.source); err == nil {
		_, _ = sandboxgit.Output(ctx, cache, nil, sandboxgit.NoPromptEnv,
			sandboxgit.AuthArgs(r.token, []string{"--git-dir=.", "fetch", "--quiet", "--no-tags", poolURL, poolTrackingRefspec})...)
	}
	branch := r.options.Branch
	heads := "+refs/heads/*:" + remoteTrackingPrefix + "*"
	if branch != "" {
		heads = "+refs/heads/" + branch + ":" + remoteTrackingPrefix + branch
	}
	fetch := []string{"--git-dir=.", "fetch", "--progress", "--no-tags"}
	if branch == "" {
		fetch = append(fetch, "--prune")
	}
	fetch = append(fetch, r.remote, heads, "+refs/tags/*:refs/tags/*")
	if err := gitTransfer(ctx, cache, sandboxgit.NoPromptEnv, StepFetchingSource, report, fetch...); err != nil {
		if branch != "" && strings.Contains(err.Error(), "couldn't find remote ref") {
			return RefreshResult{}, fmt.Errorf("%s has no branch %q", displaySafe(r.remote), branch)
		}
		return RefreshResult{}, fmt.Errorf("fetch %s: %w", displaySafe(r.remote), err)
	}

	originURL, err := sandboxgit.OriginURL(r.serverURL, r.projectID, r.sandboxID, r.source)
	if err != nil {
		return RefreshResult{}, err
	}
	tracking := remoteTrackingPrefix + "*"
	if branch != "" {
		tracking = remoteTrackingPrefix + branch
	}
	if report != nil {
		report(StepPushingSource)
	}
	refreshPush := []string{"--atomic", "-o", sourcecache.RefreshPushOption}
	var updates []sourcecache.RefUpdate
	if branch == "" {
		// The branches the remote dropped: what a pruning push of the
		// remote's branches would delete, asked of the pool's advertisement
		// without writing anything. They go in a push of their own, first: a
		// branch cannot be created beneath the name of one still there.
		planned, err := r.push(ctx, cache, originURL, []string{"--dry-run", "--prune"}, "+"+tracking+":"+tracking)
		if err != nil {
			return RefreshResult{}, err
		}
		var drops []string
		for _, update := range planned {
			if update.New == "" {
				drops = append(drops, ":"+update.Ref)
			}
		}
		if len(drops) > 0 {
			dropped, err := r.push(ctx, cache, originURL, refreshPush, drops...)
			if err != nil {
				return RefreshResult{}, err
			}
			updates = append(updates, dropped...)
		}
	}
	moved, err := r.push(ctx, cache, originURL, refreshPush, "+"+tracking+":"+tracking, "+refs/tags/*:refs/tags/*")
	if err != nil {
		return RefreshResult{}, err
	}
	updates = append(updates, moved...)
	sort.Slice(updates, func(i, j int) bool { return updates[i].Ref < updates[j].Ref })
	return RefreshResult{By: RefreshedThisMachine, Unreachable: unreachable, Updates: updates}, nil
}

// push runs one push from this client's cache of the remote into the
// source's namespace, through sandboxgit's authentication like every other,
// and returns what it moved (or, with --dry-run, would move), read from git's
// porcelain report: a ref's old and new values in full, and an empty New for
// a ref dropped.
func (r refresher) push(ctx context.Context, cache, originURL string, options []string, refspecs ...string) ([]sourcecache.RefUpdate, error) {
	// core.abbrev=no: the report's old value is a full ID, as a pool's
	// refresh reports it, and as the JSON report states it.
	full := append(append([]string{"-c", "core.abbrev=no", "--git-dir=.", "push", "--porcelain"}, options...), originURL)
	full = append(full, refspecs...)
	out, err := sandboxgit.Output(ctx, cache, nil, sandboxgit.NoPromptEnv, sandboxgit.PushArgs(r.token, full)...)
	if err != nil {
		return nil, fmt.Errorf("push the refresh of %s to the discobox's pool: %w", displaySafe(r.remote), err)
	}
	var moved []sourcecache.RefUpdate
	for _, line := range strings.Split(out, "\n") {
		// <flag> TAB <from>:<to> TAB <summary>
		fields := strings.Split(line, "\t")
		if len(fields) < 3 || len(fields[0]) != 1 || fields[0] == "=" || fields[0] == "!" {
			continue
		}
		from, to, ok := strings.Cut(fields[1], ":")
		if !ok {
			continue
		}
		update := sourcecache.RefUpdate{Ref: to}
		if old, _, ok := strings.Cut(fields[2], ".."); ok && !strings.HasPrefix(fields[2], "[") {
			update.Old = old
		}
		if fields[0] != "-" {
			object, err := gitutil.Output(ctx, cache, nil, nil, "--git-dir=.", "rev-parse", "--verify", "--quiet", from)
			if err != nil {
				return nil, fmt.Errorf("read %s in this client's cache of %s: %w", from, displaySafe(r.remote), err)
			}
			update.New = strings.TrimSpace(object)
		}
		moved = append(moved, update)
	}
	return moved, nil
}

// refUpdates is the API's account of what a pool refresh moved.
func refUpdates(answer *apiclientgen.SourceRefresh) []sourcecache.RefUpdate {
	var out []sourcecache.RefUpdate
	for _, update := range answer.Updates {
		out = append(out, sourcecache.RefUpdate{Ref: update.Ref, Old: update.Old.Or(""), New: update.New.Or("")})
	}
	return out
}
