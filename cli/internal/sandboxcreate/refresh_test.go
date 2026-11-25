package sandboxcreate

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
)

// refreshPool is the fake pool, with the control plane's answers to a pull of
// a delivered source scripted in order, and the bodies it was sent.
type refreshPool struct {
	*fakePool
	answers []apiclientgen.PullSandboxSourceRes
	bodies  []*apimodel.PullSandboxSourceBody
}

func (p *refreshPool) PullSandboxSource(_ context.Context, body *apimodel.PullSandboxSourceBody, _ apiclientgen.PullSandboxSourceParams) (apiclientgen.PullSandboxSourceRes, error) {
	p.bodies = append(p.bodies, body)
	if len(p.answers) == 0 {
		p.t.Fatal("the pool was asked to refresh more often than the test expected")
	}
	answer := p.answers[0]
	p.answers = p.answers[1:]
	return answer, nil
}

func refreshAnswer(status apiclientgen.SourceRefreshStatus) *apiclientgen.SourceRefresh {
	answer := &apiclientgen.SourceRefresh{Status: status}
	if status == apiclientgen.SourceRefreshStatusNeedsCredential {
		answer.Protocol, answer.Host, answer.Path = apiclientgen.NewOptString("https"), apiclientgen.NewOptString("example.com"), apiclientgen.NewOptString("acme/widget.git")
	}
	return answer
}

// refreshFixture delivers a remote source into the fake pool, and returns
// the pool, the remote's working repository and the delivered source.
func refreshFixture(t *testing.T) (*refreshPool, string, apimodel.GitSource) {
	t.Helper()
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "safe.bareRepository")
	t.Setenv("GIT_CONFIG_VALUE_0", "explicit")
	t.Setenv("DISCOBOX", "")
	pool := &refreshPool{fakePool: newFakePool(t)}
	useClientGitCache(t)
	remoteWork := t.TempDir()
	deliverGit(t, remoteWork, "init", "--quiet", "--initial-branch=main")
	first := deliverCommit(t, remoteWork, "a")
	deliverGit(t, remoteWork, "tag", "-a", "-m", "v1", "v1")
	deliverGit(t, remoteWork, "branch", "release")
	u, err := url.Parse("file://" + filepath.ToSlash(remoteWork))
	if err != nil {
		t.Fatal(err)
	}
	source := apimodel.GitSource{Checkout: branchCheckout(first, "main"), URL: apiclientgen.NewOptURI(*u)}
	sandbox := cacheSandbox("sbx-one", source)
	if err := DeliverSource(t.Context(), pool, "p", sandbox, nil, pool.server.URL, "token", nil, DeliveryOptions{}); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	return pool, remoteWork, sandbox.Config.Source.Value
}

func updateList(result RefreshResult) string {
	var out []string
	for _, update := range result.Updates {
		out = append(out, update.Ref+" "+shortID(update.Old)+" "+shortID(update.New))
	}
	return strings.Join(out, "|")
}

func shortID(id string) string { return id[:min(len(id), 7)] }

// A pool that refreshed the source itself is the whole of it: nothing is
// fetched or pushed here, and what the pool moved is what is reported.
func TestARefreshThePoolMadeIsTheWholeOfIt(t *testing.T) {
	pool, _, source := refreshFixture(t)
	answer := refreshAnswer(apiclientgen.SourceRefreshStatusRefreshed)
	answer.Updates = []apiclientgen.SourceRefUpdate{{Ref: "refs/remotes/origin/main", Old: apiclientgen.NewOptString("aaaaaaa"), New: apiclientgen.NewOptString("bbbbbbb")}}
	pool.answers = append(pool.answers, answer)
	pushes := pool.pushes.Load()
	result, err := RefreshSource(t.Context(), pool, pool.server.URL, "token", "p", "sbx-one", source, RefreshOptions{Branch: "staging"})
	if err != nil || result.By != RefreshedByPool || updateList(result) != "refs/remotes/origin/main aaaaaaa bbbbbbb" {
		t.Fatalf("refresh = %+v, %v", result, err)
	}
	if body := pool.bodies[0]; body.Password.IsSet() || body.Branch.Or("") != "staging" {
		t.Fatalf("the pool was asked %+v; want an anonymous refresh of staging", body)
	}
	if pool.pushes.Load() != pushes {
		t.Fatal("a refresh the pool made pushed from here too")
	}
}

