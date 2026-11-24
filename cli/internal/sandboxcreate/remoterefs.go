package sandboxcreate

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os/exec"
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
// turn, so a remote that needs a credential typed in still gets it asked for —
// once, and alone (lookInTurn). Any other failure is the answer: looking again
// would wait out a dead host's timeout twice.
//
// Asking in its turn still needs the terminal, and a create's window or status
// line owns it. So git asks there only through runGit, the frontend's handoff
// of the terminal (the one lending's `credential fill` takes); with none, git's
// own prompt is off (ssh's questions are its own: see lookNow), and a remote
// that wants a credential fails naming the way to give git one.
type remoteRefs struct {
	ctx     context.Context
	runGit  func(context.Context, *exec.Cmd) error
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
// branch or tag it was named by (resolveRemoteGitRef); and the branch the
// remote's HEAD named, when it said.
type remoteRef struct {
	commit        string
	name          string
	refType       string
	defaultBranch string
}

// newRemoteRefs starts no lookups yet. Every one it starts ends with ctx.
// runGit, when not nil, runs a lookup made in its turn with the terminal handed
// to it (PromptOptions.RunGit).
func newRemoteRefs(ctx context.Context, runGit func(context.Context, *exec.Cmd) error) *remoteRefs {
	return &remoteRefs{ctx: ctx, runGit: runGit, lookups: map[remoteRefKey]*remoteRefLookup{}}
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
		lookup.ref, lookup.err = resolveRemoteGitRef(r.ctx, key.url, key.ref, key.explicit, prefetchEnv, nil)
	}()
}

// resolve is the ref a remote source is pinned to: the prefetched answer, or
// its error, unless the lookup is one to make in its turn (lookInTurn), and a
// lookup made now (lookNow) when there was none. A nil remoteRefs only ever
// looks up now, with nobody to hand the terminal to.
func (r *remoteRefs) resolve(ctx context.Context, url, ref string, explicit bool) (remoteRef, error) {
	key := remoteLookupKey(url, ref, explicit)
	var runGit func(context.Context, *exec.Cmd) error
	var prefetched error
	if r != nil {
		runGit = r.runGit
		r.mu.Lock()
		lookup := r.lookups[key]
		r.mu.Unlock()
		if lookup != nil {
			select {
			case <-lookup.done:
				if !lookInTurn(lookup.err) {
					return lookup.ref, lookup.err
				}
				prefetched = lookup.err
			case <-ctx.Done():
				return remoteRef{}, ctx.Err()
			}
		}
	}
	return lookNow(ctx, key, prefetched, runGit)
}

// lookNow makes a lookup in its turn. With nobody to hand the terminal to
// (runGit nil, or an operation that asks nothing), git's own prompt is off:
// what a helper or askpass gives without asking is all it gets
// (sandboxgit.NoPromptEnv). ssh's questions are its own, on /dev/tty, and are
// not turned off here: a frontend that owns the terminal always has a
// handoff, and an operation that asks nothing has ssh in batch mode
// (sandboxgit.Environment). With a handoff it first looks without asking
// anything at all — ssh in batch mode, which an agent's key answers, so the
// screen is not handed away for nothing — unless the prefetch already did that
// and failed for want of asking; and only for want of asking does git get the
// terminal, its environment as the person left it. Either way, a remote that
// still wants a credential fails saying how to give git one (needsCredential).
//
// The lookups made without the terminal read their failure in English
// (quietEnv), so that a failure for want of asking can be told from any other.
// The handed one keeps the person's language, since git asks them in it; it
// only ever follows a failure for want of asking, so its own failure is taken
// for one.
func lookNow(ctx context.Context, key remoteRefKey, prefetched error, runGit func(context.Context, *exec.Cmd) error) (remoteRef, error) {
	if runGit == nil || sandboxgit.Noninteractive(ctx) {
		pinned, err := resolveRemoteGitRef(ctx, key.url, key.ref, key.explicit, quietEnv(sandboxgit.NoPromptEnv), nil)
		if wantedAsking(err) {
			err = needsCredential(key.url, err)
		}
		return pinned, err
	}
	if prefetched == nil || errors.Is(prefetched, errAsksItself) {
		pinned, err := resolveRemoteGitRef(sandboxgit.WithNoninteractive(ctx), key.url, key.ref, key.explicit, quietEnv(nil), nil)
		if !wantedAsking(err) {
			return pinned, err
		}
	}
	pinned, err := resolveRemoteGitRef(ctx, key.url, key.ref, key.explicit, nil, runGit)
	if err != nil && ctx.Err() == nil {
		err = needsCredential(key.url, err)
	}
	return pinned, err
}

// quietEnv is env with git's and ssh's messages in English (wantedAsking).
func quietEnv(env map[string]string) map[string]string {
	env = maps.Clone(env)
	if env == nil {
		env = map[string]string{}
	}
	env["LC_ALL"] = "C"
	return env
}

// wantedAsking reports whether a lookup failed for want of asking somebody:
// git's prompt turned off, or ssh in batch mode refused for want of a key's
// passphrase or a host key's confirmation. The messages are git's and ssh's in
// English, which prefetchEnv and quietEnv ask for.
func wantedAsking(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	for _, want := range []string{"terminal prompts disabled", "could not read Username", "could not read Password", "Authentication failed", "Permission denied (", "Host key verification failed"} {
		if strings.Contains(message, want) {
			return true
		}
	}
	return false
}

// needsCredential is err, a failure for want of a credential, saying which
// remote wants one and how to give git one. Before a discobox exists there is
// no Git credentials choice to offer for it: git on this machine is what pins
// the commit.
func needsCredential(remote string, err error) error {
	name := networkRemoteURL(remote)
	if name == "" {
		name = remote
	}
	return fmt.Errorf("%s needs a Git credential this machine did not give: set up a Git credential helper (for GitHub, `gh auth setup-git`) or an SSH key for it, then try again: %w", name, err)
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
// asks for itself, and one that failed for want of asking (wantedAsking),
// which prefetchEnv keeps git from doing.
func lookInTurn(err error) bool {
	return errors.Is(err, errAsksItself) || wantedAsking(err)
}
