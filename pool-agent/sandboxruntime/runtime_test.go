package sandboxruntime

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"

	"github.com/discobox-ai/discobox/layout"
	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/sandboxconfig"
	"github.com/discobox-ai/discobox/sandboxuser"
)

// What the request gave is forwarded verbatim. What it did not give stays
// unset -- including the home directory, which used to be guessed as
// /home/<name> here and then traveled inward looking like a resolved fact.
func TestSandboxUserForwardsWhatTheRequestGave(t *testing.T) {
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
	if idOf(user.UID) != 1000 || idOf(user.GID) != 1001 || user.Name != "sandbox" {
		t.Fatalf("resolveSandboxUser = %#v", user)
	}
	if user.HomeDirectory != "" {
		t.Fatalf("home = %q, want unset: the account's home lives in the image", user.HomeDirectory)
	}
	env := envWithSandboxUser(map[string]string{}, user)
	if env["DISCOBOX_USER_UID"] != "1000" || env["DISCOBOX_USER_GID"] != "1001" || env["DISCOBOX_USER_NAME"] != "sandbox" {
		t.Fatalf("envWithSandboxUser = %#v", env)
	}
	// Absent stays absent on the wire too, so boot can tell "not given" from a
	// value and resolve it against the account database itself.
	if _, ok := env["DISCOBOX_USER_HOME"]; ok {
		t.Fatalf("DISCOBOX_USER_HOME = %q, want unset", env["DISCOBOX_USER_HOME"])
	}
}

// An explicit home is forwarded: the request stating it outright is the one way
// the pool agent can know it.
func TestSandboxUserForwardsAnExplicitHome(t *testing.T) {
	user := resolveSandboxUser(&workerapimodel.PoolSandboxCreateRequest{
		Config: workerapimodel.SandboxConfig{
			User: workerclient.NewOptSandboxUser(workerapimodel.SandboxUser{
				Name:          workerclient.NewOptString("sandbox"),
				HomeDirectory: workerclient.NewOptString("/var/home/sandbox"),
			}),
		},
	})
	if user.HomeDirectory != "/var/home/sandbox" {
		t.Fatalf("home = %q, want the requested one", user.HomeDirectory)
	}
}

// A request that names no user leaves everything unset. The pool agent cannot
// resolve a sandbox's account, so it forwards nothing rather than inventing
// root -- absent must not become the most privileged identity available, and
// the image's own user stands instead (ADR 0025 §4, §5).
func TestSandboxUserWithNoUserRequestedIsLeftUnset(t *testing.T) {
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
			if user.UID != nil || user.GID != nil || user.Name != "" || user.HomeDirectory != "" {
				t.Fatalf("resolveSandboxUser = %#v, want everything unset", user)
			}
			// An absent variable is how boot tells "no user configured" from
			// "uid 0"; stamping 0 here is what made every such sandbox root.
			env := envWithSandboxUser(map[string]string{}, user)
			for _, key := range []string{"DISCOBOX_USER_UID", "DISCOBOX_USER_GID", "DISCOBOX_USER_NAME", "DISCOBOX_USER_HOME", "DISCOBOX_USER_GROUP"} {
				if _, ok := env[key]; ok {
					t.Fatalf("%s = %q, want unset", key, env[key])
				}
			}
		})
	}
}

// A bare name no longer becomes uid 1000: 1000 is one distro family's
// convention, and the account may have any id (ADR 0025 §4).
func TestSandboxUserNameAloneInventsNoIDs(t *testing.T) {
	user := resolveSandboxUser(&workerapimodel.PoolSandboxCreateRequest{
		Config: workerapimodel.SandboxConfig{
			User: workerclient.NewOptSandboxUser(workerapimodel.SandboxUser{
				Name: workerclient.NewOptString("dev"),
			}),
		},
	})
	if user.Name != "dev" {
		t.Fatalf("name = %q, want dev", user.Name)
	}
	if user.UID != nil || user.GID != nil {
		t.Fatalf("ids = %d/%d, want both unset", idOf(user.UID), idOf(user.GID))
	}
}

// A uid with no gid keeps the gid unset rather than copying the uid; the
// sandbox reads the account's real default group (ADR 0025 §6).
func TestSandboxUserUIDAloneLeavesTheGIDUnset(t *testing.T) {
	user := resolveSandboxUser(&workerapimodel.PoolSandboxCreateRequest{
		Config: workerapimodel.SandboxConfig{
			User: workerclient.NewOptSandboxUser(workerapimodel.SandboxUser{
				UID: workerclient.NewOptInt64(1000),
			}),
		},
	})
	if idOf(user.UID) != 1000 || user.GID != nil {
		t.Fatalf("resolveSandboxUser = %#v, want uid 1000 with an unset gid", user)
	}
	env := envWithSandboxUser(map[string]string{}, user)
	if env["DISCOBOX_USER_UID"] != "1000" {
		t.Fatalf("DISCOBOX_USER_UID = %q", env["DISCOBOX_USER_UID"])
	}
	if _, ok := env["DISCOBOX_USER_GID"]; ok {
		t.Fatalf("DISCOBOX_USER_GID = %q, want unset", env["DISCOBOX_USER_GID"])
	}
}

// An unknown id is omitted from the chown argument rather than guessed at.
func TestChownSpecOmitsUnsetIDs(t *testing.T) {
	for _, tc := range []struct {
		uid, gid int
		want     string
	}{
		{1000, 2000, "1000:2000"},
		{1000, -1, "1000"},
		{-1, 2000, ":2000"},
	} {
		if got := chownSpec(tc.uid, tc.gid); got != tc.want {
			t.Fatalf("chownSpec(%d,%d) = %q, want %q", tc.uid, tc.gid, got, tc.want)
		}
	}
}

