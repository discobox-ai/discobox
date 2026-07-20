package sandboxruntime

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	workerclient "github.com/obot-platform/discobox/pool-agent/api/gen"
	workerapimodel "github.com/obot-platform/discobox/pool-agent/api/model"
)

func TestSandboxUserResolvesUIDGIDAndDefaults(t *testing.T) {
	req := &workerapimodel.PoolSandboxCreateRequest{
		Config: workerapimodel.SandboxConfig{
			User: workerclient.NewOptSandboxUser(workerapimodel.SandboxUser{
				Name: workerclient.NewOptString("sandbox"),
				UID:  workerclient.NewOptInt64(1000),
				Gid:  workerclient.NewOptInt64(1001),
			}),
		},
	}
	user := resolveSandboxUser(req)
	if user.uid != 1000 || user.gid != 1001 || user.name != "sandbox" || user.homeDirectory != "/home/sandbox" {
		t.Fatalf("resolveSandboxUser = %#v", user)
	}
	env := envWithSandboxUser(map[string]string{}, user)
	if env["DISCOBOX_USER_UID"] != "1000" || env["DISCOBOX_USER_GID"] != "1001" || env["DISCOBOX_USER_NAME"] != "sandbox" || env["DISCOBOX_USER_HOME"] != "/home/sandbox" {
		t.Fatalf("envWithSandboxUser = %#v", env)
	}
}

func TestSandboxUserUsesReasonableDefaults(t *testing.T) {
	for name, req := range map[string]*workerapimodel.PoolSandboxCreateRequest{
		"nil": nil,
		"empty user": {
			Config: workerapimodel.SandboxConfig{
				User: workerclient.NewOptSandboxUser(workerapimodel.SandboxUser{}),
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			user := resolveSandboxUser(req)
			if user.uid != 0 || user.gid != 0 || user.name != "root" || user.homeDirectory != "/home/root" {
				t.Fatalf("resolveSandboxUser = %#v", user)
			}
			env := envWithSandboxUser(map[string]string{}, user)
			if env["DISCOBOX_USER_UID"] != "0" || env["DISCOBOX_USER_GID"] != "0" || env["DISCOBOX_USER_NAME"] != "root" || env["DISCOBOX_USER_HOME"] != "/home/root" {
				t.Fatalf("envWithSandboxUser = %#v", env)
			}
		})
	}
}

func TestSandboxUserEnvPreservesExplicitHomeAndUser(t *testing.T) {
	user := sandboxUserIdentity{uid: 1000, gid: 1000, name: "sandbox", homeDirectory: "/home/sandbox"}
	env := envWithSandboxUser(map[string]string{"HOME": "/custom", "USER": "custom"}, user)
	if env["HOME"] != "/custom" || env["USER"] != "custom" {
		t.Fatalf("env overrides = %#v", env)
	}
	if env["DISCOBOX_USER_HOME"] != "/home/sandbox" || env["DISCOBOX_USER_NAME"] != "sandbox" {
		t.Fatalf("discobox user env = %#v", env)
	}
}

func TestSandboxSourcesUseConfiguredAndDefaultTargets(t *testing.T) {
	primaryURL := mustURL(t, "https://example.com/primary.git")
	toolsURL := mustURL(t, "https://example.com/tools.git")
	docsURL := mustURL(t, "https://example.com/docs.git")
	req := &workerapimodel.PoolSandboxCreateRequest{
		SandboxId: "sandbox-1",
		Config: workerapimodel.SandboxConfig{
			Source: workerclient.NewOptGitSource(workerapimodel.GitSource{
				Kind: workerclient.GitSourceKindGit,
				Slug: workerclient.NewOptString("Primary Source"),
				URL:  workerclient.NewOptURI(primaryURL),
				Destination: workerclient.NewOptGitSourceDestination(workerapimodel.GitSourceDestination{
					Directory: workerclient.NewOptString("work/project"),
				}),
			}),
			SourceCodeReferences: workerclient.NewOptSandboxConfigSourceCodeReferences(workerclient.SandboxConfigSourceCodeReferences{
				"tools": {
					Kind: workerclient.GitSourceKindGit,
					URL:  workerclient.NewOptURI(toolsURL),
				},
				"workspace docs": {
					Kind: workerclient.GitSourceKindGit,
					Slug: workerclient.NewOptString("Docs"),
					URL:  workerclient.NewOptURI(docsURL),
				},
			}),
		},
	}
	sources := sandboxSources(req)
	if len(sources) != 3 {
		t.Fatalf("sources = %#v, want 3", sources)
	}
	assertSource(t, sources[0], "primary-source", "/work/project")
	assertSource(t, sources[1], "tools", "/tools")
	assertSource(t, sources[2], "docs", "/workspace/docs")
}

