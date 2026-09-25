package sandboxcreate

import (
	"context"
	"errors"
	"net/http/cgi" //nolint:gosec // serves a test origin through git-http-backend; httpoxy (CVE-2016-5386) is fixed in every Go this builds with
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxgit"
)

// awaitClient answers each read from the script, one entry per read, holding on
// the last. It is only ever asked for the sandbox, because the wait is over
// before anything is pushed.
type awaitClient struct {
	runtimes []apimodel.SandboxRuntime
	reads    int
	// pool is what the pool read answers, for the stretch of the wait where the
	// sandbox has nothing to say but the pool does.
	pool *apimodel.Pool
	// completeErr is what a completion answers, and completeOK accepts one
	// instead. Unset, a completion panics: most of these never get that far,
	// and one that does is the bug.
	completeErr error
	completeOK  bool
	completes   int
}

func (c *awaitClient) GetSandbox(context.Context, apiclientgen.GetSandboxParams) (apiclientgen.GetSandboxRes, error) {
	runtime := c.runtimes[min(c.reads, len(c.runtimes)-1)]
	c.reads++
	return &apimodel.Sandbox{ID: "sbx_1", PoolId: apiclientgen.NewOptString("pool_1"), Runtime: runtime}, nil
}

func (c *awaitClient) GetPool(context.Context, apiclientgen.GetPoolParams) (apiclientgen.GetPoolRes, error) {
	if c.pool == nil {
		return nil, errNoPool
	}
	return c.pool, nil
}

func (c *awaitClient) CompleteSandboxSourcePush(context.Context, *apimodel.CompleteSandboxSourcePushBody, apiclientgen.CompleteSandboxSourcePushParams) (apiclientgen.CompleteSandboxSourcePushRes, error) {
	if c.completeErr == nil && !c.completeOK {
		panic("the wait does not push")
	}
	c.completes++
	if c.completeOK {
		return &apimodel.Sandbox{ID: "sbx_1"}, nil
	}
	return nil, c.completeErr
}

var errNoPool = errors.New("no pool")

// pulling is a sandbox mid-pull, reporting as of now.
func pulling(current, total int64) apimodel.SandboxRuntime {
	runtime := recentProgress(apiclientgen.SandboxProvisionPhasePullingImage)
	runtime.ProvisionProgress = apiclientgen.NewOptSandboxProvisionProgress(apimodel.SandboxProvisionProgress{
		Phase: apiclientgen.SandboxProvisionPhasePullingImage,
		Pull: apiclientgen.NewOptSandboxPullProgress(apimodel.SandboxPullProgress{
			Image:   "ghcr.io/discobox-ai/discobox-vm:v2",
			Current: apiclientgen.NewOptInt64(current),
			Total:   apiclientgen.NewOptInt64(total),
		}),
	})
	return runtime
}

// The wait for a discobox to park says what it is waiting for. Everything that
// happens before a sandbox can accept a push — the pull above all — is recorded
// on the sandbox this loop is already reading, and a pull is the reason this
// step is the long one (ADR 0060).
func TestAwaitSourceRequestedNarratesTheProvisioningItWaitsOn(t *testing.T) {
	client := &awaitClient{runtimes: []apimodel.SandboxRuntime{
		pulling(100*1024*1024, 400*1024*1024),
		// The same counts again: an unchanged line is not reported twice, so a
		// caller that renders every report does not flicker.
		pulling(100*1024*1024, 400*1024*1024),
		pulling(300*1024*1024, 400*1024*1024),
		recentProgress(apiclientgen.SandboxProvisionPhaseCreatingContainer),
		{State: apiclientgen.SandboxRuntimeStateAwaitingSource},
	}}

	var reported []Step
	report := Report(func(step Step) { reported = append(reported, step) })
	if _, err := awaitSourceRequested(t.Context(), client, "project-1", "sbx_1", report); err != nil {
		t.Fatalf("await: %v", err)
	}

	want := []Step{
		"pulling discobox-vm:v2 — 100.0 MiB of 400.0 MiB",
		"pulling discobox-vm:v2 — 300.0 MiB of 400.0 MiB",
		"creating the container",
	}
	if len(reported) != len(want) {
		t.Fatalf("reported %q, want %q", reported, want)
	}
	for i, line := range want {
		if reported[i] != line {
			t.Fatalf("reported[%d] = %q, want %q", i, reported[i], line)
		}
	}
}

