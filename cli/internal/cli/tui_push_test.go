package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	apiclientgen "github.com/discobox-ai/discobox/api/gen"
	apimodel "github.com/discobox-ai/discobox/api/model"
	"github.com/discobox-ai/discobox/cli/internal/sandboxgit"
	"github.com/discobox-ai/discobox/internal/hostid"
)

// pushDeliveredSandbox is a discobox created here from a directory this machine
// still holds, whose source was delivered by pushing it — the one shape the
// window pushes for.
func pushDeliveredSandbox() apimodel.Sandbox {
	source := apimodel.GitSource{
		Slug:           apiclientgen.NewOptString("primary"),
		Delivery:       apiclientgen.NewOptGitSourceDelivery(apiclientgen.GitSourceDeliveryPush),
		LocalDirectory: apiclientgen.NewOptString("/src/disco2"),
		Checkout: apiclientgen.NewOptGitSourceCheckout(apimodel.GitSourceCheckout{
			RefName: apiclientgen.NewOptString("main"),
			RefType: apiclientgen.NewOptString("branch"),
		}),
	}
	sandbox := apimodel.Sandbox{
		Runtime: apimodel.SandboxRuntime{
			State:        apiclientgen.SandboxRuntimeStateReady,
			DesiredState: "present",
			DisplayState: apiclientgen.NewOptSandboxRuntimeDisplayState("running"),
		},
	}
	sandbox.Origin = apiclientgen.NewOptOrigin(apiclientgen.Origin{HostId: thisHost, ProjectPath: "/src/disco2"})
	sandbox.Config.SetSource(apiclientgen.NewOptGitSource(source))
	return sandbox
}

