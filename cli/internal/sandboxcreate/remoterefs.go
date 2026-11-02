package sandboxcreate

import (
	"context"
	"errors"
	"maps"
	"net/url"
	"strings"
	"sync"

	"github.com/discobox-ai/discobox/cli/internal/sandboxgit"
	"github.com/discobox-ai/discobox/pkg/sourceref"
	"github.com/discobox-ai/x/gitutil"
)

// remoteRefs pins the remote Git refs of one create, starting each lookup as
// soon as the create knows it will need it rather than when its turn comes.
//
// Resolving a create's sources is sequential on purpose: the order decides
// which source wins a name or a directory, and resolving a local one can ask
// the person something. But each remote source's turn starts with an
// `ls-remote` — a network round trip, the slow part — that depends on nothing
// before it. So the round trips are started together (prefetch) and the
// resolution walks its sources in order as before, taking each answer as it
// reaches it (resolve). A create waits about as long as its slowest lookup
// rather than the sum of them.
//
// A lookup started ahead of time must not ask anybody anything: two of them
// asking on one terminal at once would be unanswerable. So only a transport
// that asks through git itself is started early, with git's asking turned off
// (prefetchEnv). ssh asks on the terminal on its own — a passphrase, a new
// host key — and waits for the answer rather than failing, so a remote git
// would reach over ssh, or over any transport this does not know, is looked
// up in its turn as before (prefetchable). Which transport that is is git's
// answer, not the URL's: a `url.<base>.insteadOf` can turn the https URL a
// source names into an ssh one, which is how many people reach GitHub.
//
// One that failed only because it could not ask is looked up again in its
// turn, prompt and all, so a remote that needs a credential typed in still
// gets it asked for — once, and alone (lookInTurn). Any other failure is the
// answer: looking again would wait out a dead host's timeout twice.
type remoteRefs struct {
	ctx     context.Context
	mu      sync.Mutex
	lookups map[remoteRefKey]*remoteRefLookup
}

type remoteRefKey struct {
	url      string
	ref      string
	explicit bool
}

type remoteRefLookup struct {
	done chan struct{}
	ref  remoteRef
	err  error
}

// remoteRef is what one ref of a remote resolved to: the commit, and the
// branch or tag it was named by (resolveRemoteGitRef).
type remoteRef struct {
	commit  string
	name    string
	refType string
}

// newRemoteRefs starts no lookups yet. Every one it starts ends with ctx.
func newRemoteRefs(ctx context.Context) *remoteRefs {
	return &remoteRefs{ctx: ctx, lookups: map[remoteRefKey]*remoteRefLookup{}}
}

// prefetch starts the lookup a source argument will need, if it names a
// remote repository, and is a no-op for anything else: a local source has no
// round trip to start, and an argument that does not parse is refused where it
// is resolved, not here.
func (r *remoteRefs) prefetch(arg string) {
	if r == nil {
		return
	}
	source, err := ParseSource(arg)
	if err != nil || source.Kind != sourceref.Git || !prefetchable(source.Location) {
		return
	}
	key := remoteLookupKey(source.Location, source.Ref, source.Ref != "")
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.lookups[key]; ok {
		return
	}
	lookup := &remoteRefLookup{done: make(chan struct{})}
	r.lookups[key] = lookup
	go func() {
		defer close(lookup.done)
		if !prefetchable(rewrittenURL(r.ctx, key.url)) {
			lookup.err = errAsksItself
			return
		}
		lookup.ref, lookup.err = resolveRemoteGitRef(r.ctx, key.url, key.ref, key.explicit, prefetchEnv)
	}()
}

// resolve is the ref a remote source is pinned to: the prefetched answer, or
// its error, unless the lookup is one to make in its turn (lookInTurn), and a
// lookup made now, prompt and all, when there was none. A nil remoteRefs only
// ever looks up now.
func (r *remoteRefs) resolve(ctx context.Context, url, ref string, explicit bool) (remoteRef, error) {
	key := remoteLookupKey(url, ref, explicit)
	if r != nil {
		r.mu.Lock()
		lookup := r.lookups[key]
		r.mu.Unlock()
		if lookup != nil {
			select {
			case <-lookup.done:
				if !lookInTurn(lookup.err) {
					return lookup.ref, lookup.err
				}
			case <-ctx.Done():
				return remoteRef{}, ctx.Err()
			}
		}
	}
	return resolveRemoteGitRef(ctx, key.url, key.ref, key.explicit, nil)
}

// remoteLookupKey is the lookup a remote source makes: a source that names no
// ref follows the remote's default branch.
func remoteLookupKey(url, ref string, explicit bool) remoteRefKey {
	if !explicit {
		ref = defaultRemoteBranch
	}
	return remoteRefKey{url: url, ref: ref, explicit: explicit}
}

// prefetchEnv is git that does not ask (sandboxgit.NoPromptEnv), and does not
// run an askpass program either: git runs one before it checks its prompt
// setting, and an IDE's terminal points it at a dialog, which several lookups
// would open at once. Set empty, it skips GIT_ASKPASS, core.askPass and
// SSH_ASKPASS alike. That is safe here only because a lookup that failed for
// it is made again in its turn, askpass and all (lookInTurn) — an askpass is
// often how the credential arrives, not a question. Its messages are in
// English so that a failure for want of asking can be told from any other.
var prefetchEnv = func() map[string]string {
	env := maps.Clone(sandboxgit.NoPromptEnv)
	env["GIT_ASKPASS"] = ""
	env["LC_ALL"] = "C"
	return env
}()

// errAsksItself is a remote git would reach over a transport that asks on its
// own, which is not looked up ahead of its turn.
var errAsksItself = errors.New("reached over a transport that asks for itself")

// rewrittenURL is the URL git reaches a remote at, its `insteadOf` rewrites
// applied, or empty when git cannot say; it asks nothing of the network.
func rewrittenURL(ctx context.Context, remote string) string {
	out, err := gitutil.Output(ctx, "", nil, nil, "ls-remote", "--get-url", "--", remote)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// prefetchable reports whether git reaches a remote, as it spells the URL
// once its rewrites are applied (rewrittenURL), over a transport that asks
// for nothing except through git: http(s) and git://, and a local file://
// repository. ssh, scp-like addresses included, asks on the terminal itself.
func prefetchable(location string) bool {
	u, err := url.Parse(location)
	if err != nil || !strings.Contains(location, "://") {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "http", "git", "file":
		return true
	}
	return false
}

// lookInTurn reports whether a prefetched lookup is to be made again in its
// turn, where somebody can answer: one never started because its transport
// asks for itself, and one that failed because prefetchEnv kept git from
// asking for a credential.
func lookInTurn(err error) bool {
	return errors.Is(err, errAsksItself) ||
		(err != nil && strings.Contains(err.Error(), "terminal prompts disabled"))
}
