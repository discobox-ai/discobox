package sandboxcreate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxgit"
	"github.com/discobox-ai/discobox/pkg/sourceref"
	"github.com/discobox-ai/x/gitutil"
)

// A source is delivered into its pool's cache of its remote (ADR
// 26-10-10-545 §3): the client names every ref of the delivery and the object
// each points at, the pool creates those its cache already holds — a pin,
// which transfers nothing — and the client pushes the rest. Because every
// sandbox of a remote shares one object store on a pool, the push carries
// only what that pool has never been sent.
//
// A delivery does not wait for the sandbox. Its route names the sandbox and
// the source, the control plane names the cache, and the pool needs nothing
// else — so it runs while the image is pulled and the sandbox boots, and a
// pool that is not up yet is the only thing it waits for.

const (
	// remoteTrackingPrefix is where a delivery puts the remote's branches in
	// a source's namespace, whatever the client's own name for the remote is.
	// The sandbox's checkout takes them as its origin's (ADR 26-10-10-545 §6).
	remoteTrackingPrefix = "refs/remotes/origin/"
	// poolTrackingRefspec is where a client keeps what its pool's cache
	// remembers of a remote, in its own cache of that remote. They are there
	// to be negotiated against, and nothing reads them by name.
	poolTrackingRefspec = "+" + "refs/cache/*:refs/discobox/pool/*"

	// pinStall bounds the wait for a pool that is not up yet by silence, as
	// awaitSourceStall bounds the wait for a sandbox to park.
	pinStall = awaitSourceStall
	// clientFetchAttempts is how many times a fetch into the client's cache of
	// a remote is tried. Two creates from one remote fetch into it at once,
	// and the one that loses a ref's lock has only to go again.
	clientFetchAttempts = 3
)

// pinPollInterval paces the wait for a pool that is not up yet.
var pinPollInterval = time.Second

// clientGitCacheRoot is where this client keeps a bare repository per remote
// it has delivered a remote-URL source from. It is a cache in the plain
// sense: deleting it costs the next delivery a full fetch and nothing else.
var clientGitCacheRoot = func() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("find this user's cache directory: %w", err)
	}
	return filepath.Join(dir, "discobox", "client", "git"), nil
}

// cacheDelivery is where a delivery goes and as whom.
type cacheDelivery struct {
	client       sourceDeliveryClient
	serverURL    string
	projectID    string
	sandboxID    string
	token        string
	report       Report
	options      DeliveryOptions
	credentialUI *sync.Mutex
}

// deliveredToCache reports whether a source is delivered into its pool's
// cache, rather than pushed into a repository made for its sandbox.
func deliveredToCache(source apimodel.GitSource) bool {
	delivery, ok := source.Delivery.Get()
	return ok && delivery == apiclientgen.GitSourceDeliveryCache
}

// remoteURLSource is the URL of a source that names a remote and no directory
// on any client's machine, or empty.
func remoteURLSource(source apimodel.GitSource) string {
	if strings.TrimSpace(source.LocalDirectory.Or("")) != "" {
		return ""
	}
	if u, ok := source.URL.Get(); ok {
		return u.String()
	}
	return ""
}

// deliver delivers one cache-delivered source: a pin, and a push of whatever
// the pin could not create.
func (d cacheDelivery) deliver(ctx context.Context, entry pushDeliveredSource, local *LocalSources) error {
	commit, branch, snapshotRef, err := pushRefs(entry.source)
	if err != nil {
		return err
	}
	slug := strings.TrimSpace(entry.source.Slug.Or(""))
	if slug == "" {
		return fmt.Errorf("discobox source has no slug to address its repository")
	}
	originURL, err := sandboxgit.OriginURL(d.serverURL, d.projectID, d.sandboxID, entry.source)
	if err != nil {
		return err
	}
	if remote := remoteURLSource(entry.source); remote != "" {
		return d.deliverRemote(ctx, entry.source, slug, remote, originURL, commit, branch)
	}
	repoRoot, err := local.pushRoot(entry.key)
	if err != nil {
		return err
	}
	return d.deliverLocal(ctx, entry.source, slug, repoRoot, originURL, commit, branch, snapshotRef)
}