// A discobox that is already parked reports nothing: the step the caller put on
// the line before this was called is still what is happening, and replacing it
// with a phase read off the record would say the same thing in other words.
func TestAwaitSourceRequestedSaysNothingAboutASandboxAlreadyParked(t *testing.T) {
	client := &awaitClient{runtimes: []apimodel.SandboxRuntime{
		{State: apiclientgen.SandboxRuntimeStateAwaitingSource},
	}}

	report := Report(func(step Step) { t.Errorf("reported %q for a discobox that was already waiting", step) })
	if _, err := awaitSourceRequested(t.Context(), client, "project-1", "sbx_1", report); err != nil {
		t.Fatalf("await: %v", err)
	}
	if client.reads != 1 {
		t.Fatalf("read the discobox %d times, want one", client.reads)
	}
}

// A nil Report is what a non-interactive caller passes, and the wait still
// waits. Nothing here may depend on someone listening.
func TestAwaitSourceRequestedToleratesNoReport(t *testing.T) {
	client := &awaitClient{runtimes: []apimodel.SandboxRuntime{
		recentProgress(apiclientgen.SandboxProvisionPhaseCreatingContainer),
		{State: apiclientgen.SandboxRuntimeStateAwaitingSource},
	}}

	start := time.Now()
	if _, err := awaitSourceRequested(t.Context(), client, "project-1", "sbx_1", nil); err != nil {
		t.Fatalf("await: %v", err)
	}
	if elapsed := time.Since(start); elapsed < awaitSourcePollInterval {
		t.Fatalf("returned after %s, want a wait that paced itself", elapsed)
	}
}

// The wait that a sandbox cannot narrate is the one the pool can: while no pool
// has taken it, what the pool's driver is doing is the whole of the answer.
func TestAwaitSourceRequestedNarratesThePoolWhileNoPoolHasTakenIt(t *testing.T) {
	pending := apimodel.SandboxRuntime{State: apiclientgen.SandboxRuntimeStatePending}
	stamp := time.Now().UTC()
	client := &awaitClient{
		runtimes: []apimodel.SandboxRuntime{pending, pending, {State: apiclientgen.SandboxRuntimeStateAwaitingSource}},
		pool: &apimodel.Pool{
			ProvisionProgress: apiclientgen.NewOptPoolProvisionProgress(apimodel.PoolProvisionProgress{
				Phase: apiclientgen.PoolProvisionPhaseFetchingVMImage,
			}),
			ProvisionProgressAt: apiclientgen.NewOptDateTime(stamp),
		},
	}
	var steps []Step
	report := Report(func(step Step) { steps = append(steps, step) })
	if _, err := awaitSourceRequested(t.Context(), client, "proj_1", "sbx_1", report); err != nil {
		t.Fatal(err)
	}
	for _, step := range steps {
		if step == StepWaitingForPool {
			t.Fatalf("reported %q when the pool had something to say", step)
		}
	}
	if len(steps) == 0 || steps[0] != "fetching the VM image" {
		t.Fatalf("steps = %v, want the pool's own phase first", steps)
	}
}

// A delivery somebody else reported ends the wait with nothing left to push,
// whatever state the discobox has moved on to since (ADR 26-09-24-005). Waiting for
// awaiting_source instead would wait out the stall budget on a discobox that is
// already starting.
func TestAwaitSourceRequestedStopsAtADeliveryAlreadyReported(t *testing.T) {
	delivered := apimodel.SandboxRuntime{
		State:             apiclientgen.SandboxRuntimeStateReady,
		SourceDeliveredAt: apiclientgen.NewOptDateTime(time.Now()),
	}
	client := &awaitClient{runtimes: []apimodel.SandboxRuntime{delivered}}
	got, err := awaitSourceRequested(t.Context(), client, "project-1", "sbx_1", nil)
	if err != nil {
		t.Fatalf("await: %v", err)
	}
	if !got {
		t.Fatal("delivered = false, want the reported delivery noticed")
	}
	if client.reads != 1 {
		t.Fatalf("reads = %d, want the first answer to end the wait", client.reads)
	}
}