func TestNormalizeSandboxConfigPublishesPrimaryBindRoot(t *testing.T) {
	config := workerapimodel.SandboxConfig{
		Source: workerclient.NewOptGitSource(workerapimodel.GitSource{Kind: workerclient.GitSourceKindGit}),
	}
	normalizeSandboxConfig(&config)
	source, ok := config.Source.Get()
	if !ok {
		t.Fatal("normalized config lost primary source")
	}
	destination, ok := source.Destination.Get()
	if !ok || destination.Directory.Or("") != "/workspace" {
		t.Fatalf("destination = %#v, want default primary bind root /workspace", destination)
	}
	manifest := buildSandboxManifest("project-1", "sandbox-1", "pool-1", "public-key", &workerapimodel.PoolSandboxCreateRequest{Config: config}, nil)
	manifestSource, ok := manifest.Config.Source.Get()
	if !ok {
		t.Fatal("manifest lost normalized primary source")
	}
	manifestDestination, ok := manifestSource.Destination.Get()
	if !ok || manifestDestination.Directory.Or("") != "/workspace" {
		t.Fatalf("manifest destination = %#v, want runtime bind root /workspace", manifestDestination)
	}

	destination.Directory = workerclient.NewOptString("workspace/../project")
	source.Destination = workerclient.NewOptGitSourceDestination(destination)
	config.Source = workerclient.NewOptGitSource(source)
	normalizeSandboxConfig(&config)
	source, _ = config.Source.Get()
	destination, _ = source.Destination.Get()
	if destination.Directory.Or("") != "/project" {
		t.Fatalf("destination = %#v, want cleaned effective bind root /project", destination)
	}
}

func TestGitSourceCloneURLPrefixesAbsoluteLocalDirectoryWithHostMountPrefix(t *testing.T) {
	source := workerapimodel.GitSource{
		Kind:           workerclient.GitSourceKindGit,
		LocalDirectory: workerclient.NewOptString("/home/darren/project"),
	}
	cloneURL, err := gitSourceCloneURL(source, "/host")
	if err != nil {
		t.Fatalf("git source clone URL: %v", err)
	}
	if cloneURL != "/host/home/darren/project" {
		t.Fatalf("clone URL = %q, want host-mounted path", cloneURL)
	}
}

func TestGitSourceCloneURLDoesNotDoublePrefixHostMountedLocalDirectory(t *testing.T) {
	source := workerapimodel.GitSource{
		Kind:           workerclient.GitSourceKindGit,
		LocalDirectory: workerclient.NewOptString("/host/home/darren/project"),
	}
	cloneURL, err := gitSourceCloneURL(source, "/host")
	if err != nil {
		t.Fatalf("git source clone URL: %v", err)
	}
	if cloneURL != "/host/home/darren/project" {
		t.Fatalf("clone URL = %q, want original host-mounted path", cloneURL)
	}
}

func TestGitSourceCloneURLPreservesLocalDirectoryWithoutHostMountPrefix(t *testing.T) {
	source := workerapimodel.GitSource{
		Kind:           workerclient.GitSourceKindGit,
		LocalDirectory: workerclient.NewOptString("/home/darren/project"),
	}
	cloneURL, err := gitSourceCloneURL(source, "")
	if err != nil {
		t.Fatalf("git source clone URL: %v", err)
	}
	if cloneURL != "/home/darren/project" {
		t.Fatalf("clone URL = %q, want original local directory", cloneURL)
	}
}

