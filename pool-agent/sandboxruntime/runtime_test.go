package sandboxruntime

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/platform"
	workerclient "github.com/discobox-ai/discobox/pool-agent/api/gen"
	workerapimodel "github.com/discobox-ai/discobox/pool-agent/api/model"
	"github.com/discobox-ai/discobox/sandboxconfig"
	"github.com/discobox-ai/discobox/sandboxpath"
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
	user := resolveSandboxUser(linuxPaths, req)
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
	user := resolveSandboxUser(linuxPaths, &workerapimodel.PoolSandboxCreateRequest{
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
			user := resolveSandboxUser(linuxPaths, req)
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
	user := resolveSandboxUser(linuxPaths, &workerapimodel.PoolSandboxCreateRequest{
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
	user := resolveSandboxUser(linuxPaths, &workerapimodel.PoolSandboxCreateRequest{
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
	sources := sandboxSources(linuxPaths, req)
	if len(sources) != 3 {
		t.Fatalf("sources = %#v, want 3", sources)
	}
	assertSource(t, sources[0], "primary-source", "/work/project")
	assertSource(t, sources[1], "tools", "/tools")
	assertSource(t, sources[2], "docs", "/workspace/docs")
}

// A pool's sandboxes are of the platform it hosts, and every path the pool
// names in one — a source's target, the working root it writes into the
// manifest, the directory the sandbox starts in — is judged that platform's
// way (ADR 0145 §6). On Windows a path names its drive, and a source with none
// lands under the working root by name. A working directory that names no
// place in the sandbox fails the create, as the container runtime always made
// it fail; one that does is passed on as asked.
func TestSandboxPathsAreThePoolPlatforms(t *testing.T) {
	windows := sandboxpath.For(platform.Platform{OS: "windows", Arch: "amd64"})
	darwin := sandboxpath.For(platform.Platform{OS: "darwin", Arch: "arm64"})
	toolsURL := mustURL(t, "https://example.com/tools.git")
	request := func(directory, workingDirectory string) *workerapimodel.PoolSandboxCreateRequest {
		return &workerapimodel.PoolSandboxCreateRequest{
			SandboxId: "sandbox-1",
			Config: workerapimodel.SandboxConfig{
				Source: workerclient.NewOptGitSource(workerapimodel.GitSource{
					Kind: workerclient.GitSourceKindGit,
					URL:  workerclient.NewOptURI(toolsURL),
					Destination: workerclient.NewOptGitSourceDestination(workerapimodel.GitSourceDestination{
						Directory:        workerclient.NewOptString(directory),
						WorkingDirectory: workerclient.NewOptString(workingDirectory),
					}),
				}),
				SourceCodeReferences: workerclient.NewOptSandboxConfigSourceCodeReferences(workerclient.SandboxConfigSourceCodeReferences{
					"tools": {Kind: workerclient.GitSourceKindGit, URL: workerclient.NewOptURI(toolsURL)},
				}),
			},
		}
	}
	for _, tc := range []struct {
		name        string
		paths       sandboxpath.Paths
		directory   string
		workingDir  string
		wantPrimary string
		wantTools   string
		wantWorkDir string
		wantRefused bool
		wantRoot    string
	}{
		{
			name: "windows drive paths", paths: windows,
			directory: "C:/src/app", workingDir: `C:\src\app\cmd`,
			wantPrimary: `C:\src\app`, wantTools: `C:\workspace\tools`, wantWorkDir: `C:\src\app\cmd`, wantRoot: `C:\workspace`,
		},
		{
			name: "windows POSIX paths name no place", paths: windows,
			directory: "/workspace/app", workingDir: "/workspace/app",
			wantPrimary: `C:\workspace`, wantTools: `C:\workspace\tools`, wantRefused: true, wantRoot: `C:\workspace`,
		},
		{
			name: "darwin", paths: darwin,
			directory: "/Users/ada/app/", workingDir: "/Users/ada/app/./cmd",
			wantPrimary: "/Users/ada/app", wantTools: "/tools", wantWorkDir: "/Users/ada/app/./cmd", wantRoot: "/workspace",
		},
		{
			name: "linux relative working directory", paths: linuxPaths,
			directory: "/workspace/app", workingDir: "app",
			wantPrimary: "/workspace/app", wantTools: "/tools", wantRefused: true, wantRoot: "/workspace",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := request(tc.directory, tc.workingDir)
			sources := sandboxSources(tc.paths, req)
			if len(sources) != 2 {
				t.Fatalf("sources = %#v, want 2", sources)
			}
			if sources[0].target != tc.wantPrimary || sources[1].target != tc.wantTools {
				t.Fatalf("targets = %q, %q; want %q, %q", sources[0].target, sources[1].target, tc.wantPrimary, tc.wantTools)
			}
			got, err := sourceWorkingDirectory(tc.paths, req)
			if tc.wantRefused {
				if err == nil {
					t.Fatalf("working directory = %q, want it refused", got)
				}
			} else if err != nil || got != tc.wantWorkDir {
				t.Fatalf("working directory = %q, %v; want %q", got, err, tc.wantWorkDir)
			}
			doc := buildSandboxDocument(tc.paths, "project-1", "sandbox-1", "pool-1", "public-key", "", "sha256:image", req, nil, nil)
			if doc.Runtime.AgentRuntime.WorkingRoot != tc.wantRoot {
				t.Fatalf("manifest working root = %q, want %q", doc.Runtime.AgentRuntime.WorkingRoot, tc.wantRoot)
			}
		})
	}
}

func TestNormalizeSandboxConfigPublishesPrimaryBindRoot(t *testing.T) {
	config := workerapimodel.SandboxConfig{
		Source: workerclient.NewOptGitSource(workerapimodel.GitSource{Kind: workerclient.GitSourceKindGit}),
	}
	normalizeSandboxConfig(linuxPaths, &config)
	source, ok := config.Source.Get()
	if !ok {
		t.Fatal("normalized config lost primary source")
	}
	destination, ok := source.Destination.Get()
	if !ok || destination.Directory.Or("") != "/workspace" {
		t.Fatalf("destination = %#v, want default primary bind root /workspace", destination)
	}
	doc := buildSandboxDocument(linuxPaths, "project-1", "sandbox-1", "pool-1", "public-key", "", "sha256:image", &workerapimodel.PoolSandboxCreateRequest{Config: config}, nil, nil)
	cfg, _ := sandboxconfig.Effective(doc)
	if len(cfg.Sources) != 1 || cfg.Sources[0].Target != "/workspace" {
		t.Fatalf("effective sources = %#v, want runtime bind root /workspace", cfg.Sources)
	}

	destination.Directory = workerclient.NewOptString("workspace/../project")
	source.Destination = workerclient.NewOptGitSourceDestination(destination)
	config.Source = workerclient.NewOptGitSource(source)
	normalizeSandboxConfig(linuxPaths, &config)
	source, _ = config.Source.Get()
	destination, _ = source.Destination.Get()
	if destination.Directory.Or("") != "/project" {
		t.Fatalf("destination = %#v, want cleaned effective bind root /project", destination)
	}
}

// The origin a sandbox is bound to is the repository's own Git directory and
// nothing else: a .git that is a file (a linked worktree or submodule checkout)
// or a symlink would make the bind something other than that, and a symlink
// can point at the working tree whose ignored files ADR 0093 exists to keep out.
func TestCheckLocalGitDirectoryRefusesAnythingButARealDirectory(t *testing.T) {
	requirePOSIXHost(t)
	runtime := &DockerSandboxRuntime{}
	source := func(dir string) workerapimodel.GitSource {
		return workerapimodel.GitSource{
			Kind:           workerclient.GitSourceKindGit,
			LocalDirectory: workerclient.NewOptString(dir),
		}
	}

	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runtime.checkLocalGitDirectory(source(repo)); err != nil {
		t.Fatalf("check a repository's own Git directory: %v", err)
	}

	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+filepath.Join(repo, ".git", "worktrees", "w")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runtime.checkLocalGitDirectory(source(worktree)); err == nil {
		t.Fatal("a .git file was accepted as a Git directory to bind")
	}

	linked := t.TempDir()
	if err := os.Symlink(linked, filepath.Join(linked, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := runtime.checkLocalGitDirectory(source(linked)); err == nil {
		t.Fatal("a .git symlink to the working tree was accepted as a Git directory to bind")
	}

	if err := runtime.checkLocalGitDirectory(source(t.TempDir())); err == nil {
		t.Fatal("a directory with no .git was accepted as a Git directory to bind")
	}

	pushed := source(worktree)
	pushed.Delivery = workerclient.NewOptGitSourceDelivery(workerclient.GitSourceDeliveryPush)
	if err := runtime.checkLocalGitDirectory(pushed); err != nil {
		t.Fatalf("check a push-delivered source, whose origin is pool-side: %v", err)
	}
}

// Discobox state is addressed by its container path everywhere; only the mount
// source handed to the daemon is translated, and only when a driver relocated
// the state root.
func TestDockerSandboxRuntimeDaemonPathTranslatesOnlyRelocatedState(t *testing.T) {
	const containerPath = "/var/lib/discobox/projects/prj_default/sandboxes/sandbox-1/volumes/home"

	same := &DockerSandboxRuntime{}
	if got := same.daemonPath(containerPath); got != containerPath {
		t.Fatalf("daemon path = %q, want the container path unchanged", got)
	}

	relocated := &DockerSandboxRuntime{hostStateRoot: "/var/lib/docker/discobox"}
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

// The far end of the same journey: what the agent writes into sandbox.json is
// what boot reads to decide whether a cache path is partitioned (ADR 0094 cache partition). The
// scope has to survive this rebuild, and an unset one has to stay unset so
// harness.ResolveVolumes applies the default rather than this hop inventing one.
func TestDocumentVolumesKeepTheScope(t *testing.T) {
	volumes := documentVolumes([]workerapimodel.HarnessVolume{
		{
			Path:   "/nix",
			Volume: "cache",
			Scope:  workerclient.NewOptHarnessVolumeScope(workerclient.HarnessVolumeScopeShared),
			UID:    workerclient.NewOptString("0"),
		},
		{Path: "/home/darren/.cache", Volume: "cache", UID: workerclient.NewOptString("1000")},
	})
	if len(volumes) != 2 {
		t.Fatalf("volumes = %d, want 2", len(volumes))
	}
	if volumes[0].Scope != harness.VolumeScopeShared {
		t.Fatalf("/nix scope = %q, want %q", volumes[0].Scope, harness.VolumeScopeShared)
	}
	if volumes[1].Scope != "" {
		t.Fatalf("undeclared scope = %q, want it left unset for ResolveVolumes to default", volumes[1].Scope)
	}
}

// sandbox.json is where the export mode reads which data paths stay behind
// (ADR 0129 §2), so the flag has to survive this rebuild too.
func TestDocumentVolumesKeepExcludeFromExport(t *testing.T) {
	volumes := documentVolumes([]workerapimodel.HarnessVolume{
		{Path: "/var/lib/docker", Volume: "data", ExcludeFromExport: workerclient.NewOptBool(true)},
		{Path: "/home/darren", Volume: "data"},
	})
	if !volumes[0].ExcludeFromExport {
		t.Fatal("/var/lib/docker lost excludeFromExport in sandbox.json")
	}
	if volumes[1].ExcludeFromExport {
		t.Fatal("an undeclared path was excluded; absent must mean it travels")
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

	doc := buildSandboxDocument(linuxPaths, "project-1", "sandbox-1", "pool-1", "public-key", "", "sha256:image", req, nil, nil)
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

	doc := buildSandboxDocument(linuxPaths, "project-1", "sandbox-1", "pool-1", "public-key", "", "sha256:image", req, nil, nil)
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

// Every sandbox has primary source data at the fixed name harnesses read. A
// primary with a key shares it; one the control plane could give no key, and a
// sandbox with no primary at all, keep a private one instead of coming up
// without the mount. References are mounted under their own slugs, by key.
func TestSourceDataPlan(t *testing.T) {
	primaryKey := strings.Repeat("a", 64)
	refKey := strings.Repeat("b", 64)
	keyed := func(key string) workerapimodel.GitSource {
		return workerapimodel.GitSource{Kind: workerclient.GitSourceKindGit, DataKey: workerclient.NewOptString(key)}
	}
	slugged := func(source workerapimodel.GitSource, slug string) workerapimodel.GitSource {
		source.Slug = workerclient.NewOptString(slug)
		return source
	}
	unkeyed := workerapimodel.GitSource{Kind: workerclient.GitSourceKindGit}
	for _, tc := range []struct {
		name    string
		primary *workerapimodel.GitSource
		refs    workerclient.SandboxConfigSourceCodeReferences
		want    []sourceData
	}{
		{
			name:    "a keyed primary and references share by key under their slugs",
			primary: new(keyed(primaryKey)),
			refs: workerclient.SandboxConfigSourceCodeReferences{
				"/workspace/lib": slugged(keyed(refKey), "library"),
				"without-data":   unkeyed,
			},
			want: []sourceData{{slug: "primary", key: primaryKey}, {slug: "library", key: refKey}},
		},
		{
			name:    "an unkeyed primary keeps its own",
			primary: &unkeyed,
			want:    []sourceData{{slug: "primary"}},
		},
		{
			name:    "a primary with its own slug is still mounted where harnesses read it",
			primary: new(slugged(keyed(primaryKey), "app")),
			want:    []sourceData{{slug: "primary", key: primaryKey}},
		},
		{
			name:    "an unkeyed primary with its own slug keeps its own where harnesses read it",
			primary: new(slugged(unkeyed, "app")),
			want:    []sourceData{{slug: "primary"}},
		},
		{name: "no source is a private source", want: []sourceData{{slug: "primary"}}},
		{
			name: "references alone still leave the primary private",
			refs: workerclient.SandboxConfigSourceCodeReferences{"lib": keyed(refKey)},
			want: []sourceData{{slug: "lib", key: refKey}, {slug: "primary"}},
		},
		{
			name: "a reference that took the primary's name before it was reserved keeps it",
			refs: workerclient.SandboxConfigSourceCodeReferences{"primary": keyed(refKey)},
			want: []sourceData{{slug: "primary", key: refKey}},
		},
		{
			name:    "so does the primary's data keep the primary's own slug",
			primary: new(slugged(keyed(primaryKey), "app")),
			refs:    workerclient.SandboxConfigSourceCodeReferences{"primary": keyed(refKey)},
			want:    []sourceData{{slug: "app", key: primaryKey}, {slug: "primary", key: refKey}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &workerapimodel.PoolSandboxCreateRequest{SandboxId: "sandbox-1"}
			if tc.primary != nil {
				req.Config.Source = workerclient.NewOptGitSource(*tc.primary)
			}
			if tc.refs != nil {
				req.Config.SourceCodeReferences = workerclient.NewOptSandboxConfigSourceCodeReferences(tc.refs)
			}
			if got := sourceDataPlan(sandboxSources(linuxPaths, req), tc.primary != nil); !slices.Equal(got, tc.want) {
				t.Fatalf("sourceDataPlan = %#v, want %#v", got, tc.want)
			}
		})
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
		if err := runtime.initGitOrigin(ctx, origin, "", currentUser()); err != nil {
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
	if err := runtime.initGitOrigin(ctx, origin, "", currentUser()); err != nil {
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
// the name the client picked. Until anything arrives the source has not
// landed, and the sandbox stays parked.
func TestPushedSourcesLandHeadingTheOriginAtWhatTheClientPushed(t *testing.T) {
	requirePOSIXHost(t)
	ctx := context.Background()
	runtime := deliveryTestRuntime(t)
	source := sandboxSource{slug: "primary", git: pushDeliveredSource("")}
	origin := runtime.sandboxOriginPath(deliveryTestSandboxID, "primary")
	if err := runtime.initGitOrigin(ctx, origin, "", currentUser()); err != nil {
		t.Fatalf("init git origin: %v", err)
	}
	landed, err := runtime.pushedSourcesLanded(ctx, deliveryTestSandboxID, []sandboxSource{source}, currentUser())
	if err != nil || landed {
		t.Fatalf("landed before any push = %v, %v; want parked", landed, err)
	}
	// git init points HEAD at init.defaultBranch, which is not what the client
	// pushes — so "has anything been pushed" cannot be asked of HEAD either.
	pushed := pushCommitToOrigin(t, origin, "discobox-source")
	landed, err = runtime.pushedSourcesLanded(ctx, deliveryTestSandboxID, []sandboxSource{source}, currentUser())
	if err != nil || !landed {
		t.Fatalf("landed after the push = %v, %v", landed, err)
	}
	if got, want := gitOutput(t, origin, "symbolic-ref", "HEAD"), "refs/heads/discobox-source"; got != want {
		t.Fatalf("origin HEAD = %q, want %q", got, want)
	}
	if got := gitOutput(t, origin, "rev-parse", "HEAD"); got != pushed {
		t.Fatalf("origin HEAD = %q, want the pushed commit %q", got, pushed)
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
	doc := buildSandboxDocument(linuxPaths, "project-1", "sandbox-1", "pool-1", "public-key", "", "sha256:resolved", req, nil, nil)
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
	got := resolveSandboxUser(linuxPaths, req)
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
	if got := resolveSandboxUser(linuxPaths, req); idOf(got.UID) != 0 {
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

func TestSandboxHostnameIsTheHyphenatedIDAsALegalLabel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sandboxID string
		want      string
	}{
		{name: "generated id", sandboxID: "sbx_dfzx0123456789ab", want: "sbx-dfzx0123456789ab"},
		{name: "no prefix", sandboxID: "bare0123456789ab", want: "bare0123456789ab"},
		{name: "illegal characters", sandboxID: "sbx_A b/c", want: "sbx-a-b-c"},
		{name: "trims edge hyphens", sandboxID: "-abc_", want: "abc"},
		{name: "nothing usable", sandboxID: "_", want: ""},
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