// The row fact the window's automatic push reads: what it is true of, and every
// reason it is not (ADR 0095 §2 on automatic push).
func TestPushableIsThisMachinesPushDeliveredDiscoboxes(t *testing.T) {
	remote := pushDeliveredSandbox()
	remoteSource, _ := remote.Config.Source.Get()
	remoteSource.Delivery.Reset()
	remoteSource.LocalDirectory.Reset() // nothing here to push from: it clones the URL itself
	remote.Config.SetSource(apiclientgen.NewOptGitSource(remoteSource))

	bound := pushDeliveredSandbox()
	boundSource, _ := bound.Config.Source.Get()
	boundSource.Delivery.Reset() // read live off the directory instead
	bound.Config.SetSource(apiclientgen.NewOptGitSource(boundSource))

	archived := pushDeliveredSandbox()
	archived.Runtime.DisplayState = apiclientgen.NewOptSandboxRuntimeDisplayState("archived")

	parked := pushDeliveredSandbox()
	parked.Runtime.State = apiclientgen.SandboxRuntimeStateAwaitingSource

	elsewhere := pushDeliveredSandbox()
	elsewhere.Origin = apiclientgen.NewOptOrigin(apiclientgen.Origin{HostId: "hst_othermachine0002"})

	noOrigin := pushDeliveredSandbox()
	noOrigin.Origin.Reset()

	// A source checked out at a tag or a bare commit names no branch, so
	// `discobox push` would fall back to whatever HEAD is now — which on a
	// clock is whatever the developer has switched to since.
	tagged := pushDeliveredSandbox()
	taggedSource, _ := tagged.Config.Source.Get()
	taggedSource.SetCheckout(apiclientgen.NewOptGitSourceCheckout(apimodel.GitSourceCheckout{
		RefName: apiclientgen.NewOptString("v1.2.3"),
		RefType: apiclientgen.NewOptString("tag"),
	}))
	tagged.Config.SetSource(apiclientgen.NewOptGitSource(taggedSource))

	detached := pushDeliveredSandbox()
	detachedSource, _ := detached.Config.Source.Get()
	detachedSource.SetCheckout(apiclientgen.NewOptGitSourceCheckout(apimodel.GitSourceCheckout{
		Commit:  apiclientgen.NewOptString("a3f9c2179bbf0f4e2e9d1a7c5b6d8e0f11223344"),
		RefType: apiclientgen.NewOptString("commit"),
	}))
	detached.Config.SetSource(apiclientgen.NewOptGitSource(detachedSource))

	for _, tc := range []struct {
		name    string
		sandbox apimodel.Sandbox
		hostID  string
		want    bool
	}{
		{"pushed from here", pushDeliveredSandbox(), thisHost, true},
		{"cloned from a remote", remote, thisHost, false},
		{"read live off the directory", bound, thisHost, false},
		{"archived", archived, thisHost, false},
		{"still awaiting its source", parked, thisHost, false},
		{"created on another machine", elsewhere, thisHost, false},
		{"no recorded origin", noOrigin, thisHost, false},
		{"this machine has no identity", pushDeliveredSandbox(), "", false},
		{"checked out at a tag", tagged, thisHost, false},
		{"checked out at a bare commit", detached, thisHost, false},
	} {
		if got := pushable(tc.sandbox, tc.hostID); got != tc.want {
			t.Errorf("%s: pushable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A second source can need a push while the primary one is bound, so the row is
// pushable when any source is.
func TestPushableFollowsASecondSource(t *testing.T) {
	sandbox := pushDeliveredSandbox()
	primary, _ := sandbox.Config.Source.Get()
	primary.Delivery.Reset()
	sandbox.Config.SetSource(apiclientgen.NewOptGitSource(primary))
	if pushable(sandbox, thisHost) {
		t.Fatal("a bound primary source is nothing to push")
	}

	reference := apimodel.GitSource{
		Slug:           apiclientgen.NewOptString("docs"),
		Delivery:       apiclientgen.NewOptGitSourceDelivery(apiclientgen.GitSourceDeliveryPush),
		LocalDirectory: apiclientgen.NewOptString("/src/docs"),
		Checkout: apiclientgen.NewOptGitSourceCheckout(apimodel.GitSourceCheckout{
			RefName: apiclientgen.NewOptString("main"),
			RefType: apiclientgen.NewOptString("branch"),
		}),
	}
	sandbox.Config.SetSourceCodeReferences(apiclientgen.NewOptSandboxConfigSourceCodeReferences(
		apiclientgen.SandboxConfigSourceCodeReferences{"docs": reference}))
	if !pushable(sandbox, thisHost) {
		t.Fatal("a push-delivered reference is something to push")
	}
}

// pushRepo is a client repository with one commit on main, standing in for the
// checkout a push-delivered discobox was created from.
func pushRepo(t *testing.T) (root, commit string) {
	t.Helper()
	root = t.TempDir()
	pushGit(t, root, "init", "-b", "main")
	pushGit(t, root, "config", "user.email", "test@example.com")
	pushGit(t, root, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pushGit(t, root, "add", "README.md")
	pushGit(t, root, "commit", "-m", "one")
	return root, pushGit(t, root, "rev-parse", "HEAD")
}

func pushGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
}

// pathLog is every request path the stub control plane was asked for. It is
// guarded because the automatic push runs on its own goroutine, so the test
// reads what the server's goroutine writes.
type pathLog struct {
	mu    sync.Mutex
	paths []string
}

func (l *pathLog) add(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paths = append(l.paths, path)
}

func (l *pathLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.paths...)
}

func (l *pathLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.paths = nil
}

// touched reports whether any request so far named part of a path.
func (l *pathLog) touched(part string) bool {
	for _, path := range l.all() {
		if strings.Contains(path, part) {
			return true
		}
	}
	return false
}

// pushDataSource is a data source over a stub control plane holding one
// push-delivered discobox cut from dir, and the paths it was asked for.
func pushDataSource(t *testing.T, dir string) (*apiDataSource, *pathLog) {
	t.Helper()
	t.Setenv(hostid.EnvVar, thisHost)
	t.Setenv("GIT_TERMINAL_PROMPT", "0")

	paths := &pathLog{}
	// Marshaled, not interpolated: a Windows temp directory is
	// C:\Users\..., and every one of those backslashes is an invalid JSON
	// escape inside a string literal.
	dirJSON, err := json.Marshal(dir)
	if err != nil {
		t.Fatalf("marshal %s: %v", dir, err)
	}
	sandbox := `{"id":"sbx_1","projectId":"project-1","createdByUserId":"user-1","displayName":"box",` +
		`"config":{"name":"box","image":"","source":{"kind":"git","slug":"primary","delivery":"push",` +
		`"localDirectory":` + string(dirJSON) + `,"checkout":{"refName":"main","refType":"branch"}}},` +
		`"origin":{"hostId":"` + thisHost + `","projectPath":` + string(dirJSON) + `},` +
		`"runtime":{"state":"ready","desiredState":"present","displayState":"running","generation":1,"observedGeneration":1},` +
		`"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths.add(r.URL.Path)
		if r.URL.Path == "/projects/project-1/sandboxes/sbx_1" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(sandbox))
			return
		}
		// Everything else is the git route, which this stub does not serve.
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	client, err := apiclientgen.NewClient(server.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return &apiDataSource{app: &App{serverURL: server.URL}, client: client, projectID: "project-1"}, paths
}

// The ordinary beat: nothing has been committed since the last look, so the
// answer comes off two local refs and nothing is dialed at all. The discobox
// itself is read once and held, since none of what it says can change.
func TestPushSourcesSendsNothingAndDialsNothingWhenNothingChanged(t *testing.T) {
	dir, commit := pushRepo(t)
	ds, paths := pushDataSource(t, dir)
	// The commit is already in the origin, by this client's own record of
	// putting it there.
	pushGit(t, dir, "update-ref", sandboxgit.OriginLeaseRef("sbx_1", "primary", "main"), commit)

	for look := 1; look <= 2; look++ {
		pushes, err := ds.PushSources(t.Context(), "sbx_1", nil)
		if err != nil {
			t.Fatalf("look %d: %v", look, err)
		}
		if len(pushes) != 1 || pushes[0].Pushed || pushes[0].Err != nil {
			t.Fatalf("look %d: pushes = %#v, want one source with nothing to send", look, pushes)
		}
		if pushes[0].Commit != commit || pushes[0].Branch != "main" {
			t.Fatalf("look %d: pushes = %#v, want %s on main", look, pushes, commit)
		}
	}
	if want := []string{"/projects/project-1/sandboxes/sbx_1"}; !slices.Equal(paths.all(), want) {
		t.Fatalf("requests = %v, want the discobox read once and no git route touched", paths.all())
	}
}

// A commit made here is sent, and a source already refused at exactly what its
// branch names now is resolved and left alone — no second rejected transfer.
func TestPushSourcesHoldsARefusedCommit(t *testing.T) {
	dir, commit := pushRepo(t)
	ds, paths := pushDataSource(t, dir)

	// Nothing leased and no origin to reach: the push is attempted, and the
	// stub control plane refuses it.
	pushes, err := ds.PushSources(t.Context(), "sbx_1", nil)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(pushes) != 1 || pushes[0].Err == nil || pushes[0].Commit != commit {
		t.Fatalf("pushes = %#v, want %s attempted and refused", pushes, commit)
	}
	if !paths.touched("git-origins") {
		t.Fatalf("requests = %v, want the git route dialed for a commit with nowhere to be", paths.all())
	}

	paths.reset()
	held := map[string]string{"primary": commit}
	pushes, err = ds.PushSources(t.Context(), "sbx_1", held)
	if err != nil {
		t.Fatalf("second look: %v", err)
	}
	if len(pushes) != 1 || pushes[0].Err != nil || pushes[0].Pushed {
		t.Fatalf("pushes = %#v, want the held commit resolved and not sent", pushes)
	}
	if got := paths.all(); len(got) != 0 {
		t.Fatalf("requests = %v, want nothing dialed for a commit already refused", got)
	}
}

// A raw attach has no window, so it pushes for itself: the same rule, for as
// long as the stream lasts (ADR 0095 §1 on automatic push). It says nothing
// into the stream while it runs, and reports what could not be pushed once the
// terminal is the client's again.
func TestAutoPushWhileAttachedPushesAndReportsOnlyOnStop(t *testing.T) {
	dir, _ := pushRepo(t)
	ds, paths := pushDataSource(t, dir)

	var report strings.Builder
	stop := ds.app.autoPushWhileAttached(t.Context(), ds.client, "project-1", "sbx_1")
	// The push is attempted against a stub that does not serve the git route,
	// so it fails — which is what puts something in the report.
	//
	// Stopping here lands mid-transfer as often as not: the stub records this
	// when git opens /info/refs, well before it has the 404. That is deliberate
	// and not a race — a transfer that has started cannot be interrupted and is
	// reported whenever it lands, so this test says the same thing either way.
	waitFor(t, func() bool { return paths.touched("git-origins") }, "the git route to be dialed")
	if report.Len() != 0 {
		t.Fatalf("wrote %q while attached, want nothing in the stream", report.String())
	}
	stop(&report)

	got := report.String()
	if !strings.Contains(got, "could not push primary") {
		t.Fatalf("report = %q, want the refused source named", got)
	}
	// The reason has to be the one the stub gave, not one stopping produced:
	// a report that cannot tell a refused push from a killed one would pass
	// just as happily if detaching started killing transfers again.
	if !strings.Contains(got, "not found") && !strings.Contains(got, "404") {
		t.Fatalf("report = %q, want the reason the push was refused", got)
	}
	for _, canceled := range []string{"signal: killed", "context canceled", "context deadline exceeded"} {
		if strings.Contains(got, canceled) {
			t.Fatalf("report = %q, want a refusal rather than %s: stopping must not interrupt a push", got, canceled)
		}
	}
}

// Stopping never produces a failure of its own. A transfer that has started
// finishes and is reported whatever it says (ADR 0095 §6 on automatic push);
// what a stop can cut short is the half that decides whether to send, and that
// half says nothing. Without the distinction, an ordinary detach prints a push
// failure nobody caused — and can leave the origin ahead of the lease that
// guards it.
func TestAutoPushWhileAttachedDoesNotReportItsOwnStop(t *testing.T) {
	dir, _ := pushRepo(t)
	ds, paths := pushDataSource(t, dir)

	stop := ds.app.autoPushWhileAttached(t.Context(), ds.client, "project-1", "sbx_1")
	// Stop as early as possible: while the first look is still resolving, or
	// mid-transfer. Resolving, there is nothing to say; mid-transfer, the push
	// finishes and its refusal is reported like any other. What must never
	// appear either way is a failure the stop itself caused.
	waitFor(t, func() bool { return len(paths.all()) > 0 }, "the first look to start")
	var report strings.Builder
	stop(&report)

	for _, canceled := range []string{"signal: killed", "context canceled", "context deadline exceeded"} {
		if strings.Contains(report.String(), canceled) {
			t.Fatalf("report = %q, want stopping to be silent about itself", report.String())
		}
	}
}

// A discobox this machine has nothing to push to is never asked, on the raw
// path as in the window: the whole rule lives in one place.
func TestAutoPushWhileAttachedSkipsADiscoboxItCannotPush(t *testing.T) {
	dir, _ := pushRepo(t)
	ds, paths := pushDataSource(t, dir)
	ds.app.pushCache = nil
	t.Setenv(hostid.EnvVar, "hst_othermachine0002")

	var report strings.Builder
	stop := ds.app.autoPushWhileAttached(t.Context(), ds.client, "project-1", "sbx_1")
	waitFor(t, func() bool { return len(paths.all()) > 0 }, "the discobox to be read")
	stop(&report)

	if paths.touched("git-origins") {
		t.Fatalf("requests = %v, want no push for another machine's discobox", paths.all())
	}
	if report.String() != "" {
		t.Fatalf("report = %q, want nothing said about a discobox this machine cannot push", report.String())
	}
}

// waitFor pumps until cond holds, so a test can wait on the loop's own
// goroutine without sleeping for a fixed time.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A refusal stands until something resolves it, not until the next beat. The
// beat after a failure sends nothing — the source is held — so a report built
// from the current look alone would forget the refusal five seconds after it
// happened, and only ever tell you about one that landed just before you
// detached.
func TestAutoPushWhileAttachedReportsARefusalManyBeatsLater(t *testing.T) {
	dir, _ := pushRepo(t)
	ds, paths := pushDataSource(t, dir)
	beatFast(t)

	stop := ds.app.autoPushWhileAttached(t.Context(), ds.client, "project-1", "sbx_1")
	waitFor(t, func() bool { return paths.touched("git-origins") }, "the push to be refused")

	// Let several beats pass. Each one resolves the branch, finds it held at
	// the commit that failed, and sends nothing — so the git route is dialed
	// exactly once however long this runs.
	dialed := len(paths.all())
	time.Sleep(20 * autoPushEvery)
	if got := len(paths.all()); got != dialed {
		t.Fatalf("requests grew from %d to %d, want a held refusal to cost nothing per beat", dialed, got)
	}

	var report strings.Builder
	stop(&report)
	if !strings.Contains(report.String(), "could not push primary") {
		t.Fatalf("report = %q, want the refusal still standing after many beats", report.String())
	}
}

// beatFast shortens the automatic push's beat for the length of a test, so a
// test about what happens across beats does not take a minute.
func beatFast(t *testing.T) {
	t.Helper()
	previous := autoPushEvery
	autoPushEvery = 5 * time.Millisecond
	t.Cleanup(func() { autoPushEvery = previous })
}

// A hold has to be released by the origin holding the commit, not only by the
// branch moving: `discobox push --force` answers a refusal by moving the lease,
// which leaves the tip exactly where the hold remembers it.
func TestPushSourcesReportsAHeldCommitTheOriginNowHolds(t *testing.T) {
	dir, commit := pushRepo(t)
	ds, paths := pushDataSource(t, dir)
	// What answering the refusal by hand leaves behind: the lease now names the
	// commit that was refused.
	pushGit(t, dir, "update-ref", sandboxgit.OriginLeaseRef("sbx_1", "primary", "main"), commit)

	pushes, err := ds.PushSources(t.Context(), "sbx_1", map[string]string{"primary": commit})
	if err != nil {
		t.Fatalf("look: %v", err)
	}
	if len(pushes) != 1 || !pushes[0].UpToDate || pushes[0].Err != nil {
		t.Fatalf("pushes = %#v, want the held commit reported as already in the origin", pushes)
	}
	if paths.touched("git-origins") {
		t.Fatalf("requests = %v, want nothing sent for a commit the origin already has", paths.all())
	}
}
