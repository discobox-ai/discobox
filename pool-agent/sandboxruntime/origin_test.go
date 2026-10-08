package sandboxruntime

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
)

// originTestRequest is a sandbox with a clone-delivered primary at local,
// checked out on a branch with a dirty workspace, and a push-delivered
// reference.
func originTestRequest(local string) *workerapimodel.PoolSandboxCreateRequest {
	return &workerapimodel.PoolSandboxCreateRequest{
		SandboxId: deliveryTestSandboxID,
		Config: workerapimodel.SandboxConfig{
			Source: workerclient.NewOptGitSource(workerapimodel.GitSource{
				Kind:           workerclient.GitSourceKindGit,
				Slug:           workerclient.NewOptString("primary"),
				LocalDirectory: workerclient.NewOptString(local),
				Checkout: workerclient.NewOptGitSourceCheckout(workerapimodel.GitSourceCheckout{
					RefType: workerclient.NewOptString("branch"),
					RefName: workerclient.NewOptString("feature"),
					Commit:  workerclient.NewOptString("0123456789abcdef0123456789abcdef01234567"),
				}),
				Workspace: workerclient.NewOptGitSourceWorkspace(workerapimodel.GitSourceWorkspace{
					Mode:        workerclient.NewOptGitSourceWorkspaceMode(workerclient.GitSourceWorkspaceModeDirty),
					BaseCommit:  workerclient.NewOptString("0123456789abcdef0123456789abcdef01234567"),
					SnapshotRef: workerclient.NewOptString("refs/discobox/run/run-1"),
				}),
			}),
			SourceCodeReferences: workerclient.NewOptSandboxConfigSourceCodeReferences(
				workerclient.SandboxConfigSourceCodeReferences{
					"/home/user/src/hooks": {
						Kind:     workerclient.GitSourceKindGit,
						Slug:     workerclient.NewOptString("hooks"),
						Delivery: workerclient.NewOptGitSourceDelivery(workerclient.GitSourceDeliveryPush),
					},
				}),
		},
	}
}

// makeGitDirectory gives dir a .git that passes for a repository.
func makeGitDirectory(t *testing.T, dir string) string {
	t.Helper()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return gitDir
}