// deliverLocal delivers a source out of a repository on this machine, which
// already holds every object: the source's branch at its pinned commit, the
// snapshot of a dirty workspace, and the remote's branches and tags as this
// checkout knows them. The last is what lets the sandbox's checkout read as a
// clone of the remote without reaching it — its origin/<branch> is where the
// remote was, not where the developer's unpushed commits are.
func (d cacheDelivery) deliverLocal(ctx context.Context, source apimodel.GitSource, slug, repoRoot, originURL, commit, branch, snapshotRef string) error {
	leaseRef := sandboxgit.OriginLeaseRef(d.sandboxID, slug, pushBranch(branch))
	// The branch is left alone when the origin already reaches the commit, by
	// this client's own record of what it put there; see pushSource.
	held := originHolds(ctx, repoRoot, leaseRef, commit)
	refs := map[string]string{}
	var refspecs []string
	if !held {
		refs["refs/heads/"+pushBranch(branch)] = commit
		refspecs = append(refspecs, commit+":refs/heads/"+pushBranch(branch))
	}
	if snapshotRef != "" {
		snapshot, err := gitutil.Output(ctx, repoRoot, nil, nil, "rev-parse", "--verify", "--quiet", snapshotRef)
		if err != nil {
			return fmt.Errorf("source %q carried uncommitted changes, and the snapshot holding them (%s) is gone from %s", slug, snapshotRef, repoRoot)
		}
		refs[snapshotRef] = strings.TrimSpace(snapshot)
		refspecs = append(refspecs, "+"+snapshotRef+":"+snapshotRef)
	}
	if remote := remoteNamed(ctx, repoRoot, source.UpstreamUrl.Or("")); remote != "" {
		tracking, err := refsUnder(ctx, repoRoot, "refs/remotes/"+remote+"/")
		if err != nil {
			return err
		}
		// Not the remote's HEAD: a push would land it as a plain ref frozen
		// at one commit, where a clone has a symbolic one.
		delete(tracking, "HEAD")
		for name, object := range tracking {
			refs[remoteTrackingPrefix+name] = object
		}
		if len(tracking) > 0 {
			refspecs = append(refspecs, "+refs/remotes/"+remote+"/*:"+remoteTrackingPrefix+"*", "^refs/remotes/"+remote+"/HEAD")
		}
	}
	tags, err := refsUnder(ctx, repoRoot, "refs/tags/")
	if err != nil {
		return err
	}
	for name, object := range tags {
		refs["refs/tags/"+name] = object
	}
	if len(tags) > 0 {
		refspecs = append(refspecs, "+refs/tags/*:refs/tags/*")
	}

	missing, err := d.pin(ctx, source, refs)
	if err != nil {
		return err
	}
	if len(missing) > 0 {
		if err := d.push(ctx, repoRoot, originURL, refspecs); err != nil {
			return err
		}
	}
	if !held {
		// The commit just delivered is the lease every later `discobox push`
		// of this source leases against (ADR 0058 §6). A failure to record it
		// must not fail the delivery: the source is delivered either way, and
		// the only cost is that the next push has no lease to hold.
		_ = gitutil.UpdateRef(ctx, repoRoot, leaseRef, commit)
	}
	return nil
}

