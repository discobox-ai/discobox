package dockercache_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/obot-platform/discobox/sandbox-agent/dockercache"
)

// withMaterial stages the client certificate material a sandbox is given, which
// is what makes the pool builder reachable at all.
func withMaterial(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"mtls-ca.crt", "client.crt", "client.key"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("stage %s: %v", name, err)
		}
	}
	dockercache.SetMaterialDir(dir)
	t.Cleanup(func() { dockercache.SetMaterialDir(t.TempDir()) })
}

func argvAfterDocker(t *testing.T, got dockercache.Args) []string {
	t.Helper()
	if len(got.Argv) == 0 || got.Argv[0] != dockercache.RealDocker {
		t.Fatalf("argv does not exec the real docker CLI: %v", got.Argv)
	}
	return got.Argv[1:]
}

func TestBuildIsPointedAtThePoolBuilder(t *testing.T) {
	withMaterial(t)
	got := dockercache.Rewrite([]string{"build", "-t", "app:1", "."})
	if !got.Rewritten {
		t.Fatalf("build was not rewritten: %v", got.Argv)
	}
	argv := argvAfterDocker(t, got)

	// `docker build` is an alias for `docker buildx build` that pins the default
	// instance, so the subcommand must be spelled out for --builder to take.
	if argv[0] != "buildx" || argv[1] != "build" {
		t.Errorf("subcommand not spelled out, so --builder will be ignored: %v", argv)
	}
	if !slices.Contains(argv, "--builder") || !slices.Contains(argv, dockercache.BuilderName) {
		t.Errorf("build does not name the pool builder: %v", argv)
	}
	if !slices.Contains(argv, "-t") || !slices.Contains(argv, "app:1") {
		t.Errorf("the user's own arguments were lost: %v", argv)
	}
}

func TestUntaggedBuildStillLandsInTheLocalImageStore(t *testing.T) {
	withMaterial(t)
	// The case with no name at all: there is nothing to push to a registry, so
	// --load is the only way the result comes back. Without it the image
	// vanishes into the build cache and `docker images` shows nothing.
	argv := argvAfterDocker(t, dockercache.Rewrite([]string{"build", "."}))
	if !slices.Contains(argv, "--load") {
		t.Errorf("untagged build has no --load, so its image is unreachable: %v", argv)
	}
}

func TestAnExplicitOutputChoiceIsNotOverridden(t *testing.T) {
	withMaterial(t)
	for _, args := range [][]string{
		{"build", "--push", "-t", "reg/app:1", "."},
		{"build", "--output", "type=oci,dest=out.tar", "."},
	} {
		argv := argvAfterDocker(t, dockercache.Rewrite(args))
		if slices.Contains(argv, "--load") {
			t.Errorf("--load was added on top of %v, overriding the user's choice", args)
		}
	}
}

func TestAnExplicitBuilderWins(t *testing.T) {
	withMaterial(t)
	got := dockercache.Rewrite([]string{"build", "--builder", "mine", "."})
	if got.Rewritten {
		t.Error("the user named a builder and the shim overrode it, removing the escape hatch")
	}
}

func TestBuildxIsLeftAlone(t *testing.T) {
	withMaterial(t)
	// A user reaching for buildx directly has chosen their own semantics, and
	// buildx already honors the selected builder.
	got := dockercache.Rewrite([]string{"buildx", "build", "."})
	if got.Rewritten {
		t.Errorf("docker buildx was rewritten: %v", got.Argv)
	}
}

func TestNonBuildCommandsPassStraightThrough(t *testing.T) {
	withMaterial(t)
	for _, args := range [][]string{
		{"run", "--rm", "alpine"},
		{"ps"},
		{"images"},
	} {
		got := dockercache.Rewrite(args)
		if got.Rewritten {
			t.Errorf("%v was rewritten but is not a build", args)
		}
		if !slices.Equal(argvAfterDocker(t, got), args) {
			t.Errorf("%v was altered: %v", args, got.Argv)
		}
	}
}

func TestGlobalFlagsBeforeTheSubcommandAreHandled(t *testing.T) {
	withMaterial(t)
	// A value-taking global flag would otherwise hide the subcommand behind it.
	argv := argvAfterDocker(t, dockercache.Rewrite([]string{"--context", "remote", "build", "."}))
	if argv[0] != "--context" || argv[1] != "remote" {
		t.Errorf("global flags were reordered: %v", argv)
	}
	if !slices.Contains(argv, "--builder") {
		t.Errorf("build behind a global flag was not rewritten: %v", argv)
	}
}

func TestWithoutClientMaterialTheBuildStaysLocal(t *testing.T) {
	// No certificate means the mediator cannot authenticate this sandbox, so
	// there is no pool builder to reach. Building locally is worse than sharing
	// a cache and far better than failing.
	dockercache.SetMaterialDir(t.TempDir())
	got := dockercache.Rewrite([]string{"build", "."})
	if got.Rewritten {
		t.Error("build was pointed at a builder this sandbox cannot authenticate to")
	}
}

func TestTheShimNeverInjectsCacheFlags(t *testing.T) {
	withMaterial(t)
	argv := argvAfterDocker(t, dockercache.Rewrite([]string{"build", "."}))
	// A shared builder's cache is its own state. Cache flags would reintroduce
	// the per-sandbox import/export this design exists to remove.
	for _, flag := range argv {
		if strings.HasPrefix(flag, "--cache-from") || strings.HasPrefix(flag, "--cache-to") {
			t.Errorf("cache flag %q reintroduced: %v", flag, argv)
		}
	}
}