func TestGitSafeDirectoriesTrustsHostMountPrefixAndChildren(t *testing.T) {
	dirs := gitSafeDirectories("/host/home/darren/src/disco2", "/host")
	want := []string{
		"/host",
		"/host/*",
	}
	if strings.Join(dirs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("safe directories = %#v, want %#v", dirs, want)
	}
}

func TestGitSafeDirectoriesTrustsWorktreeAndDotGitWithoutHostMountPrefix(t *testing.T) {
	dirs := gitSafeDirectories("/home/darren/src/disco2", "")
	want := []string{
		"/home/darren/src/disco2",
		"/home/darren/src/disco2/.git",
	}
	if strings.Join(dirs, "\n") != strings.Join(want, "\n") {
		t.Fatalf("safe directories = %#v, want %#v", dirs, want)
	}
}

func TestGitSafeDirectoriesIgnoresRemoteAndRelativeURLs(t *testing.T) {
	for _, cloneURL := range []string{"https://example.com/repo.git", "file:///host/repo", "../repo"} {
		if dirs := gitSafeDirectories(cloneURL, "/host"); len(dirs) != 0 {
			t.Fatalf("safe directories for %q = %#v, want none", cloneURL, dirs)
		}
	}
}

func TestDockerSandboxRuntimePoolHostPathUsesHostMountPrefix(t *testing.T) {
	runtime := &DockerSandboxRuntime{hostMountPrefix: "/host"}

	got := runtime.workerHostPath("/var/lib/discobox/projects/prj_default/sandboxes/sandbox-1/volumes/home")
	want := "/host/var/lib/discobox/projects/prj_default/sandboxes/sandbox-1/volumes/home"
	if got != want {
		t.Fatalf("pool host path = %q, want %q", got, want)
	}
}

func TestDockerSandboxRuntimePoolHostPathPreservesHostPathWithoutPrefix(t *testing.T) {
	runtime := &DockerSandboxRuntime{}

	got := runtime.workerHostPath("/var/lib/discobox/projects/prj_default")
	if got != "/var/lib/discobox/projects/prj_default" {
		t.Fatalf("pool host path = %q, want host path", got)
	}
}

func TestSandboxAgentTerminalStateErrorStopsOnExitedSandbox(t *testing.T) {
	err := sandboxAgentTerminalStateError(&Sandbox{
		SandboxID: "sandbox-1",
		Status:    StatusStopped,
		Error:     "container exited with status \"exited\" and exit code 127; last logs: /bin/sh: bad: not found",
	})
	if err == nil {
		t.Fatal("terminal state error = nil, want error")
	}
	if !strings.Contains(err.Error(), "before sandbox-agent became healthy") {
		t.Fatalf("terminal state error = %q, want sandbox-agent context", err)
	}
	if !strings.Contains(err.Error(), "exit code 127") || !strings.Contains(err.Error(), "/bin/sh: bad: not found") {
		t.Fatalf("terminal state error = %q, want container failure detail", err)
	}
}

func TestSandboxAgentTerminalStateErrorAllowsRunningSandbox(t *testing.T) {
	if err := sandboxAgentTerminalStateError(&Sandbox{SandboxID: "sandbox-1", Status: StatusRunning}); err != nil {
		t.Fatalf("terminal state error = %v, want nil", err)
	}
}

func TestDockerSandboxExitErrorIncludesExitCodeStateErrorAndLogs(t *testing.T) {
	message := dockerSandboxExitError(container.InspectResponse{
		State: &container.State{
			Status:   "exited",
			ExitCode: 127,
			Error:    "exec failed",
		},
	}, "line one | line two")

	for _, want := range []string{
		`status "exited"`,
		"exit code 127",
		"state error: exec failed",
		"last logs: line one | line two",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("exit error = %q, want %q", message, want)
		}
	}
}

func TestCompactLogTailTrimsBlankLinesAndJoins(t *testing.T) {
	got := compactLogTail("\n first line \n\nsecond line\n")
	if got != "first line | second line" {
		t.Fatalf("compact log tail = %q", got)
	}
}