// parkedWithPushSource is a discobox parked waiting for one push-delivered
// source at commit.
func parkedWithPushSource(commit string) *apimodel.Sandbox {
	sandbox := &apimodel.Sandbox{ID: "sbx_1"}
	sandbox.Config.SetSource(apiclientgen.NewOptGitSource(apimodel.GitSource{
		Slug:     apiclientgen.NewOptString("primary"),
		Delivery: apiclientgen.NewOptGitSourceDelivery(apiclientgen.GitSourceDeliveryPush),
		Checkout: apiclientgen.NewOptGitSourceCheckout(apimodel.GitSourceCheckout{
			Commit:  apiclientgen.NewOptString(commit),
			RefName: apiclientgen.NewOptString("feature-foo"),
			RefType: apiclientgen.NewOptString(runSourceRefTypeBranch),
		}),
	}))
	return sandbox
}

// A push refused because another client delivered the same discobox first is
// not a failed delivery: once that delivery is reported there is nothing left
// to send or report (ADR 26-09-24-005 §3). The push here is refused by an origin that
// is not there at all, which is the same refusal from this side.
func TestDeliverSourceStopsWhenAnotherDeliveryIsReported(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	commit := strings.TrimSpace(runSourceTestGit(t, repo)("rev-parse", "HEAD"))
	parked := apimodel.SandboxRuntime{State: apiclientgen.SandboxRuntimeStateAwaitingSource}
	delivered := parked
	delivered.SourceDeliveredAt = apiclientgen.NewOptDateTime(time.Now())
	// awaitClient panics on a completion, which is the assertion that nothing
	// is reported twice.
	client := &awaitClient{runtimes: []apimodel.SandboxRuntime{parked, delivered}}

	err := DeliverSource(t.Context(), client, "project-1", parkedWithPushSource(commit), NewLocalSources(map[string]string{"": repo}), "http://127.0.0.1:1", "token", nil)
	if err != nil {
		t.Fatalf("DeliverSource: %v, want the other delivery to stand for this one", err)
	}
}

// With no other delivery reported, a refused push is made once more and then
// believed.
func TestDeliverSourceReportsAPushThatKeepsFailing(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	commit := strings.TrimSpace(runSourceTestGit(t, repo)("rev-parse", "HEAD"))
	client := &awaitClient{runtimes: []apimodel.SandboxRuntime{{State: apiclientgen.SandboxRuntimeStateAwaitingSource}}}

	err := DeliverSource(t.Context(), client, "project-1", parkedWithPushSource(commit), NewLocalSources(map[string]string{"": repo}), "http://127.0.0.1:1", "token", nil)
	if err == nil || !strings.Contains(err.Error(), "push source to discobox") {
		t.Fatalf("DeliverSource = %v, want the push's own failure", err)
	}
	if client.reads != 2 {
		t.Fatalf("reads = %d, want the wait's read and one after the refused push", client.reads)
	}
}

// gitOriginServer serves the discobox's origin repository for the source slug
// "primary" over git's own smart HTTP, so a delivery can push for real.
func gitOriginServer(t *testing.T) string {
	t.Helper()
	backend, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	execPath, err := exec.CommandContext(t.Context(), backend, "--exec-path").Output()
	if err != nil {
		t.Skipf("git --exec-path: %v", err)
	}
	httpBackend := filepath.Join(strings.TrimSpace(string(execPath)), "git-http-backend")
	if _, err := os.Stat(httpBackend); err != nil {
		t.Skip("git-http-backend is not installed")
	}
	root := t.TempDir()
	origin := filepath.Join(root, "projects", "project-1", "sandboxes", "sbx_1", "git-origins", "primary.git")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	runSourceTestGit(t, origin)("init", "--bare")
	runSourceTestGit(t, origin)("config", "http.receivepack", "true")
	server := httptest.NewServer(&cgi.Handler{
		Path: httpBackend,
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"},
	})
	t.Cleanup(server.Close)
	return server.URL
}