// deliverRemote delivers a source that is a remote URL. The client lists the
// remote, which gives every ref of the delivery without fetching anything;
// when the pool holds them all, nothing more happens. Otherwise it fetches
// into its own cache of the remote — first what the pool has, then the remote
// — and pushes: the second fetch is negotiated against the first, so each leg
// carries only what the next host lacks.
//
// The first create for a remote that neither this client nor the pool has
// seen moves the history twice, remote to client to pool. That is the price
// of a pool that never fetches and never holds a credential, and it is paid
// once per remote per pool.
func (d cacheDelivery) deliverRemote(ctx context.Context, source apimodel.GitSource, slug, remote, originURL, commit, branch string) error {
	cachePath, err := clientCachePath(remote)
	if err != nil {
		return err
	}
	tipFromCache, heldErr := gitutil.Output(ctx, cachePath, nil, nil, "rev-parse", "--verify", "--quiet", commit+"^{commit}")
	held := heldErr == nil && tipFromCache != ""
	var listed map[string]string
	var peeled map[string]string
	if held {
		listed = map[string]string{}
		peeled = map[string]string{commit: strings.TrimSpace(tipFromCache)}
		for _, prefix := range []string{remoteTrackingPrefix, "refs/tags/"} {
			refs, err := refsUnder(ctx, cachePath, prefix)
			if err != nil {
				return err
			}
			for name, object := range refs {
				listed[prefix+name] = object
			}
		}
	} else {
		if done, err := d.waitForPull(ctx, slug, remote, commit); err != nil || done {
			return err
		}
		listed, peeled, err = listRemote(ctx, remote)
		if err != nil {
			return fmt.Errorf("list the refs of source %q at %s: %w", slug, remote, err)
		}
	}
	// A source pinned at an annotated tag names the tag; a branch holds the
	// commit it peels to.
	tip := commit
	if commitOf, ok := peeled[commit]; ok {
		tip = commitOf
	}
	refs := map[string]string{"refs/heads/" + pushBranch(branch): tip}
	for ref, object := range listed {
		refs[ref] = object
	}
	// A source asked to start in a directory of its commit is refused here
	// when the commit has none, before the pool is given anything: the
	// harness would otherwise fail to start in it (ADR 26-10-15-664 §3).
	fetched, err := d.checkRemoteWorkingPath(ctx, source, slug, remote, commit)
	if err != nil {
		return err
	}
	missing, err := d.pin(ctx, source, refs)
	if err != nil || len(missing) == 0 {
		return err
	}

	cache, err := clientCache(ctx, remote)
	if err != nil {
		return err
	}
	if !fetched && !held {
		d.report.step(StepFetchingSource)
		// What the pool already has, when it will say: a project member's to
		// fetch and not a sandbox's, and worth nothing but bytes saved, so a
		// refusal is not a failure.
		if poolURL, err := sandboxgit.CacheRemoteURL(d.serverURL, d.projectID, d.sandboxID, source); err == nil {
			_, _ = gitutil.Output(ctx, cache, nil, sandboxgit.NoPromptEnv,
				sandboxgit.AuthArgs(d.token, []string{"fetch", "--quiet", "--no-tags", poolURL, poolTrackingRefspec})...)
		}
		if err := fetchRemote(ctx, cache, remote, commit, d.report); err != nil {
			return fmt.Errorf("fetch source %q from %s: %w", slug, remote, err)
		}
	}
	d.report.step(StepPushingSource)
	return d.push(ctx, cache, originURL, []string{
		commit + "^{commit}:refs/heads/" + pushBranch(branch),
		"+" + remoteTrackingPrefix + "*:" + remoteTrackingPrefix + "*",
		"+refs/tags/*:refs/tags/*",
	})
}

// checkRemoteWorkingPath refuses a remote source whose commit has no
// directory at the //PATH it was asked to start in (remoteWorkingPath). The
// commit is read from this client's cache of the remote, fetched into it
// first when the cache does not hold it — even when the pool does, since the
// pool is never asked what a commit contains. fetched reports that fetch, so
// the delivery does not make it twice.
func (d cacheDelivery) checkRemoteWorkingPath(ctx context.Context, source apimodel.GitSource, slug, remote, commit string) (fetched bool, err error) {
	dir := remoteWorkingPath(source)
	if dir == "" {
		return false, nil
	}
	cache, err := clientCache(ctx, remote)
	if err != nil {
		return false, err
	}
	if _, err := gitutil.Output(ctx, cache, nil, nil, "cat-file", "-e", commit+"^{commit}"); err != nil {
		d.report.step(StepFetchingSource)
		if err := fetchRemote(ctx, cache, remote, commit, d.report); err != nil {
			return false, fmt.Errorf("fetch source %q from %s: %w", slug, remote, err)
		}
		fetched = true
	}
	// A directory, or a link the checkout follows as a local DIR//PATH's
	// would be: a link's target is the sandbox's to resolve.
	entry, err := gitutil.Output(ctx, cache, nil, nil, "ls-tree", commit+"^{commit}", "--", dir)
	if mode, _, _ := strings.Cut(strings.TrimSpace(entry), " "); err != nil || mode != "040000" && mode != "120000" {
		return fetched, fmt.Errorf("source %q: %s at %s has no directory %s for the harness to start in", slug, remote, commit[:min(len(commit), 12)], dir)
	}
	return fetched, nil
}

