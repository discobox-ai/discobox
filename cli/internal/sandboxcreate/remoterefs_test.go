package sandboxcreate

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Every remote a create names is looked up ahead of its turn, and each answer
// is the one a lookup made in its turn would have given.
func TestRemoteRefsPrefetchedAnswersMatchADirectLookup(t *testing.T) {
	ctx := context.Background()
	var urls []string
	for range 3 {
		urls = append(urls, "file://"+filepath.ToSlash(newRunSourceTestRepo(t)))
	}
	refs := newRemoteRefs(ctx, nil)
	for _, url := range urls {
		refs.prefetch(url)
		refs.prefetch(url + "@feature-foo")
	}
	if got := len(refs.lookups); got != 2*len(urls) {
		t.Fatalf("lookups started = %d, want %d", got, 2*len(urls))
	}
	for _, url := range urls {
		for _, explicit := range []bool{false, true} {
			ref := ""
			if explicit {
				ref = "feature-foo"
			}
			got, err := refs.resolve(ctx, url, ref, explicit)
			if err != nil {
				t.Fatalf("resolve %s@%s: %v", url, ref, err)
			}
			want, err := resolveRemoteGitRef(ctx, url, remoteLookupKey(url, ref, explicit).ref, explicit, nil, nil)
			if err != nil {
				t.Fatalf("direct lookup %s@%s: %v", url, ref, err)
			}
			if got != want {
				t.Fatalf("resolve %s@%s = %+v, want %+v", url, ref, got, want)
			}
		}
	}
}

// A prefetched answer is taken as it is: the lookup is not made twice.
func TestRemoteRefsResolveTakesTheFinishedLookup(t *testing.T) {
	refs := newRemoteRefs(context.Background(), nil)
	url := "file:///nowhere/at/all"
	want := remoteRef{commit: strings.Repeat("a", 40), name: "main", refType: runSourceRefTypeBranch}
	lookup := &remoteRefLookup{done: make(chan struct{}), ref: want}
	close(lookup.done)
	refs.lookups[remoteLookupKey(url, "", false)] = lookup

	got, err := refs.resolve(context.Background(), url, "", false)
	if err != nil || got != want {
		t.Fatalf("resolve = %+v, %v; want the prefetched %+v", got, err, want)
	}
}

// A prefetch that failed for want of asking — it ran with git's prompt off, so
// a remote that needs a credential typed in fails it — is looked up again in
// its turn.
func TestRemoteRefsLookAgainWhenThePrefetchCouldNotAsk(t *testing.T) {
	ctx := context.Background()
	url := "file://" + filepath.ToSlash(newRunSourceTestRepo(t))
	refs := newRemoteRefs(ctx, nil)
	lookup := &remoteRefLookup{done: make(chan struct{}), err: errors.New("fatal: could not read Username for 'https://example.com': terminal prompts disabled")}
	close(lookup.done)
	refs.lookups[remoteLookupKey(url, "", false)] = lookup

	got, err := refs.resolve(ctx, url, "", false)
	if err != nil {
		t.Fatalf("resolve after a failed prefetch: %v", err)
	}
	if len(got.commit) != 40 {
		t.Fatalf("resolve = %+v, want a pinned commit", got)
	}
}

// Any other failure is the answer: looking again would wait out a dead host's
// timeout a second time.
func TestRemoteRefsTakeAFailedPrefetchAsTheAnswer(t *testing.T) {
	ctx := context.Background()
	url := "file://" + filepath.ToSlash(newRunSourceTestRepo(t))
	refs := newRemoteRefs(ctx, nil)
	failed := errors.New("fatal: unable to access 'https://example.com/': Connection timed out")
	lookup := &remoteRefLookup{done: make(chan struct{}), err: failed}
	close(lookup.done)
	refs.lookups[remoteLookupKey(url, "", false)] = lookup

	if _, err := refs.resolve(ctx, url, "", false); !errors.Is(err, failed) {
		t.Fatalf("resolve after a failed prefetch = %v, want its error and no second lookup", err)
	}
}