func TestSandboxUserEnvPreservesExplicitHomeAndUser(t *testing.T) {
	user := sandboxuser.User{UID: sandboxuser.ID(1000), GID: sandboxuser.ID(1000), Name: "sandbox", HomeDirectory: "/home/sandbox"}
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
	doc := buildSandboxDocument("project-1", "sandbox-1", "pool-1", "public-key", "sha256:image", &workerapimodel.PoolSandboxCreateRequest{Config: config}, nil, nil)
	cfg, _ := sandboxconfig.Effective(doc)
	if len(cfg.Sources) != 1 || cfg.Sources[0].Target != "/workspace" {
		t.Fatalf("effective sources = %#v, want runtime bind root /workspace", cfg.Sources)
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
	requirePOSIXHost(t)
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
	requirePOSIXHost(t)
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
	requirePOSIXHost(t)
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

// Discobox state is addressed by its container path everywhere; only the mount
// source handed to the daemon is translated, and only when a driver relocated
// the state root.
func TestDockerSandboxRuntimeDaemonPathTranslatesOnlyRelocatedState(t *testing.T) {
	const containerPath = "/var/lib/discobox/projects/prj_default/sandboxes/sandbox-1/volumes/home"

	same := &DockerSandboxRuntime{hostState: layout.NewHostMapping("")}
	if got := same.daemonPath(containerPath); got != containerPath {
		t.Fatalf("daemon path = %q, want the container path unchanged", got)
	}

	relocated := &DockerSandboxRuntime{hostState: layout.NewHostMapping("/var/lib/docker/discobox")}
	want := "/var/lib/docker/discobox/projects/prj_default/sandboxes/sandbox-1/volumes/home"
	if got := relocated.daemonPath(containerPath); got != want {
		t.Fatalf("daemon path = %q, want %q", got, want)
	}

	// A path the user brought in is already a daemon path; it must not be
	// rewritten just because the state root moved.
	if got := relocated.daemonPath("/home/dev/src"); got != "/home/dev/src" {
		t.Fatalf("daemon path = %q, want foreign paths passed through", got)
	}
}

func TestDockerSandboxRuntimePoolCacheUsesIndependentRoot(t *testing.T) {
	runtime := &DockerSandboxRuntime{projectID: "proj_a", poolID: "pool_a"}

	got := runtime.poolCacheRoot()
	want := "/var/lib/discobox/cache/projects/proj_a/pools/pool_a/cache"
	if got != want {
		t.Fatalf("pool cache root = %q, want %q", got, want)
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

func TestBuildSandboxDocumentIncludesSelectedHarnessIdentityAndFiles(t *testing.T) {
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

	doc := buildSandboxDocument("project-1", "sandbox-1", "pool-1", "public-key", "sha256:image", req, nil, nil)
	cfg, _ := sandboxconfig.Effective(doc)
	if cfg.APIVersion != sandboxconfig.APIVersion || cfg.SandboxID != "sandbox-1" {
		t.Fatalf("effective identity = %#v, want v1 sandbox-1", cfg)
	}
	if cfg.Provider.Kind != "discobox-pool" || cfg.Provider.ProjectID != "project-1" || cfg.Provider.PoolID != "pool-1" {
		t.Fatalf("provider = %#v, want pool provider identity", cfg.Provider)
	}
	if cfg.Provider.PublicKeys["controlPlane"] != "public-key" {
		t.Fatalf("public keys = %#v, want control plane key", cfg.Provider.PublicKeys)
	}
	// The request named no user, so the manifest publishes none and the image's
	// own account stands (ADR 0025 §5). It used to publish root.
	if cfg.User.Name != "" || cfg.User.UID != nil || cfg.User.GID != nil || cfg.User.HomeDirectory != "" {
		t.Fatalf("effective user = %#v, want nothing published for an unrequested user", cfg.User)
	}
	if cfg.Env["BASE"] != "sandbox" || cfg.Env["OVERRIDE"] != "sandbox" {
		t.Fatalf("env = %#v, want sandbox env in effective config", cfg.Env)
	}
	if cfg.Harness.ID != "claude" || len(cfg.Files) != 1 {
		t.Fatalf("resolved harness = %#v, want claude with one file", cfg.Harness)
	}
}

func TestBuildSandboxDocumentOverlaysConfiguredFilesOntoRuntimeLayer(t *testing.T) {
	req := &workerapimodel.PoolSandboxCreateRequest{
		ResolvedHarnessConfig: workerclient.NewOptResolvedHarnessConfig(workerapimodel.ResolvedHarnessConfig{
			ID: "claude", Name: "Claude",
			Files: workerclient.NewOptNilHarnessConfigFileArray([]workerapimodel.HarnessConfigFile{
				{Path: ".claude/settings.json", Content: `{"theme":"dark"}`},
				{Path: ".claude.json", Content: `{}`},
			}),
			ConfiguredFiles: workerclient.NewOptNilHarnessConfigFileArray([]workerapimodel.HarnessConfigFile{
				// Overlays the image baseline's settings.json by path with what the
				// configure flow captured.
				{Path: ".claude/settings.json", Content: `{"theme":"light"}`},
			}),
		}),
	}

	doc := buildSandboxDocument("project-1", "sandbox-1", "pool-1", "public-key", "sha256:image", req, nil, nil)
	if len(doc.Image.Files) != 2 {
		t.Fatalf("image files = %+v, want the unmodified image baseline", doc.Image.Files)
	}
	if len(doc.Runtime.Files) != 1 || doc.Runtime.Files[0].Path != ".claude/settings.json" || doc.Runtime.Files[0].Content != `{"theme":"light"}` {
		t.Fatalf("runtime files = %+v, want configured settings.json alone", doc.Runtime.Files)
	}

	cfg, _ := sandboxconfig.Effective(doc)
	var settings, claudeJSON *sandboxconfig.File
	for i := range cfg.Files {
		switch cfg.Files[i].Path {
		case ".claude/settings.json":
			settings = &cfg.Files[i]
		case ".claude.json":
			claudeJSON = &cfg.Files[i]
		}
	}
	if settings == nil || settings.Content != `{"theme":"light"}` {
		t.Fatalf("effective settings.json = %+v, want the configured overlay to win", settings)
	}
	if claudeJSON == nil || claudeJSON.Content != `{}` {
		t.Fatalf("effective .claude.json = %+v, want the untouched image baseline", claudeJSON)
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
	// Windows has no POSIX permission bits: the perm argument maps only to the
	// read-only attribute, so Perm() reads back 0666 whatever was asked for.
	// The property here -- that a sandbox can read the manifest the agent wrote
	// -- is a POSIX one and cannot be expressed on Windows.
	if runtime.GOOS != "windows" {
		if got, want := info.Mode().Perm(), os.FileMode(0o644); got != want {
			t.Fatalf("sandbox manifest mode = %04o, want %04o", got, want)
		}
	}
}

// currentUser returns the identity of the test process itself, for exercising
// resolveSandboxUser-driven code paths without requiring the privilege a real
// cross-user switch would need (setting Credential to any uid this process
// doesn't already run as fails with EPERM unless the process is root).
func currentUser() sandboxuser.User {
	return sandboxuser.User{
		UID: sandboxuser.ID(int64(os.Getuid())),
		GID: sandboxuser.ID(int64(os.Getgid())),
	}
}

func TestRunGitWithSafeDirectoriesUsesTemporaryGlobalConfig(t *testing.T) {
	requirePOSIXHost(t)
	ctx := context.Background()
	repo := t.TempDir()
	git(t, repo, "init", "-b", "main")

	if err := runGitWithSafeDirectories(ctx, "", -1, -1, []string{repo}, "config", "--global", "--get-all", "safe.directory"); err != nil {
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
	git(t, repo, "-c", "user.Name=Test", "-c", "user.email=test@example.com", "commit", "-m", "one")
	pinned := gitOutput(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "README.md")
	git(t, repo, "-c", "user.Name=Test", "-c", "user.email=test@example.com", "commit", "-m", "two")
	latest := gitOutput(t, repo, "rev-parse", "HEAD")

	source := workerapimodel.GitSource{
		Kind: workerclient.GitSourceKindGit,
		Checkout: workerclient.NewOptGitSourceCheckout(workerapimodel.GitSourceCheckout{
			RefName: workerclient.NewOptString("main"),
			RefType: workerclient.NewOptString("branch"),
			Commit:  workerclient.NewOptString(pinned),
		}),
	}
	if err := checkoutGitSource(ctx, repo, source, -1, -1); err != nil {
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
	if err := checkoutGitSource(ctx, repo, source, -1, -1); err != nil {
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
	requirePOSIXHost(t)
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
	git(t, sourceRepo, "-c", "user.Name=Test", "-c", "user.email=test@example.com", "commit", "-m", "base")
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
	git(t, sourceRepo, "-c", "user.Name=Test", "-c", "user.email=test@example.com", "commit", "-m", "snapshot")
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
		if err := runtime.materializeGitSource(ctx, source, target, "", currentUser()); err != nil {
			t.Fatalf("materialize dirty git source attempt %d: %v", attempt, err)
		}
		// Both halves of a create, in the order a create runs them.
		if err := runtime.ensureOriginRemote(ctx, target, source, "primary", currentUser()); err != nil {
			t.Fatalf("ensure origin remote attempt %d: %v", attempt, err)
		}
		// origin must point at the in-sandbox path after every attempt, and
		// restoreGitWorkspace's fetch above must keep succeeding on attempt 2
		// despite attempt 1 already having pointed it there — the regression
		// case for the ordering hazard ADR 0020 fixes by fetching
		// dirty-workspace snapshots by explicit URL instead of via "origin".
		if got, want := gitOutput(t, target, "remote", "get-url", "origin"), "/.discobox/origins/primary"; got != want {
			t.Fatalf("origin remote after attempt %d = %q, want %q", attempt, got, want)
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

// A clone-delivered source is materialized once, like a push-delivered one. Any
// later create for the same sandbox — a re-pin, or a reconcile that re-drives
// create after a failure — reaches the same code with the workspace already
// live, and reset/clean/checkout there discards whatever the sandbox has done
// since: uncommitted edits, untracked files, and commits the branch points at.
func TestMaterializeGitSourceLeavesLiveCloneDeliveredWorkspaceAlone(t *testing.T) {
	requirePOSIXHost(t)
	ctx := context.Background()

	sourceRepo := t.TempDir()
	git(t, sourceRepo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(sourceRepo, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, sourceRepo, "add", "-A")
	git(t, sourceRepo, "-c", "user.Name=Test", "-c", "user.email=test@example.com", "commit", "-m", "base")
	baseCommit := gitOutput(t, sourceRepo, "rev-parse", "HEAD")

	source := workerapimodel.GitSource{
		Kind:           workerclient.GitSourceKindGit,
		LocalDirectory: workerclient.NewOptString(sourceRepo),
		Checkout: workerclient.NewOptGitSourceCheckout(workerapimodel.GitSourceCheckout{
			Commit:  workerclient.NewOptString(baseCommit),
			RefName: workerclient.NewOptString("main"),
			RefType: workerclient.NewOptString("branch"),
		}),
	}
	target := filepath.Join(t.TempDir(), "target")
	runtime := &DockerSandboxRuntime{}
	if err := runtime.materializeGitSource(ctx, source, target, "", currentUser()); err != nil {
		t.Fatalf("materialize clone source: %v", err)
	}
	if !gitSourceMaterialized(target) {
		t.Fatal("clone-delivered source was not marked materialized")
	}

	// Work the sandbox has done since the harness started: a commit of its own,
	// an uncommitted edit on top of it, and an untracked file.
	if err := os.WriteFile(filepath.Join(target, "feature.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, target, "add", "-A")
	git(t, target, "-c", "user.Name=Sandbox", "-c", "user.email=sandbox@example.com", "commit", "-m", "sandbox work")
	sandboxCommit := gitOutput(t, target, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(target, "README.md"), []byte("edited in the sandbox\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "scratch.txt"), []byte("in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The repeat create must be a no-op.
	if err := runtime.materializeGitSource(ctx, source, target, "", currentUser()); err != nil {
		t.Fatalf("materialize clone source again: %v", err)
	}

	if head := gitOutput(t, target, "rev-parse", "HEAD"); head != sandboxCommit {
		t.Fatalf("HEAD = %q, want the sandbox's own commit %q", head, sandboxCommit)
	}
	data, err := os.ReadFile(filepath.Join(target, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "edited in the sandbox\n" {
		t.Fatalf("README = %q, want the sandbox's uncommitted edit", data)
	}
	if _, err := os.Stat(filepath.Join(target, "scratch.txt")); err != nil {
		t.Fatalf("untracked file the sandbox created was cleaned away: %v", err)
	}
}

// A remote-URL source has no on-disk origin the pool host can bind, so its
// "origin" stays exactly as git clone set it: the real remote the sandbox
// fetches from itself.
func TestEnsureOriginRemoteLeavesARemoteURLSourceAlone(t *testing.T) {
	requirePOSIXHost(t)
	ctx := context.Background()
	sourceRepo := t.TempDir()
	git(t, sourceRepo, "init", "-b", "main")
	git(t, sourceRepo, "-c", "user.Name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "base")

	cloneURL := "file://" + sourceRepo
	source := workerapimodel.GitSource{
		Kind: workerclient.GitSourceKindGit,
		URL:  workerclient.NewOptURI(mustURL(t, cloneURL)),
	}
	target := filepath.Join(t.TempDir(), "target")
	runtime := &DockerSandboxRuntime{}
	if err := runtime.materializeGitSource(ctx, source, target, "", currentUser()); err != nil {
		t.Fatalf("materialize remote git source: %v", err)
	}
	if err := runtime.ensureOriginRemote(ctx, target, source, "primary", currentUser()); err != nil {
		t.Fatalf("ensure origin remote: %v", err)
	}
	if got := gitOutput(t, target, "remote", "get-url", "origin"); got != cloneURL {
		t.Fatalf("origin remote = %q, want unchanged clone URL %q", got, cloneURL)
	}
}

// A push-delivered source's origin is the repository the client pushed into,
// which the sandbox sees at /.discobox/origins/<slug> — the same path, and the
// same meaning, a clone-delivered source's live bind has (ADR 0058 §2).
func TestEnsureOriginRemotePointsAPushedSourceAtTheSandboxPath(t *testing.T) {
	requirePOSIXHost(t)
	ctx := context.Background()
	source := pushDeliveredSource("main")
	runtime := &DockerSandboxRuntime{}
	origin := filepath.Join(t.TempDir(), "primary.git")
	if err := runtime.initGitOrigin(ctx, origin, currentUser()); err != nil {
		t.Fatalf("init git origin: %v", err)
	}
	pushCommitToOrigin(t, origin, "main")

	target := filepath.Join(t.TempDir(), "target")
	if err := runtime.materializeGitSource(ctx, source, target, origin, currentUser()); err != nil {
		t.Fatalf("materialize push source: %v", err)
	}
	if err := runtime.ensureOriginRemote(ctx, target, source, "primary", currentUser()); err != nil {
		t.Fatalf("ensure origin remote: %v", err)
	}
	if got, want := gitOutput(t, target, "remote", "get-url", "origin"), "/.discobox/origins/primary"; got != want {
		t.Fatalf("origin = %q, want %q", got, want)
	}
}

// The case repair exists for: a checkout with no "origin" at all. Every sandbox
// delivered by push before this host had a pool-side origin is one — delivery
// pushed straight into the worktree, so git clone never ran and no remote was
// ever configured. Adding it back has to install the fetch refspec too, or
// origin/<branch> never resolves and there is nothing to rebase onto.
func TestEnsureOriginRemoteAddsAMissingRemote(t *testing.T) {
	requirePOSIXHost(t)
	ctx := context.Background()
	runtime := &DockerSandboxRuntime{}
	origin := filepath.Join(t.TempDir(), "primary.git")
	if err := runtime.initGitOrigin(ctx, origin, currentUser()); err != nil {
		t.Fatalf("init git origin: %v", err)
	}
	commit := pushCommitToOrigin(t, origin, "main")

	// The pre-ADR-0058 shape: a worktree repository that was pushed into, with
	// no remote of any kind.
	target := t.TempDir()
	git(t, target, "init", "-b", "main")
	if remotes := gitOutput(t, target, "remote"); remotes != "" {
		t.Fatalf("fixture already has remotes %q", remotes)
	}

	source := pushDeliveredSource("main")
	if err := runtime.ensureOriginRemote(ctx, target, source, "primary", currentUser()); err != nil {
		t.Fatalf("ensure origin remote: %v", err)
	}
	if got, want := gitOutput(t, target, "remote", "get-url", "origin"), "/.discobox/origins/primary"; got != want {
		t.Fatalf("origin = %q, want %q", got, want)
	}
	// The remote is only worth having if it fetches. The sandbox reaches the
	// repository through the read-only bind, which this test does not have, so
	// fetch the pool-side path that bind is of and check the configured refspec
	// puts the tip where a rebase would look for it.
	git(t, target, "fetch", origin, gitOutput(t, target, "config", "remote.origin.fetch"))
	if got := gitOutput(t, target, "rev-parse", "refs/remotes/origin/main"); got != commit {
		t.Fatalf("origin/main = %q, want the pushed commit %q", got, commit)
	}
}

// A remote pointing somewhere else — a stale path, or wherever someone working
// in the sandbox retargeted it — is corrected rather than left alone, and a
// second create leaves the corrected remote exactly as it is.
func TestEnsureOriginRemoteCorrectsAndThenLeavesTheRemote(t *testing.T) {
	requirePOSIXHost(t)
	ctx := context.Background()
	runtime := &DockerSandboxRuntime{}
	target := t.TempDir()
	git(t, target, "init", "-b", "main")
	git(t, target, "remote", "add", "origin", "/somewhere/else.git")

	source := pushDeliveredSource("main")
	for i := range 2 {
		if err := runtime.ensureOriginRemote(ctx, target, source, "primary", currentUser()); err != nil {
			t.Fatalf("ensure origin remote (call %d): %v", i+1, err)
		}
		if got, want := gitOutput(t, target, "remote", "get-url", "origin"), "/.discobox/origins/primary"; got != want {
			t.Fatalf("origin after call %d = %q, want %q", i+1, got, want)
		}
	}
	if got := gitOutput(t, target, "remote"); got != "origin" {
		t.Fatalf("remotes = %q, want only origin", got)
	}
}

// A URL alone is not a working remote. A remote assembled by hand — the shape a
// bare `git config remote.origin.url` write leaves behind — fetches into nothing,
// so origin/<branch> never resolves and there is still nothing to rebase onto.
func TestEnsureOriginRemoteRestoresAMissingFetchRefspec(t *testing.T) {
	requirePOSIXHost(t)
	ctx := context.Background()
	runtime := &DockerSandboxRuntime{}
	origin := filepath.Join(t.TempDir(), "primary.git")
	if err := runtime.initGitOrigin(ctx, origin, currentUser()); err != nil {
		t.Fatalf("init git origin: %v", err)
	}
	commit := pushCommitToOrigin(t, origin, "main")

	target := t.TempDir()
	git(t, target, "init", "-b", "main")
	git(t, target, "config", "remote.origin.url", "/.discobox/origins/primary")
	if got := gitOutput(t, target, "remote"); got != "origin" {
		t.Fatalf("fixture remotes = %q, want origin", got)
	}

	if err := runtime.ensureOriginRemote(ctx, target, pushDeliveredSource("main"), "primary", currentUser()); err != nil {
		t.Fatalf("ensure origin remote: %v", err)
	}
	git(t, target, "fetch", origin, gitOutput(t, target, "config", "remote.origin.fetch"))
	if got := gitOutput(t, target, "rev-parse", "refs/remotes/origin/main"); got != commit {
		t.Fatalf("origin/main = %q, want the pushed commit %q", got, commit)
	}
}

// A refspec other than the default is whoever works in the sandbox making a
// choice, so it survives a create rather than being normalized away.
func TestEnsureOriginRemoteKeepsACustomFetchRefspec(t *testing.T) {
	requirePOSIXHost(t)
	ctx := context.Background()
	runtime := &DockerSandboxRuntime{}
	target := t.TempDir()
	git(t, target, "init", "-b", "main")
	git(t, target, "remote", "add", "origin", "/somewhere/else.git")
	custom := "+refs/heads/main:refs/remotes/origin/main"
	git(t, target, "config", "--replace-all", "remote.origin.fetch", custom)

	if err := runtime.ensureOriginRemote(ctx, target, pushDeliveredSource("main"), "primary", currentUser()); err != nil {
		t.Fatalf("ensure origin remote: %v", err)
	}
	if got, want := gitOutput(t, target, "remote", "get-url", "origin"), "/.discobox/origins/primary"; got != want {
		t.Fatalf("origin = %q, want the corrected %q", got, want)
	}
	if got := gitOutput(t, target, "config", "--get-all", "remote.origin.fetch"); got != custom {
		t.Fatalf("fetch refspec = %q, want the custom %q left alone", got, custom)
	}
}

// A push-delivered source parks with no repository at all until the client's
// push lands. There is nothing to configure yet, and saying so is not a failure:
// the create that clones it is what gives it a remote to correct.
func TestEnsureOriginRemoteSkipsASourceThatIsNotThereYet(t *testing.T) {
	requirePOSIXHost(t)
	ctx := context.Background()
	runtime := &DockerSandboxRuntime{}
	target := filepath.Join(t.TempDir(), "not-cloned-yet")
	if err := runtime.ensureOriginRemote(ctx, target, pushDeliveredSource("main"), "primary", currentUser()); err != nil {
		t.Fatalf("ensure origin remote on a parked source: %v", err)
	}
}

// originMounts must add one read-only bind per source with an origin the sandbox
// can reach: the real host directory for a clone-delivered local source, the
// pool-side pushed repository for a push-delivered one, and nothing for a
// remote-URL source, whose origin is that remote (ADR 0026, ADR 0058 §2). This
// is the pure piece of prepareSandboxVolumes' origin-mount logic; it needs no
// real filesystem state or privilege to exercise directly.
func TestOriginMountsCoverEveryReachableOrigin(t *testing.T) {
	req := &workerapimodel.PoolSandboxCreateRequest{
		SandboxId: "sandbox-1",
		Config: workerapimodel.SandboxConfig{
			Source: workerclient.NewOptGitSource(workerapimodel.GitSource{
				Kind:           workerclient.GitSourceKindGit,
				LocalDirectory: workerclient.NewOptString("/host/primary"),
			}),
			SourceCodeReferences: workerclient.NewOptSandboxConfigSourceCodeReferences(workerclient.SandboxConfigSourceCodeReferences{
				"ref": {
					Kind:           workerclient.GitSourceKindGit,
					LocalDirectory: workerclient.NewOptString("/host/ref"),
				},
				// No LocalDirectory: the wire withholds it for a pushed
				// source, so an origin mount keyed on that field would skip
				// exactly the source that needs one.
				"pushed": {
					Kind:     workerclient.GitSourceKindGit,
					Delivery: workerclient.NewOptGitSourceDelivery(workerclient.GitSourceDeliveryPush),
				},
				"remote": {
					Kind: workerclient.GitSourceKindGit,
					URL:  workerclient.NewOptURI(mustURL(t, "https://example.com/remote.git")),
				},
			}),
		},
	}
	identity := func(p string) string { return p }
	originPath := func(slug string) string { return "/pool/origins/" + slug + ".git" }
	mounts := originMounts(sandboxSources(req), originPath, identity)

	byTarget := map[string]mount.Mount{}
	for _, m := range mounts {
		byTarget[m.Target] = m
	}
	if len(byTarget) != 3 {
		t.Fatalf("origin mounts = %#v, want exactly 3 (primary, ref and pushed)", byTarget)
	}
	primary, ok := byTarget[sandboxOriginsMount+"/primary"]
	if !ok || primary.Source != "/host/primary" || !primary.ReadOnly || primary.Type != mount.TypeBind {
		t.Fatalf("primary origin mount = %#v, origins = %#v", primary, byTarget)
	}
	ref, ok := byTarget[sandboxOriginsMount+"/ref"]
	if !ok || ref.Source != "/host/ref" || !ref.ReadOnly || ref.Type != mount.TypeBind {
		t.Fatalf("ref origin mount = %#v, origins = %#v", ref, byTarget)
	}
	// A push-delivered source's origin is the pool-side repository. The client
	// directory is unreachable from this host — which is why the source is
	// push-delivered — so the wire does not even carry it.
	pushed, ok := byTarget[sandboxOriginsMount+"/pushed"]
	if !ok || pushed.Source != "/pool/origins/pushed.git" || !pushed.ReadOnly || pushed.Type != mount.TypeBind {
		t.Fatalf("pushed origin mount = %#v, origins = %#v", pushed, byTarget)
	}
}

func TestSourceDataMountsUseStableKeysAndSandboxSlugs(t *testing.T) {
	primaryKey := strings.Repeat("a", 64)
	refKey := strings.Repeat("b", 64)
	req := &workerapimodel.PoolSandboxCreateRequest{
		SandboxId: "sandbox-1",
		Config: workerapimodel.SandboxConfig{
			Source: workerclient.NewOptGitSource(workerapimodel.GitSource{
				Kind: workerclient.GitSourceKindGit, DataKey: workerclient.NewOptString(primaryKey),
			}),
			SourceCodeReferences: workerclient.NewOptSandboxConfigSourceCodeReferences(workerclient.SandboxConfigSourceCodeReferences{
				"/workspace/lib": {
					Kind: workerclient.GitSourceKindGit,
					Slug: workerclient.NewOptString("library"), DataKey: workerclient.NewOptString(refKey),
				},
				"without-data": {Kind: workerclient.GitSourceKindGit},
			}),
		},
	}
	mounts := sourceDataMounts(
		sandboxSources(req),
		func(key string) string { return "/pool/data-per-source/" + key },
		func(host string) string { return "/daemon" + host },
	)

	if len(mounts) != 2 {
		t.Fatalf("source data mounts = %#v, want primary and library", mounts)
	}
	byTarget := map[string]mount.Mount{}
	for _, m := range mounts {
		byTarget[m.Target] = m
	}
	for target, key := range map[string]string{
		sandboxSourceDataMount + "/primary": primaryKey,
		sandboxSourceDataMount + "/library": refKey,
	} {
		got, ok := byTarget[target]
		if !ok || got.Type != mount.TypeBind || got.ReadOnly || got.Source != "/daemon/pool/data-per-source/"+key {
			t.Errorf("mount %q = %#v", target, got)
		}
	}
}

func TestValidSourceDataKey(t *testing.T) {
	for _, key := range []string{strings.Repeat("a", 64), strings.Repeat("09", 32)} {
		if !validSourceDataKey(key) {
			t.Errorf("validSourceDataKey(%q) = false", key)
		}
	}
	for _, key := range []string{"", strings.Repeat("a", 63), strings.Repeat("A", 64), strings.Repeat("g", 64), "../" + strings.Repeat("a", 61)} {
		if validSourceDataKey(key) {
			t.Errorf("validSourceDataKey(%q) = true", key)
		}
	}
}

// pushDeliveredSource is a push-delivered source checked out at a branch, or at a
// bare commit when the branch is empty.
//
// It carries no LocalDirectory and no URL, which is what the pool agent actually
// receives for a pushed source: poolGitSource withholds both so this host cannot
// try to reach a client filesystem it has no route to.
func pushDeliveredSource(branch string) workerapimodel.GitSource {
	source := workerapimodel.GitSource{
		Kind:     workerclient.GitSourceKindGit,
		Delivery: workerclient.NewOptGitSourceDelivery(workerclient.GitSourceDeliveryPush),
	}
	if branch != "" {
		source.Checkout = workerclient.NewOptGitSourceCheckout(workerapimodel.GitSourceCheckout{
			RefName: workerclient.NewOptString(branch),
			RefType: workerclient.NewOptString("branch"),
		})
	}
	return source
}

// pushCommitToOrigin does what the client does: commit in a repository of its
// own and push that branch into the source's origin repository.
func pushCommitToOrigin(t *testing.T, origin, branch string) string {
	t.Helper()
	client := t.TempDir()
	git(t, client, "init", "-b", branch)
	git(t, client, "config", "user.email", "test@example.com")
	git(t, client, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(client, "README.md"), []byte("pushed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, client, "add", "README.md")
	git(t, client, "commit", "-m", "pushed")
	git(t, client, "push", origin, branch)
	return gitOutput(t, client, "rev-parse", "HEAD")
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

// Provisioning creates the origin repository before any source exists: it is
// what the client pushes into while the sandbox parks (ADR 0058 §1). Bare,
// refusing deletes, and allowing the non-fast-forward a local rebase produces.
func TestInitGitOriginIsBareAndRefusesDeletes(t *testing.T) {
	requirePOSIXHost(t)
	ctx := context.Background()
	runtime := &DockerSandboxRuntime{}
	origin := filepath.Join(t.TempDir(), "primary.git")

	// Twice: create is retried, and the second call must leave the repository
	// exactly as it stands.
	for attempt := 1; attempt <= 2; attempt++ {
		if err := runtime.initGitOrigin(ctx, origin, currentUser()); err != nil {
			t.Fatalf("init git origin attempt %d: %v", attempt, err)
		}
	}
	if got := gitOutput(t, origin, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Fatalf("is-bare-repository = %q, want true", got)
	}
	if got := gitOutput(t, origin, "config", "--get", "receive.denyDeletes"); got != "true" {
		t.Fatalf("receive.denyDeletes = %q, want true", got)
	}
	if got := gitOutput(t, origin, "config", "--get", "receive.denyNonFastForwards"); got != "false" {
		t.Fatalf("receive.denyNonFastForwards = %q, want false", got)
	}

	// Re-initializing an origin that already holds pushed commits must never
	// discard them: the client's history lives here between pushes.
	pushed := pushCommitToOrigin(t, origin, "main")
	if err := runtime.initGitOrigin(ctx, origin, currentUser()); err != nil {
		t.Fatalf("init git origin after a push: %v", err)
	}
	if got := gitOutput(t, origin, "rev-parse", "refs/heads/main"); got != pushed {
		t.Fatalf("main = %q after re-init, want the pushed commit %q", got, pushed)
	}
}

// A source checked out at a bare commit or tag names no branch, so the branch its
// commits land on is the client's choice. The origin's HEAD follows what actually
// arrived, which is what makes origin/HEAD — the upstream ref such a source
// tracks — resolve once the sandbox clones, without the pool host having to know
// the name the client picked.
func TestMaterializeGitSourceHeadsTheOriginAtWhatTheClientPushed(t *testing.T) {
	requirePOSIXHost(t)
	ctx := context.Background()
	source := pushDeliveredSource("")
	runtime := &DockerSandboxRuntime{}
	origin := filepath.Join(t.TempDir(), "primary.git")
	if err := runtime.initGitOrigin(ctx, origin, currentUser()); err != nil {
		t.Fatalf("init git origin: %v", err)
	}
	// git init points HEAD at init.defaultBranch, which is not what the client
	// pushes — so "has anything been pushed" cannot be asked of HEAD either.
	pushed := pushCommitToOrigin(t, origin, "discobox-source")

	target := filepath.Join(t.TempDir(), "target")
	if err := runtime.materializeGitSource(ctx, source, target, origin, currentUser()); err != nil {
		t.Fatalf("materialize push source: %v", err)
	}
	if got, want := gitOutput(t, origin, "symbolic-ref", "HEAD"), "refs/heads/discobox-source"; got != want {
		t.Fatalf("origin HEAD = %q, want %q", got, want)
	}
	if got := gitOutput(t, target, "rev-parse", "refs/remotes/origin/HEAD"); got != pushed {
		t.Fatalf("origin/HEAD in the sandbox = %q, want the pushed commit %q", got, pushed)
	}
}

// Until the client pushes there is nothing to clone, so materializing is a
// no-op and the sandbox stays parked. The push completes it: the commit and its
// files land in the working tree by checkout, the way a clone-delivered source's
// do.
func TestMaterializeGitSourceWaitsForThePushThenClonesTheOrigin(t *testing.T) {
	requirePOSIXHost(t)
	ctx := context.Background()
	source := pushDeliveredSource("main")
	runtime := &DockerSandboxRuntime{}
	origin := filepath.Join(t.TempDir(), "primary.git")
	if err := runtime.initGitOrigin(ctx, origin, currentUser()); err != nil {
		t.Fatalf("init git origin: %v", err)
	}
	target := filepath.Join(t.TempDir(), "target")

	if err := runtime.materializeGitSource(ctx, source, target, origin, currentUser()); err != nil {
		t.Fatalf("materialize before the push: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); !os.IsNotExist(err) {
		t.Fatalf("a source was materialized before the client pushed (stat err = %v)", err)
	}

	pushed := pushCommitToOrigin(t, origin, "main")
	if err := runtime.materializeGitSource(ctx, source, target, origin, currentUser()); err != nil {
		t.Fatalf("materialize after the push: %v", err)
	}
	if head := gitOutput(t, target, "rev-parse", "HEAD"); head != pushed {
		t.Fatalf("HEAD = %q, want the pushed commit %q", head, pushed)
	}
	if branch := gitOutput(t, target, "branch", "--show-current"); branch != "main" {
		t.Fatalf("branch = %q, want main", branch)
	}
	data, err := os.ReadFile(filepath.Join(target, "README.md"))
	if err != nil {
		t.Fatalf("pushed file is not in the working tree: %v", err)
	}
	if string(data) != "pushed\n" {
		t.Fatalf("pushed file = %q, want the pushed content", data)
	}
	// The sandbox tracks the origin it will fetch from, so a later re-push is
	// something it can rebase onto.
	if got, want := gitOutput(t, target, "rev-parse", "refs/remotes/origin/main"), pushed; got != want {
		t.Fatalf("origin/main = %q, want %q", got, want)
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

	err := runtime.materializeGitSource(ctx, workerapimodel.GitSource{Kind: workerclient.GitSourceKindGit}, target, "", currentUser())
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
	requirePOSIXHost(t)
	ctx := context.Background()

	// The client's repository: a base commit plus uncommitted edits captured as
	// a snapshot commit, exactly as resolveLocalRunSource builds them.
	client := t.TempDir()
	git(t, client, "init", "-b", "main")
	git(t, client, "config", "user.email", "test@example.com")
	git(t, client, "config", "user.Name", "Test")
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

	// Provision: the origin repository the client pushes into.
	origin := filepath.Join(t.TempDir(), "primary.git")
	if err := runtime.initGitOrigin(ctx, origin, currentUser()); err != nil {
		t.Fatalf("init git origin: %v", err)
	}
	if err := runtime.materializeGitSource(ctx, source, target, origin, currentUser()); err != nil {
		t.Fatalf("materialize push source: %v", err)
	}

	// The client pushes its branch and the snapshot ref. The snapshot is not a
	// branch or a tag, so the clone below does not bring it: it is fetched
	// explicitly out of the origin, exactly as a clone-delivered source's is
	// fetched out of the client's directory.
	git(t, client, "push", origin, "main", "+"+snapshotRef+":"+snapshotRef)

	// Resume: the same call now clones, checks out and restores the workspace.
	if err := runtime.materializeGitSource(ctx, source, target, origin, currentUser()); err != nil {
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
	requirePOSIXHost(t)
	ctx := context.Background()
	const projectID = "project-1"
	const sandboxID = "sandbox-1"

	client := t.TempDir()
	git(t, client, "init", "-b", "main")
	git(t, client, "config", "user.email", "test@example.com")
	git(t, client, "config", "user.Name", "Test")
	if err := os.WriteFile(filepath.Join(client, "README.md"), []byte("pushed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, client, "add", "README.md")
	git(t, client, "commit", "-m", "pushed")
	pushed := gitOutput(t, client, "rev-parse", "HEAD")

	// The state tree is relocated under a writable root for this test.
	withTestRoot(t)
	runtime := &DockerSandboxRuntime{projectID: projectID}
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
	// The resolved sandbox user must be the test process's own identity: a real
	// cross-user Credential switch requires root, which the test process isn't.
	req := &workerapimodel.PoolSandboxCreateRequest{
		SandboxId: sandboxID,
		Config: workerapimodel.SandboxConfig{
			Source: workerclient.NewOptGitSource(source),
			User: workerclient.NewOptSandboxUser(workerapimodel.SandboxUser{
				UID: workerclient.NewOptInt64(int64(os.Getuid())),
				Gid: workerclient.NewOptInt64(int64(os.Getgid())),
			}),
		},
	}
	target := runtime.sandboxSourcePath(sandboxID, "primary")
	origin := runtime.sandboxOriginPath(sandboxID, "primary")

	// Provision creates the origin and parks; the client pushes into it.
	if err := runtime.initGitOrigin(ctx, origin, currentUser()); err != nil {
		t.Fatalf("init git origin: %v", err)
	}
	if err := runtime.materializeGitSource(ctx, source, target, origin, currentUser()); err != nil {
		t.Fatalf("materialize push source: %v", err)
	}
	git(t, client, "push", origin, "main")

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
	requirePOSIXHost(t)
	ctx := context.Background()
	const projectID = "project-1"
	const sandboxID = "sandbox-1"

	withTestRoot(t)
	runtime := &DockerSandboxRuntime{projectID: projectID}
	target := runtime.sandboxSourcePath(sandboxID, "primary")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, target, "init", "-b", "main")
	git(t, target, "config", "user.email", "test@example.com")
	git(t, target, "config", "user.Name", "Test")
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

// A push-delivered source is only supposed to be finalized once, between the
// client's push and the harness starting. A stray duplicate resume call must
// not reset/clean a workspace the sandbox has been using since finalization.
func TestMaterializePushedSourcesIsANoOpOnceFinalized(t *testing.T) {
	requirePOSIXHost(t)
	ctx := context.Background()
	const projectID = "project-1"
	const sandboxID = "sandbox-1"

	client := t.TempDir()
	git(t, client, "init", "-b", "main")
	git(t, client, "config", "user.email", "test@example.com")
	git(t, client, "config", "user.Name", "Test")
	if err := os.WriteFile(filepath.Join(client, "README.md"), []byte("pushed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, client, "add", "README.md")
	git(t, client, "commit", "-m", "pushed")
	pushed := gitOutput(t, client, "rev-parse", "HEAD")

	withTestRoot(t)
	runtime := &DockerSandboxRuntime{projectID: projectID}
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
			User: workerclient.NewOptSandboxUser(workerapimodel.SandboxUser{
				UID: workerclient.NewOptInt64(int64(os.Getuid())),
				Gid: workerclient.NewOptInt64(int64(os.Getgid())),
			}),
		},
	}
	target := runtime.sandboxSourcePath(sandboxID, "primary")
	origin := runtime.sandboxOriginPath(sandboxID, "primary")

	// Provision creates the origin and parks, then the client pushes into it.
	if err := runtime.initGitOrigin(ctx, origin, currentUser()); err != nil {
		t.Fatalf("init git origin: %v", err)
	}
	if err := runtime.materializeGitSource(ctx, source, target, origin, currentUser()); err != nil {
		t.Fatalf("materialize push source: %v", err)
	}
	git(t, client, "push", origin, "main")

	// The resume create finalizes the source.
	if err := runtime.materializePushedSources(ctx, sandboxID, req); err != nil {
		t.Fatalf("materialize pushed sources: %v", err)
	}
	if !gitSourceMaterialized(target) {
		t.Fatal("push-delivered source was not marked materialized after finalizing")
	}

	// Work the sandbox has done since the harness started.
	if err := os.WriteFile(filepath.Join(target, "scratch.txt"), []byte("in progress\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A stray duplicate resume call must leave the now-live workspace alone.
	if err := runtime.materializePushedSources(ctx, sandboxID, req); err != nil {
		t.Fatalf("materialize pushed sources again: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "scratch.txt")); err != nil {
		t.Fatalf("a finalized push-delivered source was re-materialized and lost in-progress work: %v", err)
	}
}

// The pin is the identity a sandbox runs, and the same comparison decides both
// whether to launch an image and whether an existing container must be replaced
// (ADR 0016 §6).
func TestImageMatchesPin(t *testing.T) {
	const pinned = "sha256:aaa"
	if !imageMatchesPin(pinned, pinned) {
		t.Fatal("the pinned image must match its own pin")
	}
	if imageMatchesPin("sha256:bbb", pinned) {
		t.Fatal("a rebuilt image under the same tag must not match the pin")
	}
	// An unpinned sandbox runs whatever its reference names; treating that as a
	// mismatch would replace containers no one asked to upgrade.
	if !imageMatchesPin("sha256:bbb", "") {
		t.Fatal("an empty pin must match any image")
	}
	if !imageMatchesPin("  sha256:aaa  ", "  sha256:aaa  ") {
		t.Fatal("surrounding whitespace must not defeat the comparison")
	}
}

// The image a sandbox actually ran is the first thing a version-skew
// investigation needs, and it is recorded as the resolved identity rather than
// the mutable reference it was asked for (ADR 0016).
func TestSandboxDocumentRecordsResolvedImageIdentity(t *testing.T) {
	req := &workerapimodel.PoolSandboxCreateRequest{SandboxId: "sandbox-1"}
	doc := buildSandboxDocument("project-1", "sandbox-1", "pool-1", "public-key", "sha256:resolved", req, nil, nil)
	if doc.Runtime.Image != "sha256:resolved" {
		t.Fatalf("runtime image = %q, want the resolved image identity", doc.Runtime.Image)
	}
	_, provenance := sandboxconfig.Effective(doc)
	if provenance.Runtime.Image != "sha256:resolved" {
		t.Fatalf("provenance image = %q, want the resolved image identity", provenance.Runtime.Image)
	}
}

// A caller who names a non-root user must not silently get root. uid 0 is
// load-bearing: boot skips additionalGroups for it, so this also cost the
// sandbox every group its image declared (e.g. "docker").
func TestResolveSandboxUserNamedUserIsNotRoot(t *testing.T) {
	req := &workerapimodel.PoolSandboxCreateRequest{
		Config: workerapimodel.SandboxConfig{
			User: workerclient.NewOptSandboxUser(workerapimodel.SandboxUser{
				Name: workerclient.NewOptString("dev"),
			}),
		},
	}
	got := resolveSandboxUser(req)
	if got.UID != nil || got.GID != nil {
		t.Fatalf("named user invented ids: uid=%d gid=%d", idOf(got.UID), idOf(got.GID))
	}
	if got.Name != "dev" {
		t.Fatalf("name = %q, want dev", got.Name)
	}
	// The home directory is no longer guessed from the name. /home/<name> is a
	// convention, and which one this account actually has is a fact that lives
	// in the image's own passwd file (ADR 0033 §5).
	if got.HomeDirectory != "" {
		t.Fatalf("home = %q, want unset: only the sandbox can answer that", got.HomeDirectory)
	}
}

// Nothing specified still means root, unchanged.
// An explicit uid 0 is honored: root is a legitimate choice, and distinct
// from omitting the field.
func TestResolveSandboxUserExplicitRootIsHonoured(t *testing.T) {
	req := &workerapimodel.PoolSandboxCreateRequest{
		Config: workerapimodel.SandboxConfig{
			User: workerclient.NewOptSandboxUser(workerapimodel.SandboxUser{
				Name: workerclient.NewOptString("root"),
				UID:  workerclient.NewOptInt64(0),
			}),
		},
	}
	if got := resolveSandboxUser(req); idOf(got.UID) != 0 {
		t.Fatalf("explicit root uid = %d, want 0", idOf(got.UID))
	}
}

// A recorded fingerprint is the whole answer when it is present.
func TestSpecDriftedComparesTheRecordedFingerprint(t *testing.T) {
	if specDrifted("fp-1", "sha256:old", "fp-1", "sha256:new") {
		t.Fatal("matching fingerprints reported as drift")
	}
	if !specDrifted("fp-1", "sha256:same", "fp-2", "sha256:same") {
		t.Fatal("changed fingerprint not reported as drift")
	}
}

// A container with no fingerprint label predates fingerprinting. It falls back
// to the image comparison rather than being declared clean, which is what left
// such sandboxes stranded on an old image with no way to upgrade off it.
func TestSpecDriftedFallsBackToTheImagePinWithoutALabel(t *testing.T) {
	if !specDrifted("", "sha256:old", "fp-1", "sha256:new") {
		t.Fatal("unlabeled container running an unpinned image not reported as drift")
	}
	if specDrifted("", "sha256:pinned", "fp-1", "sha256:pinned") {
		t.Fatal("unlabeled container already on the pinned image reported as drift")
	}
}

// An unpinned sandbox runs whatever its reference names, so an unlabeled
// container cannot drift: there is nothing to compare it against.
func TestSpecDriftedUnlabeledAndUnpinnedIsNotDrift(t *testing.T) {
	if specDrifted("", "sha256:whatever", "fp-1", "") {
		t.Fatal("unpinned sandbox reported as drift")
	}
}

// TestValidateCreateRequestRefusesAnUnresolvedRequest: the pool agent runs what
// the control plane tells it and invents nothing. It used to substitute a plain
// alpine image for a missing one — a container that can never host a sandbox
// agent, so the sandbox simply never answered instead of the request being
// reported as wrong (ADR 0025).
func TestValidateCreateRequestRefusesAnUnresolvedRequest(t *testing.T) {
	resolved := workerclient.NewOptResolvedHarnessConfig(workerapimodel.ResolvedHarnessConfig{ID: "harness-1", Name: "Shell"})

	for name, tc := range map[string]struct {
		req     *workerapimodel.PoolSandboxCreateRequest
		wantErr string
	}{
		"no image": {
			req: &workerapimodel.PoolSandboxCreateRequest{
				Config:                workerapimodel.SandboxConfig{},
				ResolvedHarnessConfig: resolved,
			},
			wantErr: "no image",
		},
		"no harness config": {
			req: &workerapimodel.PoolSandboxCreateRequest{
				Config: workerapimodel.SandboxConfig{Image: workerclient.NewOptString("discobox-sandbox-agent:local")},
			},
			wantErr: "no resolved harness config",
		},
		"fully resolved": {
			req: &workerapimodel.PoolSandboxCreateRequest{
				Config:                workerapimodel.SandboxConfig{Image: workerclient.NewOptString("discobox-sandbox-agent:local")},
				ResolvedHarnessConfig: resolved,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateCreateRequest("sbx_test0000000001", tc.req)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestSandboxHostnameDropsThePrefixAndStaysALegalLabel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sandboxID string
		want      string
	}{
		{name: "generated id", sandboxID: "sbx_dfzx0123456789ab", want: "dfzx0123456789ab"},
		{name: "no prefix", sandboxID: "bare0123456789ab", want: "bare0123456789ab"},
		{name: "illegal characters", sandboxID: "sbx_A b/c", want: "a-b-c"},
		{name: "trims edge hyphens", sandboxID: "sbx_-abc-", want: "abc"},
		{name: "nothing usable", sandboxID: "sbx_", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sandboxHostname(tc.sandboxID); got != tc.want {
				t.Fatalf("sandboxHostname(%q) = %q, want %q", tc.sandboxID, got, tc.want)
			}
		})
	}
}

// idOf renders an optional id for assertions, using -1 for absent so a failure
// prints the value rather than a pointer.
func idOf(v *int64) int64 {
	if v == nil {
		return -1
	}
	return *v
}