// remoteWorkingPath is the //PATH a remote source asked the harness to start
// in (ADR 26-10-15-664 §3): its working directory beneath the directory it is
// placed in, slash-separated, or empty for its root. It is read off what the
// server recorded, so whichever client delivers the source checks it.
func remoteWorkingPath(source apimodel.GitSource) string {
	destination, ok := source.Destination.Get()
	if !ok {
		return ""
	}
	dir, working := destination.Directory.Or(""), destination.WorkingDirectory.Or("")
	rest, ok := strings.CutPrefix(working, dir)
	if dir == "" || !ok || len(rest) < 2 || (rest[0] != '/' && rest[0] != '\\') {
		return ""
	}
	return strings.ReplaceAll(rest[1:], `\`, "/")
}

// push sends refspecs into the source's namespace in one transaction: a
// sandbox clones as soon as its namespace has a branch, so a branch never
// lands ahead of the snapshot or the remote's refs that belong with it.
//
// Another client can be delivering this discobox at the same time (ADR
// 26-09-24-005 §3). Both send the same pinned commit, so a push that loses
// the ref update is read against the discobox and made once more: by then
// either the other delivery has been reported and there is nothing left to
// do, or the branch holds this commit and the push is a no-op.
func (d cacheDelivery) push(ctx context.Context, repoRoot, originURL string, refspecs []string) error {
	args := append([]string{"push", "--atomic", "--progress", originURL}, refspecs...)
	attempt := func() error {
		return gitTransfer(ctx, repoRoot, sandboxgit.NoPromptEnv, StepPushingSource, d.report, sandboxgit.PushArgs(d.token, args)...)
	}
	err := attempt()
	if err == nil {
		return nil
	}
	if delivered, readErr := sourceDelivered(ctx, d.client, d.projectID, d.sandboxID); readErr == nil && delivered {
		return nil
	}
	if err := attempt(); err != nil {
		return fmt.Errorf("push source to discobox: %w", err)
	}
	return nil
}

// pinResult is the pool's answer to a pin.
type pinResult struct {
	Pinned  []string `json:"pinned"`
	Missing []string `json:"missing"`
}

// pin asks the pool to create refs in the source's namespace from what its
// cache already holds, and returns the refs it could not. It waits out a pool
// that is not up yet, which is the one thing a delivery waits for.
func (d cacheDelivery) pin(ctx context.Context, source apimodel.GitSource, refs map[string]string) ([]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	pinURL, err := sandboxgit.PinURL(d.serverURL, d.projectID, d.sandboxID, source)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(map[string]any{"refs": refs})
	if err != nil {
		return nil, err
	}
	stall := NewStallClock(pinStall)
	for {
		result, wait, err := d.pinOnce(ctx, pinURL, body)
		if err == nil {
			sort.Strings(result.Missing)
			return result.Missing, nil
		}
		if !wait {
			return nil, err
		}
		if stall.Expired() {
			return nil, fmt.Errorf("gave up after %s waiting for the discobox's pool to take its source: %w", pinStall, err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pinPollInterval):
		}
	}
}

// pinOnce makes one pin request. wait reports a refusal that only says the
// pool is not answering yet.
func (d cacheDelivery) pinOnce(ctx context.Context, pinURL string, body []byte) (result pinResult, wait bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pinURL, bytes.NewReader(body))
	if err != nil {
		return pinResult{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := strings.TrimSpace(d.token); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return pinResult{}, false, ctx.Err()
		}
		return pinResult{}, true, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return pinResult{}, true, err
	}
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(data, &result); err != nil {
			return pinResult{}, false, fmt.Errorf("read the pool's answer to a pin: %w", err)
		}
		return result, false, nil
	}
	message := strings.TrimSpace(string(data))
	err = fmt.Errorf("the discobox's pool refused the source (%s): %s", resp.Status, message)
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return pinResult{}, true, err
	case http.StatusConflict:
		// A pool, or its agent, that is still coming up; any other conflict
		// is an answer.
		return pinResult{}, strings.Contains(message, "is not reachable"), err
	}
	return pinResult{}, false, err
}

// remoteNamed is the name this repository configures the remote at remoteURL
// under, or empty when it configures none there. The URL is the one the
// create reported (localRemote), so the comparison is of the same spelling.
func remoteNamed(ctx context.Context, repoRoot, remoteURL string) string {
	remoteURL = strings.TrimSpace(remoteURL)
	if remoteURL == "" {
		return ""
	}
	out, err := gitutil.Output(ctx, repoRoot, nil, nil, "remote")
	if err != nil {
		return ""
	}
	want := sourceref.NormalizeRemote(remoteURL)
	fallback := ""
	for _, name := range strings.Fields(out) {
		value := remoteFetchURL(ctx, repoRoot, name)
		if value == remoteURL {
			return name
		}
		if fallback == "" && want != "" && sourceref.NormalizeRemote(value) == want {
			fallback = name
		}
	}
	return fallback
}

// refsUnder is every ref of repoRoot under prefix and the object it names,
// by its name past the prefix. A symbolic ref — a remote's HEAD — is listed
// at the object it resolves to.
func refsUnder(ctx context.Context, repoRoot, prefix string) (map[string]string, error) {
	out, err := gitutil.Output(ctx, repoRoot, nil, nil, "for-each-ref", "--format=%(objectname) %(refname)", prefix)
	if err != nil {
		return nil, fmt.Errorf("list %s in %s: %w", prefix, repoRoot, err)
	}
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		object, ref, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if name := strings.TrimPrefix(ref, prefix); name != "" && name != ref {
			refs[name] = object
		}
	}
	return refs, nil
}

// listRemote lists a remote's branches and tags as the refs a delivery puts
// in a source's namespace — branches under remoteTrackingPrefix, tags under
// their own names — and, for each annotated tag, the commit it peels to.
func listRemote(ctx context.Context, remote string) (refs, peeled map[string]string, err error) {
	out, err := gitutil.Output(ctx, "", nil, sandboxgit.NoPromptEnv, "ls-remote", "--", remote)
	if err != nil {
		return nil, nil, err
	}
	refs, peeled = map[string]string{}, map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		object, ref := fields[0], fields[1]
		switch {
		case strings.HasSuffix(ref, "^{}"):
			if tag, ok := refs[strings.TrimSuffix(ref, "^{}")]; ok {
				peeled[tag] = object
			}
		case strings.HasPrefix(ref, "refs/heads/"):
			refs[remoteTrackingPrefix+strings.TrimPrefix(ref, "refs/heads/")] = object
		case strings.HasPrefix(ref, "refs/tags/"):
			refs[ref] = object
		}
	}
	return refs, peeled, nil
}

// clientCache is this client's bare repository for remote, made if it is not
// there.
func clientCachePath(remote string) (string, error) {
	root, err := clientGitCacheRoot()
	if err != nil {
		return "", err
	}
	name := sourceref.NormalizeRemote(remote)
	if name == "" {
		name = strings.TrimSpace(remote)
	}
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(root, hex.EncodeToString(sum[:16])+".git"), nil
}

func clientCache(ctx context.Context, remote string) (string, error) {
	path, err := clientCachePath(remote)
	if err != nil {
		return "", err
	}
	root := filepath.Dir(path)
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	if _, err := gitutil.Output(ctx, root, nil, nil, "init", "--quiet", "--bare", path); err != nil {
		return "", fmt.Errorf("make this client's cache of %s: %w", remote, err)
	}
	return path, nil
}

// fetchRemote brings the client's cache of remote up to date with it, with
// the user's own Git configuration and credentials, and makes sure it holds
// commit. Git never asks: this runs under a status line, where a prompt is a
// hang with nothing on screen to explain it.
func fetchRemote(ctx context.Context, cache, remote, commit string, report Report) error {
	fetch := []string{"fetch", "--progress", "--prune", "--no-tags", remote,
		"+refs/heads/*:" + remoteTrackingPrefix + "*", "+refs/tags/*:refs/tags/*"}
	var err error
	for attempt := 0; attempt < clientFetchAttempts; attempt++ {
		if err = gitTransfer(ctx, cache, sandboxgit.NoPromptEnv, StepFetchingSource, report, fetch...); err == nil || ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return err
	}
	if _, err := gitutil.Output(ctx, cache, nil, nil, "cat-file", "-e", commit+"^{commit}"); err == nil {
		return nil
	}
	// Pinned at a commit no branch or tag reaches now: the remote is asked
	// for it by name.
	if _, err := gitutil.Output(ctx, cache, nil, sandboxgit.NoPromptEnv, "fetch", "--quiet", "--no-tags", remote, commit); err != nil {
		return fmt.Errorf("the remote no longer has commit %s: %w", commit, err)
	}
	return nil
}