func TestBuildSandboxManifestIncludesSelectedHarnessIdentityAndFiles(t *testing.T) {
	req := &workerapimodel.PoolSandboxCreateRequest{
		Config: workerapimodel.SandboxConfig{
			Env: workerclient.NewOptSandboxConfigEnv(workerclient.SandboxConfigEnv{
				"BASE":     "sandbox",
				"OVERRIDE": "sandbox",
			}),
		},
		ResolvedHarnessConfig: workerclient.NewOptResolvedHarnessConfig(workerapimodel.ResolvedHarnessConfig{
			ID: "claude", Name: "Claude",
			Files: workerclient.NewOptNilHarnessConfigFileArray([]workerapimodel.HarnessConfigFile{
				{Path: ".claude.json", Content: `{}`},
			}),
		}),
	}

	manifest := buildSandboxManifest("project-1", "sandbox-1", "pool-1", "public-key", req, nil)
	if manifest.APIVersion != "discobox.dev/sandbox/v1" || manifest.SandboxID != "sandbox-1" {
		t.Fatalf("manifest identity = %#v, want v1 sandbox-1", manifest)
	}
	if manifest.Provider == nil || manifest.Provider.Kind != "discobox-pool" || manifest.Provider.ProjectID != "project-1" || manifest.Provider.PoolId != "pool-1" {
		t.Fatalf("provider = %#v, want pool provider identity", manifest.Provider)
	}
	if manifest.Provider.PublicKeys["controlPlane"] != "public-key" {
		t.Fatalf("public keys = %#v, want control plane key", manifest.Provider.PublicKeys)
	}
	user, ok := manifest.Config.User.Get()
	if !ok || user.Name.Or("") != "root" || user.UID.Or(-1) != 0 || user.Gid.Or(-1) != 0 || user.HomeDirectory.Or("") != "/home/root" {
		t.Fatalf("manifest user = %#v, want resolved root identity at /home/root", user)
	}
	env, ok := manifest.Config.Env.Get()
	if !ok || env["BASE"] != "sandbox" || env["OVERRIDE"] != "sandbox" {
		t.Fatalf("env = %#v, want sandbox env in manifest config", env)
	}
	if manifest.ResolvedHarnessConfig == nil || manifest.ResolvedHarnessConfig.ID != "claude" || len(manifest.ResolvedHarnessConfig.Files) != 1 {
		t.Fatalf("resolved agent config = %#v, want claude", manifest.ResolvedHarnessConfig)
	}
}

func TestWriteSandboxManifestIsWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sandbox.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeSandboxManifest(path, []byte("new")); err != nil {
		t.Fatalf("write sandbox manifest: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o644); got != want {
		t.Fatalf("sandbox manifest mode = %04o, want %04o", got, want)
	}
}

func TestRunGitWithSafeDirectoriesUsesTemporaryGlobalConfig(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	git(t, repo, "init", "-b", "main")

	if err := runGitWithSafeDirectories(ctx, "", []string{repo}, "config", "--global", "--get-all", "safe.directory"); err != nil {
		t.Fatalf("run git with safe directories: %v", err)
	}
}

func TestCheckoutGitSourceChecksOutBranchAtPinnedCommit(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	git(t, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "README.md")
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "one")
	pinned := gitOutput(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "README.md")
	git(t, repo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "two")
	latest := gitOutput(t, repo, "rev-parse", "HEAD")

	source := workerapimodel.GitSource{
		Kind: workerclient.GitSourceKindGit,
		Checkout: workerclient.NewOptGitSourceCheckout(workerapimodel.GitSourceCheckout{
			RefName: workerclient.NewOptString("main"),
			RefType: workerclient.NewOptString("branch"),
			Commit:  workerclient.NewOptString(pinned),
		}),
	}
	if err := checkoutGitSource(ctx, repo, source); err != nil {
		t.Fatalf("checkout git source: %v", err)
	}
	if branch := gitOutput(t, repo, "branch", "--show-current"); branch != "main" {
		t.Fatalf("branch = %q, want main", branch)
	}
	if head := gitOutput(t, repo, "rev-parse", "HEAD"); head != pinned {
		t.Fatalf("HEAD = %q, want pinned commit %q", head, pinned)
	}
	git(t, repo, "checkout", "main")
	git(t, repo, "reset", "--hard", latest)
	if err := checkoutGitSource(ctx, repo, source); err != nil {
		t.Fatalf("checkout git source again: %v", err)
	}
	if branch := gitOutput(t, repo, "branch", "--show-current"); branch != "main" {
		t.Fatalf("branch after second checkout = %q, want main", branch)
	}
	if head := gitOutput(t, repo, "rev-parse", "HEAD"); head != pinned {
		t.Fatalf("HEAD after second checkout = %q, want pinned commit %q", head, pinned)
	}
}