// Nothing is looked up ahead of time but a remote repository over a transport
// that asks only through git: not a local directory, and not ssh, which asks
// on the terminal itself. A nil remoteRefs, as a resolution with nothing
// prefetched has, still resolves.
func TestRemoteRefsPrefetchOnlyRemotesThatCannotAsk(t *testing.T) {
	refs := newRemoteRefs(context.Background(), nil)
	refs.prefetch(newRunSourceTestRepo(t))
	refs.prefetch("")
	refs.prefetch("ssh://git@example.invalid/o/r")
	refs.prefetch("git@example.invalid:o/r")
	if len(refs.lookups) != 0 {
		t.Fatalf("lookups = %v, want none for a local directory, nothing, or ssh", refs.lookups)
	}
	for location, want := range map[string]bool{
		"https://github.com/o/r":    true,
		"http://example.com/r.git":  true,
		"git://example.com/r":       true,
		"file:///srv/r":             true,
		"ssh://git@github.com/o/r":  false,
		"git+ssh://github.com/o/r":  false,
		"git@github.com:o/r":        false,
		"ext::ssh -i key host %S r": false,
	} {
		if got := prefetchable(location); got != want {
			t.Errorf("prefetchable(%q) = %v, want %v", location, got, want)
		}
	}
	var none *remoteRefs
	none.prefetch("https://example.invalid/repo")
	url := "file://" + filepath.ToSlash(newRunSourceTestRepo(t))
	if _, err := none.resolve(context.Background(), url, "", false); err != nil {
		t.Fatalf("nil remoteRefs resolve: %v", err)
	}
}

// Which transport a remote is reached over is git's answer, rewrites and all:
// an https URL that a url.<base>.insteadOf turns into ssh is not started
// ahead of its turn, and is looked up in it.
func TestRemoteRefsDoNotPrefetchARemoteRewrittenToSSH(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url.git@example.invalid:.insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", "https://example.invalid/")
	refs := newRemoteRefs(context.Background(), nil)
	url := "https://example.invalid/o/r"
	refs.prefetch(url)
	lookup := refs.lookups[remoteLookupKey(url, "", false)]
	if lookup == nil {
		t.Fatal("no lookup recorded for the remote")
	}
	<-lookup.done
	if !errors.Is(lookup.err, errAsksItself) {
		t.Fatalf("prefetch of a remote rewritten to ssh = %+v, %v; want it left to its turn", lookup.ref, lookup.err)
	}
	if !lookInTurn(lookup.err) {
		t.Fatal("a remote left to its turn must be looked up in it")
	}
}

// A remote that wants a credential fails a prefetch rather than asking for
// one — not on the terminal, and not through an askpass program, which git
// runs before it looks at its prompt setting and an IDE's terminal points at
// a dialog — and the failure is one to look up again in its turn.
func TestRemoteRefsPrefetchAsksNobodyForACredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	asked := filepath.Join(t.TempDir(), "asked")
	askpass := filepath.Join(t.TempDir(), "askpass.sh")
	if err := os.WriteFile(askpass, []byte("#!/bin/sh\necho \"$*\" >> "+asked+"\necho x\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_ASKPASS", askpass)
	t.Setenv("SSH_ASKPASS", askpass)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.askPass")
	t.Setenv("GIT_CONFIG_VALUE_0", askpass)

	refs := newRemoteRefs(context.Background(), nil)
	url := server.URL + "/o/r.git"
	refs.prefetch(url)
	lookup := refs.lookups[remoteLookupKey(url, "", false)]
	if lookup == nil {
		t.Fatal("no lookup started for an http remote")
	}
	<-lookup.done
	if got, err := os.ReadFile(asked); err == nil {
		t.Fatalf("the prefetch ran an askpass program: %q", got)
	}
	if !lookInTurn(lookup.err) {
		t.Fatalf("prefetch error = %v, want a failure to ask, to be looked up in its turn", lookup.err)
	}
}