// When the pool cannot reach the remote, this machine fetches it and pushes
// what moved, as a refresh: the remote's branches, a new one, and one the
// remote dropped, and never the box's own branch.
func TestARefreshThePoolCannotReachIsMadeHere(t *testing.T) {
	pool, remoteWork, source := refreshFixture(t)
	delivered := pool.refs("sbx-one", "primary")
	second := deliverCommit(t, remoteWork, "b")
	deliverGit(t, remoteWork, "branch", "staging")
	deliverGit(t, remoteWork, "branch", "-D", "release")
	pool.answers = append(pool.answers, refreshAnswer(apiclientgen.SourceRefreshStatusUnreachable))
	result, err := RefreshSource(t.Context(), pool, pool.server.URL, "token", "p", "sbx-one", source, RefreshOptions{})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	first := shortID(delivered["refs/heads/main"])
	if want := "refs/remotes/origin/main " + first + " " + shortID(second) + "|refs/remotes/origin/release  |refs/remotes/origin/staging  " + shortID(second); updateList(result) != want {
		t.Fatalf("updates = %s, want %s", updateList(result), want)
	}
	if old := result.Updates[0].Old; old != delivered["refs/heads/main"] {
		t.Fatalf("origin/main's old value = %q, want the full %s", old, delivered["refs/heads/main"])
	}
	if result.By != RefreshedThisMachine || !result.Unreachable {
		t.Fatalf("refresh = %+v", result)
	}
	got := pool.refs("sbx-one", "primary")
	if got["refs/remotes/origin/main"] != second || got["refs/remotes/origin/staging"] != second || got["refs/remotes/origin/release"] != "" {
		t.Fatalf("namespace after the refresh = %v", got)
	}
	if got["refs/heads/main"] != delivered["refs/heads/main"] || got["refs/tags/v1"] != delivered["refs/tags/v1"] {
		t.Fatalf("a refresh moved the box's branch or a tag: %v", got)
	}

	// Again: already current, nothing sent.
	pushes := pool.pushes.Load()
	pool.answers = append(pool.answers, refreshAnswer(apiclientgen.SourceRefreshStatusUnreachable))
	if result, err = RefreshSource(t.Context(), pool, pool.server.URL, "token", "p", "sbx-one", source, RefreshOptions{}); err != nil || len(result.Updates) != 0 || pool.pushes.Load() != pushes {
		t.Fatalf("a refresh of a current source = %+v, %v, %d pushes", result, err, pool.pushes.Load()-pushes)
	}

	// One branch the remote does not have is named.
	pool.answers = append(pool.answers, refreshAnswer(apiclientgen.SourceRefreshStatusUnreachable))
	if _, err = RefreshSource(t.Context(), pool, pool.server.URL, "token", "p", "sbx-one", source, RefreshOptions{Branch: "absent"}); err == nil || !strings.Contains(err.Error(), `no branch "absent"`) {
		t.Fatalf("refresh of a missing branch = %v", err)
	}
}

// A private remote's refresh follows the credential policy: ask with nobody
// to ask is refused for the caller to skip, local fetches here, and lend
// lends what the helper fills to the pool's pull.
func TestAPrivateRemotesRefreshFollowsTheCredentialPolicy(t *testing.T) {
	pool, remoteWork, source := refreshFixture(t)
	needs := func() {
		pool.answers = append(pool.answers, refreshAnswer(apiclientgen.SourceRefreshStatusNeedsCredential))
	}

	needs()
	if _, err := RefreshSource(t.Context(), pool, pool.server.URL, "token", "p", "sbx-one", source, RefreshOptions{Delivery: DeliveryOptions{Credentials: GitCredentialsAsk, ConfigPath: "client.yaml"}}); !errors.Is(err, ErrNeedsCredentialPolicy) || !strings.Contains(err.Error(), "--git-credentials=local") {
		t.Fatalf("ask with nobody to ask = %v", err)
	}

	second := deliverCommit(t, remoteWork, "b")
	needs()
	result, err := RefreshSource(t.Context(), pool, pool.server.URL, "token", "p", "sbx-one", source, RefreshOptions{Delivery: DeliveryOptions{Credentials: GitCredentialsLocal}})
	if err != nil || result.By != RefreshedThisMachine || pool.refs("sbx-one", "primary")["refs/remotes/origin/main"] != second {
		t.Fatalf("local = %+v, %v", result, err)
	}

	// The helper fills a credential; the pool's lent pull takes it.
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_1", "credential.helper")
	t.Setenv("GIT_CONFIG_VALUE_1", "!f() { echo username=reader; echo password=secret; }; f")
	needs()
	pool.answers = append(pool.answers, refreshAnswer(apiclientgen.SourceRefreshStatusRefreshed))
	var asked string
	confirm := func(_ context.Context, target string) (bool, error) { asked = target; return true, nil }
	result, err = RefreshSource(t.Context(), pool, pool.server.URL, "token", "p", "sbx-one", source, RefreshOptions{Delivery: DeliveryOptions{Credentials: GitCredentialsAsk, ConfirmCredential: confirm}})
	if err != nil || result.By != RefreshedByPoolLent || asked != "example.com/acme/widget.git" {
		t.Fatalf("ask, then lend = %+v, %v (asked about %q)", result, err, asked)
	}
	if lent := pool.bodies[len(pool.bodies)-1]; lent.Username.Or("") != "reader" || lent.Password.Or("") != "secret" {
		t.Fatalf("lent %+v", lent)
	}
}

// A discobox never lends: inside one, a private remote is fetched by the
// box's own Git, whatever the policy says.
func TestADiscoboxRefreshesAPrivateRemoteItself(t *testing.T) {
	pool, remoteWork, source := refreshFixture(t)
	second := deliverCommit(t, remoteWork, "b")
	t.Setenv("DISCOBOX", "1")
	pool.answers = append(pool.answers, refreshAnswer(apiclientgen.SourceRefreshStatusNeedsCredential))
	result, err := RefreshSource(t.Context(), pool, pool.server.URL, "token", "p", "sbx-one", source, RefreshOptions{Delivery: DeliveryOptions{Credentials: GitCredentialsLend}})
	if err != nil || result.By != RefreshedThisMachine || pool.refs("sbx-one", "primary")["refs/remotes/origin/main"] != second {
		t.Fatalf("refresh in a discobox = %+v, %v", result, err)
	}
}