func TestMaterializeGitSourceRestoresDirtySnapshotAsUnstagedChanges(t *testing.T) {
	ctx := context.Background()
	sourceRepo := t.TempDir()
	git(t, sourceRepo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(sourceRepo, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRepo, "deleted.txt"), []byte("delete me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, sourceRepo, "add", "README.md", "deleted.txt")
	git(t, sourceRepo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "base")
	baseCommit := gitOutput(t, sourceRepo, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(sourceRepo, "README.md"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(sourceRepo, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceRepo, "added.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, sourceRepo, "add", "-A")
	git(t, sourceRepo, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "snapshot")
	snapshotCommit := gitOutput(t, sourceRepo, "rev-parse", "HEAD")
	snapshotRef := "refs/discobox/run/snap_test"
	git(t, sourceRepo, "update-ref", snapshotRef, snapshotCommit)
	git(t, sourceRepo, "reset", "--hard", baseCommit)

	source := workerapimodel.GitSource{
		Kind:           workerclient.GitSourceKindGit,
		LocalDirectory: workerclient.NewOptString(sourceRepo),
		Checkout: workerclient.NewOptGitSourceCheckout(workerapimodel.GitSourceCheckout{
			Commit:  workerclient.NewOptString(baseCommit),
			RefName: workerclient.NewOptString("main"),
			RefType: workerclient.NewOptString("branch"),
		}),
		Workspace: workerclient.NewOptGitSourceWorkspace(workerapimodel.GitSourceWorkspace{
			Mode:        workerclient.NewOptGitSourceWorkspaceMode(workerclient.GitSourceWorkspaceModeDirty),
			BaseCommit:  workerclient.NewOptString(baseCommit),
			SnapshotRef: workerclient.NewOptString(snapshotRef),
		}),
	}
	target := filepath.Join(t.TempDir(), "target")
	runtime := &DockerSandboxRuntime{}
	for attempt := 1; attempt <= 2; attempt++ {
		if err := runtime.materializeGitSource(ctx, source, target); err != nil {
			t.Fatalf("materialize dirty git source attempt %d: %v", attempt, err)
		}
		if branch := gitOutput(t, target, "branch", "--show-current"); branch != "main" {
			t.Fatalf("branch after attempt %d = %q, want main", attempt, branch)
		}
		if head := gitOutput(t, target, "rev-parse", "HEAD"); head != baseCommit {
			t.Fatalf("HEAD after attempt %d = %q, want base commit %q", attempt, head, baseCommit)
		}
		if staged := gitOutput(t, target, "diff", "--cached", "--name-only"); staged != "" {
			t.Fatalf("staged paths after attempt %d = %q, want none", attempt, staged)
		}
		status := gitOutput(t, target, "status", "--porcelain=v1", "--untracked-files=all")
		for _, want := range []string{"README.md", " D deleted.txt", "?? added.txt"} {
			if !strings.Contains(status, want) {
				t.Fatalf("status after attempt %d = %q, want %q", attempt, status, want)
			}
		}
		data, err := os.ReadFile(filepath.Join(target, "README.md"))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "dirty\n" {
			t.Fatalf("README after attempt %d = %q, want dirty content", attempt, data)
		}
	}
}

func mustURL(t *testing.T, raw string) url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return *parsed
}

func assertSource(t *testing.T, source sandboxSource, slug, target string) {
	t.Helper()
	if source.slug != slug || source.target != target {
		t.Fatalf("source = %#v, want slug %q target %q", source, slug, target)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out))
}

// A push-delivered source must end up holding a real repository, because git
// http-backend only serves a repository that already exists.
func TestMaterializeGitSourceWithPushDeliveryInitializesRepository(t *testing.T) {
	ctx := context.Background()
	source := workerapimodel.GitSource{
		Kind:     workerclient.GitSourceKindGit,
		Delivery: workerclient.NewOptGitSourceDelivery(workerclient.GitSourceDeliveryPush),
		Checkout: workerclient.NewOptGitSourceCheckout(workerapimodel.GitSourceCheckout{
			RefName: workerclient.NewOptString("main"),
			RefType: workerclient.NewOptString("branch"),
		}),
	}
	target := filepath.Join(t.TempDir(), "target")
	runtime := &DockerSandboxRuntime{}

	// Materializing twice must be safe: create is retried.
	for attempt := 1; attempt <= 2; attempt++ {
		if err := runtime.materializeGitSource(ctx, source, target); err != nil {
			t.Fatalf("materialize push source attempt %d: %v", attempt, err)
		}
		if _, err := os.Stat(filepath.Join(target, ".git")); err != nil {
			t.Fatalf("attempt %d: repository was not initialized: %v", attempt, err)
		}
		// The pushed branch must be the checked-out one, or
		// receive.denyCurrentBranch=updateInstead leaves the working tree empty.
		if branch := gitOutput(t, target, "branch", "--show-current"); branch != "main" {
			t.Fatalf("initial branch after attempt %d = %q, want main", attempt, branch)
		}
	}
}

// The end state the push path depends on: a client pushing into the initialized
// repository lands its commit *and* its files in the sandbox's working tree.
func TestPushIntoInitializedSourceUpdatesWorkingTree(t *testing.T) {
	ctx := context.Background()
	source := workerapimodel.GitSource{
		Kind:     workerclient.GitSourceKindGit,
		Delivery: workerclient.NewOptGitSourceDelivery(workerclient.GitSourceDeliveryPush),
		Checkout: workerclient.NewOptGitSourceCheckout(workerapimodel.GitSourceCheckout{
			RefName: workerclient.NewOptString("main"),
			RefType: workerclient.NewOptString("branch"),
		}),
	}
	target := filepath.Join(t.TempDir(), "target")
	runtime := &DockerSandboxRuntime{}
	if err := runtime.materializeGitSource(ctx, source, target); err != nil {
		t.Fatalf("materialize push source: %v", err)
	}
	// git http-backend serves the repository with these settings; apply them
	// here so this exercises the same receive behavior a real push gets.
	git(t, target, "config", "http.receivepack", "true")
	git(t, target, "config", "receive.denyCurrentBranch", "updateInstead")

	client := t.TempDir()
	git(t, client, "init", "-b", "main")
	git(t, client, "config", "user.email", "test@example.com")
	git(t, client, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(client, "README.md"), []byte("pushed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, client, "add", "README.md")
	git(t, client, "commit", "-m", "pushed")
	pushed := gitOutput(t, client, "rev-parse", "HEAD")
	git(t, client, "push", target, "main")

	if head := gitOutput(t, target, "rev-parse", "HEAD"); head != pushed {
		t.Fatalf("sandbox HEAD = %q, want pushed commit %q", head, pushed)
	}
	data, err := os.ReadFile(filepath.Join(target, "README.md"))
	if err != nil {
		t.Fatalf("pushed file is not in the working tree: %v", err)
	}
	if string(data) != "pushed\n" {
		t.Fatalf("pushed file = %q, want pushed content", data)
	}
}

// Delivery is stated, not inferred. A clone-delivered source with nothing to
// clone from is a malformed request and must fail, rather than silently
// producing a sandbox with an empty workspace that waits for a push no client
// will send.
func TestMaterializeGitSourceWithoutDeliveryOrCloneSourceFails(t *testing.T) {
	ctx := context.Background()
	runtime := &DockerSandboxRuntime{}
	target := filepath.Join(t.TempDir(), "target")

	err := runtime.materializeGitSource(ctx, workerapimodel.GitSource{Kind: workerclient.GitSourceKindGit}, target)
	if err == nil {
		t.Fatal("materialize source with no url, no localDirectory, and no push delivery: got nil error, want failure")
	}
	if _, statErr := os.Stat(filepath.Join(target, ".git")); statErr == nil {
		t.Fatal("a malformed source initialized a repository; absence must not imply push delivery")
	}
}

// A dirty workspace must survive push delivery. Its semantics are the base
// commit checked out with uncommitted changes on top, so dropping the workspace
// on the push path would silently bring the sandbox up clean at the base commit
// and lose the client's edits.
func TestMaterializeGitSourceWithPushDeliveryRestoresDirtyWorkspace(t *testing.T) {
	ctx := context.Background()

	// The client's repository: a base commit plus uncommitted edits captured as
	// a snapshot commit, exactly as resolveLocalRunSource builds them.
	client := t.TempDir()
	git(t, client, "init", "-b", "main")
	git(t, client, "config", "user.email", "test@example.com")
	git(t, client, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(client, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, client, "add", "README.md")
	git(t, client, "commit", "-m", "base")
	baseCommit := gitOutput(t, client, "rev-parse", "HEAD")

	if err := os.WriteFile(filepath.Join(client, "README.md"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, client, "add", "README.md")
	git(t, client, "commit", "-m", "snapshot")
	snapshotCommit := gitOutput(t, client, "rev-parse", "HEAD")
	const snapshotRef = "refs/discobox/run/snap_test"
	git(t, client, "update-ref", snapshotRef, snapshotCommit)
	// The client's own branch stays at the base commit; the snapshot is a ref.
	git(t, client, "reset", "--hard", baseCommit)

	source := workerapimodel.GitSource{
		Kind:     workerclient.GitSourceKindGit,
		Delivery: workerclient.NewOptGitSourceDelivery(workerclient.GitSourceDeliveryPush),
		Checkout: workerclient.NewOptGitSourceCheckout(workerapimodel.GitSourceCheckout{
			Commit:  workerclient.NewOptString(baseCommit),
			RefName: workerclient.NewOptString("main"),
			RefType: workerclient.NewOptString("branch"),
		}),
		Workspace: workerclient.NewOptGitSourceWorkspace(workerapimodel.GitSourceWorkspace{
			Mode:        workerclient.NewOptGitSourceWorkspaceMode(workerclient.GitSourceWorkspaceModeDirty),
			BaseCommit:  workerclient.NewOptString(baseCommit),
			SnapshotRef: workerclient.NewOptString(snapshotRef),
		}),
	}

	target := filepath.Join(t.TempDir(), "target")
	runtime := &DockerSandboxRuntime{}

	// Provision: an empty repository parked for the push.
	if err := runtime.materializeGitSource(ctx, source, target); err != nil {
		t.Fatalf("materialize push source: %v", err)
	}
	git(t, target, "config", "receive.denyCurrentBranch", "updateInstead")

	// The client pushes its branch and the snapshot ref.
	git(t, client, "push", target, "main", "+"+snapshotRef+":"+snapshotRef)

	// Resume: the same call now finishes the checkout and restores the workspace.
	if err := runtime.materializeGitSource(ctx, source, target); err != nil {
		t.Fatalf("materialize after push: %v", err)
	}

	if head := gitOutput(t, target, "rev-parse", "HEAD"); head != baseCommit {
		t.Fatalf("HEAD = %q, want the base commit %q", head, baseCommit)
	}
	data, err := os.ReadFile(filepath.Join(target, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	// The edit must be present and uncommitted, not folded into a commit.
	if string(data) != "dirty\n" {
		t.Fatalf("README = %q, want the client's uncommitted edit", data)
	}
	if status := gitOutput(t, target, "status", "--porcelain=v1"); !strings.Contains(status, "README.md") {
		t.Fatalf("status = %q, want README.md modified but uncommitted", status)
	}
}

// A push-delivered source is materialized only after the client pushes, which
// happens after the container exists and parked. The create that resumes the
// sandbox must therefore finish the source, not short-circuit on the existing
// container and leave the workspace empty.
func TestMaterializePushedSourcesCompletesExistingSandbox(t *testing.T) {
	ctx := context.Background()
	const projectID = "project-1"
	const sandboxID = "sandbox-1"

	client := t.TempDir()
	git(t, client, "init", "-b", "main")
	git(t, client, "config", "user.email", "test@example.com")
	git(t, client, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(client, "README.md"), []byte("pushed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, client, "add", "README.md")
	git(t, client, "commit", "-m", "pushed")
	pushed := gitOutput(t, client, "rev-parse", "HEAD")

	// hostMountPrefix relocates the sandbox volume tree under a writable root.
	runtime := &DockerSandboxRuntime{projectID: projectID, hostMountPrefix: t.TempDir()}
	source := workerapimodel.GitSource{
		Kind:     workerclient.GitSourceKindGit,
		Delivery: workerclient.NewOptGitSourceDelivery(workerclient.GitSourceDeliveryPush),
		Slug:     workerclient.NewOptString("primary"),
		Checkout: workerclient.NewOptGitSourceCheckout(workerapimodel.GitSourceCheckout{
			Commit:  workerclient.NewOptString(pushed),
			RefName: workerclient.NewOptString("main"),
			RefType: workerclient.NewOptString("branch"),
		}),
	}
	req := &workerapimodel.PoolSandboxCreateRequest{
		SandboxId: sandboxID,
		Config: workerapimodel.SandboxConfig{
			Source: workerclient.NewOptGitSource(source),
		},
	}
	target := runtime.workerHostPath(runtime.sandboxSourcePath(sandboxID, "primary"))

	// Provision parks an empty repository.
	if err := runtime.materializeGitSource(ctx, source, target); err != nil {
		t.Fatalf("materialize push source: %v", err)
	}
	git(t, target, "config", "receive.denyCurrentBranch", "updateInstead")
	git(t, client, "push", target, "main")

	// The resume create.
	if err := runtime.materializePushedSources(ctx, sandboxID, req); err != nil {
		t.Fatalf("materialize pushed sources: %v", err)
	}
	if head := gitOutput(t, target, "rev-parse", "HEAD"); head != pushed {
		t.Fatalf("HEAD = %q, want the pushed commit %q", head, pushed)
	}
	data, err := os.ReadFile(filepath.Join(target, "README.md"))
	if err != nil {
		t.Fatalf("pushed file is not in the workspace: %v", err)
	}
	if string(data) != "pushed\n" {
		t.Fatalf("README = %q, want the pushed content", data)
	}
}

// A clone-delivered source was fully materialized at create. Re-running it on a
// repeat create would reset and clean a workspace the sandbox has been using.
func TestMaterializePushedSourcesLeavesCloneDeliveredSourcesAlone(t *testing.T) {
	ctx := context.Background()
	const projectID = "project-1"
	const sandboxID = "sandbox-1"

	runtime := &DockerSandboxRuntime{projectID: projectID, hostMountPrefix: t.TempDir()}
	target := runtime.workerHostPath(runtime.sandboxSourcePath(sandboxID, "primary"))
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, target, "init", "-b", "main")
	git(t, target, "config", "user.email", "test@example.com")
	git(t, target, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, target, "add", "README.md")
	git(t, target, "commit", "-m", "committed")
	// Work the sandbox has done since it started.
	if err := os.WriteFile(filepath.Join(target, "scratch.txt"), []byte("in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := &workerapimodel.PoolSandboxCreateRequest{
		SandboxId: sandboxID,
		Config: workerapimodel.SandboxConfig{
			Source: workerclient.NewOptGitSource(workerapimodel.GitSource{
				Kind:           workerclient.GitSourceKindGit,
				Delivery:       workerclient.NewOptGitSourceDelivery(workerclient.GitSourceDeliveryClone),
				Slug:           workerclient.NewOptString("primary"),
				LocalDirectory: workerclient.NewOptString("/does/not/exist"),
			}),
		},
	}
	if err := runtime.materializePushedSources(ctx, sandboxID, req); err != nil {
		t.Fatalf("materialize pushed sources: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "scratch.txt")); err != nil {
		t.Fatalf("a clone-delivered source was re-materialized and lost in-progress work: %v", err)
	}
}
