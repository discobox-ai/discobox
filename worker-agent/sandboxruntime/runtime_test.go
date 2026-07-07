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
	workerclient "github.com/obot-platform/discobox/worker-agent/api/gen"
	workerapimodel "github.com/obot-platform/discobox/worker-agent/api/model"
)

func TestSandboxUserResolvesUIDGIDAndDefaults(t *testing.T) {
	req := &workerapimodel.WorkerSandboxCreateRequest{
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
	for name, req := range map[string]*workerapimodel.WorkerSandboxCreateRequest{
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
	uiURL := mustURL(t, "https://example.com/ui.git")
	req := &workerapimodel.WorkerSandboxCreateRequest{
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
				"workspace ui": {
					Kind: workerclient.GitSourceKindGit,
					Slug: workerclient.NewOptString("UI"),
					URL:  workerclient.NewOptURI(uiURL),
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
	assertSource(t, sources[2], "ui", "/workspace/ui")
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

func TestDockerSandboxRuntimeWorkerHostPathUsesHostMountPrefix(t *testing.T) {
	runtime := &DockerSandboxRuntime{hostMountPrefix: "/host"}

	got := runtime.workerHostPath("/var/lib/discobox/projects/prj_default/sandboxes/sandbox-1/volumes/home")
	want := "/host/var/lib/discobox/projects/prj_default/sandboxes/sandbox-1/volumes/home"
	if got != want {
		t.Fatalf("worker host path = %q, want %q", got, want)
	}
}

func TestDockerSandboxRuntimeWorkerHostPathPreservesHostPathWithoutPrefix(t *testing.T) {
	runtime := &DockerSandboxRuntime{}

	got := runtime.workerHostPath("/var/lib/discobox/projects/prj_default")
	if got != "/var/lib/discobox/projects/prj_default" {
		t.Fatalf("worker host path = %q, want host path", got)
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

func TestBuildSandboxManifestIncludesProjectAgentConfigs(t *testing.T) {
	req := &workerapimodel.WorkerSandboxCreateRequest{
		Config: workerapimodel.SandboxConfig{
			Env: workerclient.NewOptSandboxConfigEnv(workerclient.SandboxConfigEnv{
				"BASE":     "sandbox",
				"OVERRIDE": "sandbox",
			}),
		},
		ResolvedAgentConfig: workerclient.NewOptResolvedAgentConfig(workerapimodel.ResolvedAgentConfig{
			ID:             "claude",
			Name:           "Claude",
			InstallCommand: workerclient.NewOptNilStringArray([]string{"npm", "install", "-g", "@anthropic-ai/claude-code"}),
			RunCommand:     []string{"claude"},
		}),
		AgentConfigs: workerclient.NewOptNilSandboxAgentConfigArray([]workerapimodel.SandboxAgentConfig{
			{
				ID:             "codex",
				Name:           "Codex",
				InstallCommand: workerclient.NewOptNilStringArray([]string{"npm", "install", "-g", "@openai/codex"}),
				RunCommand:     []string{"codex"},
				IsDefault:      true,
			},
			{
				ID:         "claude",
				Name:       "Claude",
				RunCommand: []string{"claude"},
			},
		}),
	}

	manifest := buildSandboxManifest("project-1", "sandbox-1", "worker-1", "public-key", req)
	if manifest.APIVersion != "discobox.dev/sandbox/v1" || manifest.SandboxID != "sandbox-1" {
		t.Fatalf("manifest identity = %#v, want v1 sandbox-1", manifest)
	}
	if manifest.Provider == nil || manifest.Provider.Kind != "discobox-worker" || manifest.Provider.ProjectID != "project-1" || manifest.Provider.WorkerID != "worker-1" {
		t.Fatalf("provider = %#v, want worker provider identity", manifest.Provider)
	}
	if manifest.Provider.PublicKeys["controlPlane"] != "public-key" {
		t.Fatalf("public keys = %#v, want control plane key", manifest.Provider.PublicKeys)
	}
	env, ok := manifest.Config.Env.Get()
	if !ok || env["BASE"] != "sandbox" || env["OVERRIDE"] != "sandbox" {
		t.Fatalf("env = %#v, want sandbox env in manifest config", env)
	}
	if manifest.ResolvedAgentConfig == nil || manifest.ResolvedAgentConfig.ID != "claude" || len(manifest.ResolvedAgentConfig.RunCommand) != 1 || manifest.ResolvedAgentConfig.RunCommand[0] != "claude" {
		t.Fatalf("resolved agent config = %#v, want claude", manifest.ResolvedAgentConfig)
	}
	if len(manifest.AgentConfigs) != 2 {
		t.Fatalf("agent configs = %#v, want 2", manifest.AgentConfigs)
	}
	if !manifest.AgentConfigs[0].IsDefault || len(manifest.AgentConfigs[0].InstallCommand) == 0 || len(manifest.AgentConfigs[0].RunCommand) != 1 || manifest.AgentConfigs[0].RunCommand[0] != "codex" {
		t.Fatalf("default agent config = %#v, want default with install and run command", manifest.AgentConfigs[0])
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