// A frontend that can hand the terminal over is given the lookup made again in
// its turn, with git's environment as the person left it — prompt and
// language: the window steps aside for it as it does for lending's credential
// fill, so git's question is answered on a terminal that is git's for the
// while. What git said is still the error, and it names the remote and how to
// give git a credential.
func TestRemoteRefsLookupInTurnRunsThroughTheTerminalHandoff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	t.Setenv("GIT_ASKPASS", "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_TERMINAL_PROMPT", "")
	os.Unsetenv("GIT_TERMINAL_PROMPT")
	t.Setenv("LC_ALL", "")
	os.Unsetenv("LC_ALL")

	var handed []*exec.Cmd
	runGit := func(_ context.Context, cmd *exec.Cmd) error {
		handed = append(handed, cmd)
		_, _ = cmd.Stderr.Write([]byte("fatal: Authentication failed for 'https://example.test/o/r.git/'\n"))
		return errors.New("exit status 128")
	}
	refs := newRemoteRefs(context.Background(), runGit)
	url := server.URL + "/o/r.git"
	refs.prefetch(url)
	_, err := refs.resolve(context.Background(), url, "", false)
	if len(handed) != 1 {
		t.Fatalf("terminal handed to %d lookups, want the one made in its turn", len(handed))
	}
	cmd := handed[0]
	if !slices.Contains(cmd.Args, "ls-remote") || !slices.Contains(cmd.Args, url) {
		t.Fatalf("handed command = %v, want the ls-remote of %s", cmd.Args, url)
	}
	for _, name := range []string{"GIT_TERMINAL_PROMPT", "LC_ALL"} {
		if value, set := lastEnv(cmd.Env, name); set {
			t.Fatalf("handed lookup set %s=%q, want git's environment as the person left it", name, value)
		}
	}
	if err == nil || !strings.Contains(err.Error(), "Authentication failed") || !strings.Contains(err.Error(), "gh auth setup-git") || !strings.Contains(err.Error(), server.URL) {
		t.Fatalf("resolve error = %v, want git's own message, the remote and how to give git a credential", err)
	}
}

// A lookup that needs nobody never takes the terminal: in its turn it first
// looks without asking anything, and only a failure for want of asking hands
// the terminal over. Otherwise every create from ssh, whose lookups are all
// made in their turn, would blank the window for nothing.
func TestRemoteRefsLookupInTurnTakesNoTerminalWhenNobodyIsAsked(t *testing.T) {
	url := "file://" + filepath.ToSlash(newRunSourceTestRepo(t))
	runGit := func(context.Context, *exec.Cmd) error {
		t.Fatal("a lookup that needs no credential was handed the terminal")
		return nil
	}
	refs := newRemoteRefs(context.Background(), runGit)
	refs.lookups[remoteLookupKey(url, "", false)] = func() *remoteRefLookup {
		lookup := &remoteRefLookup{done: make(chan struct{}), err: errAsksItself}
		close(lookup.done)
		return lookup
	}()
	got, err := refs.resolve(context.Background(), url, "", false)
	if err != nil || len(got.commit) != 40 {
		t.Fatalf("resolve = %+v, %v; want a pinned commit", got, err)
	}
}

// With nobody to hand the terminal to, a remote that wants a credential fails
// saying which remote and how to give git one, rather than hanging on a prompt
// nobody can see.
func TestRemoteRefsLookupInTurnWithoutAHandoffSaysHowToGiveACredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	t.Setenv("GIT_ASKPASS", "")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)

	refs := newRemoteRefs(context.Background(), nil)
	url := server.URL + "/o/r.git"
	refs.prefetch(url)
	_, err := refs.resolve(context.Background(), url, "", false)
	if err == nil || !strings.Contains(err.Error(), "gh auth setup-git") || !strings.Contains(err.Error(), server.URL) {
		t.Fatalf("resolve error = %v, want the remote named and how to give git a credential", err)
	}
}

func lastEnv(env []string, key string) (value string, set bool) {
	for _, entry := range env {
		if k, v, ok := strings.Cut(entry, "="); ok && k == key {
			value, set = v, true
		}
	}
	return value, set
}