// originTestRuntime is deliveryTestRuntime with the sandbox's tree in place,
// as provisioning has it by the time the live origins are recorded, and a host
// mount prefix the developer's repositories are found under.
func originTestRuntime(t *testing.T) *DockerSandboxRuntime {
	t.Helper()
	r := deliveryTestRuntime(t)
	r.hostMountPrefix = t.TempDir()
	if err := os.MkdirAll(r.sandboxRoot(deliveryTestSandboxID), 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

func makeBareOrigin(t *testing.T, r *DockerSandboxRuntime, slug string) string {
	t.Helper()
	origin := r.sandboxOriginPath(deliveryTestSandboxID, slug)
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return origin
}

// A clone-delivered source's origin route serves the developer's own Git
// directory, through this host's mount of it, with the refs the source
// declares; a push-delivered one serves its bare origin.
func TestTheOriginRouteServesALocalSourcesLiveRepository(t *testing.T) {
	requirePOSIXHost(t)
	r := originTestRuntime(t)
	gitDir := makeGitDirectory(t, filepath.Join(r.hostMountPrefix, "home", "dev", "project"))
	bare := makeBareOrigin(t, r, "hooks")

	if err := r.writeLiveOrigins(deliveryTestSandboxID, sandboxSources(linuxPaths, originTestRequest("/home/dev/project"))); err != nil {
		t.Fatal(err)
	}

	live, err := r.originLocation(deliveryTestSandboxID, "primary", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !live.Live || live.Path != gitDir {
		t.Fatalf("primary origin = %+v, want the live Git directory %s", live, gitDir)
	}
	if want := []string{"refs/discobox/run/run-1", "refs/heads/feature"}; !slices.Equal(live.Refs, want) {
		t.Fatalf("live origin refs = %v, want %v", live.Refs, want)
	}
	info, err := os.Stat(gitDir)
	if err != nil {
		t.Fatal(err)
	}
	if uid, gid := fileOwner(info); live.UID != uid || live.GID != gid {
		t.Fatalf("live origin runs as %d:%d, want the directory's owner %d:%d", live.UID, live.GID, uid, gid)
	}

	pushed, err := r.originLocation(deliveryTestSandboxID, "hooks", map[string]string{"DISCOBOX_USER_UID": "1000", "DISCOBOX_USER_GID": "1000"})
	if err != nil {
		t.Fatal(err)
	}
	if pushed.Live || pushed.Path != bare || pushed.UID != 1000 || pushed.GID != 1000 {
		t.Fatalf("hooks origin = %+v, want the bare origin %s as the sandbox user", pushed, bare)
	}
}

// A pool that cannot see the developer's repository serves the bare origin
// instead, and the sandbox is none the wiser; with neither there is no
// repository.
func TestTheOriginRouteFallsBackToTheBareOrigin(t *testing.T) {
	requirePOSIXHost(t)
	r := originTestRuntime(t)
	if err := r.writeLiveOrigins(deliveryTestSandboxID, sandboxSources(linuxPaths, originTestRequest("/home/dev/unseen"))); err != nil {
		t.Fatal(err)
	}

	if _, err := r.originLocation(deliveryTestSandboxID, "primary", nil); !errors.Is(err, ErrRepositoryNotFound) {
		t.Fatalf("an origin the pool can neither see nor holds = %v, want ErrRepositoryNotFound", err)
	}

	bare := makeBareOrigin(t, r, "primary")
	location, err := r.originLocation(deliveryTestSandboxID, "primary", nil)
	if err != nil {
		t.Fatal(err)
	}
	if location.Live || location.Path != bare {
		t.Fatalf("primary origin = %+v, want the bare origin %s", location, bare)
	}
}

// A .git that is no longer a real directory is not the repository a sandbox
// may read (ADR 0093), so it is not served as one.
func TestALiveOriginThatIsNoLongerADirectoryIsNotServed(t *testing.T) {
	requirePOSIXHost(t)
	r := originTestRuntime(t)
	project := filepath.Join(r.hostMountPrefix, "home", "dev", "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	makeGitDirectory(t, filepath.Join(r.hostMountPrefix, "elsewhere"))
	if err := os.Symlink(filepath.Join(r.hostMountPrefix, "elsewhere", ".git"), filepath.Join(project, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := r.writeLiveOrigins(deliveryTestSandboxID, sandboxSources(linuxPaths, originTestRequest("/home/dev/project"))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.originLocation(deliveryTestSandboxID, "primary", nil); !errors.Is(err, ErrRepositoryNotFound) {
		t.Fatalf("a symlinked .git = %v, want ErrRepositoryNotFound", err)
	}
}

// The record describes the sources the sandbox has now: a create without a
// local source leaves no live origin behind it.
func TestLiveOriginsAreRewrittenOnEveryCreate(t *testing.T) {
	requirePOSIXHost(t)
	r := originTestRuntime(t)
	if err := r.writeLiveOrigins(deliveryTestSandboxID, sandboxSources(linuxPaths, originTestRequest("/home/dev/project"))); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := r.readLiveOrigin(deliveryTestSandboxID, "primary"); err != nil || !ok {
		t.Fatalf("primary live origin recorded = %v, %v", ok, err)
	}
	if _, ok, _ := r.readLiveOrigin(deliveryTestSandboxID, "hooks"); ok {
		t.Fatal("a push-delivered source was recorded as a live origin")
	}

	if err := r.writeLiveOrigins(deliveryTestSandboxID, sandboxSources(linuxPaths, deliveryTestRequest())); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.sandboxRoot(deliveryTestSandboxID), liveOriginsFileName)); !os.IsNotExist(err) {
		t.Fatalf("a sandbox with no local source kept its live origins: %v", err)
	}
}