// A completion refused because another delivery reported first — the
// discobox has moved on and is no longer awaiting its source — is a delivery
// made, not a failed one (ADR 26-09-24-005 §3).
func TestDeliverSourceStopsWhenAnotherDeliveryReportedFirst(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	commit := strings.TrimSpace(runSourceTestGit(t, repo)("rev-parse", "HEAD"))
	serverURL := gitOriginServer(t)
	parked := apimodel.SandboxRuntime{State: apiclientgen.SandboxRuntimeStateAwaitingSource}
	delivered := apimodel.SandboxRuntime{State: apiclientgen.SandboxRuntimeStateReady, SourceDeliveredAt: apiclientgen.NewOptDateTime(time.Now())}

	client := &awaitClient{runtimes: []apimodel.SandboxRuntime{parked, delivered}, completeErr: errors.New("sandbox is not awaiting its source")}
	if err := DeliverSource(t.Context(), client, "project-1", parkedWithPushSource(commit), NewLocalSources(map[string]string{"": repo}), serverURL, "token", nil); err != nil {
		t.Fatalf("DeliverSource: %v, want the other delivery to stand for this one", err)
	}

	// Nobody else reported it: the refusal is the answer.
	client = &awaitClient{runtimes: []apimodel.SandboxRuntime{parked}, completeErr: errors.New("sandbox is not awaiting its source")}
	err := DeliverSource(t.Context(), client, "project-1", parkedWithPushSource(commit), NewLocalSources(map[string]string{"": repo}), serverURL, "token", nil)
	if err == nil || !strings.Contains(err.Error(), "not awaiting its source") {
		t.Fatalf("DeliverSource = %v, want the refused completion", err)
	}
	if client.completes != 1 {
		t.Fatalf("completes = %d, want one", client.completes)
	}
}

// A delivery owed to a discobox whose origin this client has since pushed newer
// commits into — `discobox push` moved the branch past the pinned commit before
// the discobox came back to awaiting its source — is already made. Pushing the
// pinned commit onto that branch is a non-fast-forward the origin refuses on
// every attempt, so the branch is left where it is, and so is the lease the next
// `discobox push` holds.
func TestDeliverSourceLeavesABranchThatHasMovedPastThePinnedCommit(t *testing.T) {
	repo := newRunSourceTestRepo(t)
	git := runSourceTestGit(t, repo)
	pinned := strings.TrimSpace(git("rev-parse", "HEAD"))
	git("commit", "--allow-empty", "-m", "later")
	later := strings.TrimSpace(git("rev-parse", "HEAD"))
	serverURL := gitOriginServer(t)
	originURL := serverURL + "/projects/project-1/sandboxes/sbx_1/git-origins/primary.git"
	leaseRef := sandboxgit.OriginLeaseRef("sbx_1", "primary", "feature-foo")
	git("push", originURL, later+":refs/heads/feature-foo")
	git("update-ref", leaseRef, later)

	client := &awaitClient{runtimes: []apimodel.SandboxRuntime{{State: apiclientgen.SandboxRuntimeStateAwaitingSource}}, completeOK: true}
	if err := DeliverSource(t.Context(), client, "project-1", parkedWithPushSource(pinned), NewLocalSources(map[string]string{"": repo}), serverURL, "token", nil); err != nil {
		t.Fatalf("DeliverSource: %v, want the origin's branch to stand for the pinned commit", err)
	}
	if client.completes != 1 {
		t.Fatalf("completes = %d, want the delivery reported", client.completes)
	}
	if tip := strings.Fields(git("ls-remote", originURL, "refs/heads/feature-foo")); len(tip) == 0 || tip[0] != later {
		t.Fatalf("origin feature-foo = %v, want it left at %s", tip, later)
	}
	if lease := strings.TrimSpace(git("rev-parse", leaseRef)); lease != later {
		t.Fatalf("lease = %s, want it left at %s", lease, later)
	}
}
