package dockercache_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/distribution/reference"
	"github.com/obot-platform/discobox/sandbox-agent/dockercache"
)

// withMaterial stages the forwarder config pool-agent writes for a sandbox
// whose pool runs a builder, which is what makes one reachable at all.
func withMaterial(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bridge-buildkit.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("stage forwarder config: %v", err)
	}
	dockercache.SetBridgeConfigPath(path)
	t.Cleanup(func() { dockercache.SetBridgeConfigPath(filepath.Join(t.TempDir(), "absent")) })
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
	// The user's tag is applied locally after the pull, not pushed: a tag may
	// name any registry, and rewriting arbitrary tag syntax to be pushable to
	// the pool would be a source of surprises.
	if slices.Contains(argv, "app:1") {
		t.Errorf("the user's tag was pushed to the pool registry: %v", argv)
	}
	if !slices.Contains(got.Tags, "app:1") {
		t.Errorf("the user's tag was dropped rather than deferred: %v", got.Tags)
	}
	if !slices.Contains(argv, "--push") || !slices.Contains(argv, got.RegistryRef) {
		t.Errorf("build does not push to the pool registry: %v", argv)
	}
	if !strings.HasPrefix(got.RegistryRef, dockercache.PoolRegistry+"/") {
		t.Errorf("registry reference is not in the pool registry: %q", got.RegistryRef)
	}
}

func TestUntaggedBuildStillHasSomethingToPush(t *testing.T) {
	withMaterial(t)
	// A build naming no tag has nothing to push on its own, so a reference is
	// synthesized for it. Without one the result would stay in the pool and
	// `docker images` would show nothing.
	got := dockercache.Rewrite([]string{"build", "."})
	if got.RegistryRef == "" {
		t.Fatal("untagged build has no reference, so its result cannot come back")
	}
	if len(got.Tags) != 0 {
		t.Errorf("untagged build invented a tag: %v", got.Tags)
	}
	if !slices.Contains(argvAfterDocker(t, got), got.RegistryRef) {
		t.Error("the synthesized reference is not what the build pushes")
	}
}

func TestIdenticalBuildsProduceTheSameImage(t *testing.T) {
	withMaterial(t)
	// buildx attests provenance whenever the output is a registry, and that
	// provenance records the build rather than its result — so two builds of
	// identical content get different index digests, and the same Dockerfile
	// shows a different image ID in every sandbox even on a full cache hit.
	// Plain `docker build` attests nothing; moving the transport to a registry
	// must not change what the user ends up holding.
	argv := argvAfterDocker(t, dockercache.Rewrite([]string{"build", "."}))
	if !slices.Contains(argv, "--provenance=false") {
		t.Errorf("build attests provenance, so its image ID varies per build: %v", argv)
	}
}

func TestAnAskedForAttestationSurvives(t *testing.T) {
	withMaterial(t)
	// Someone who asked for provenance or an SBOM wants it; turning it off
	// under them would drop what they asked for without saying so.
	for _, flag := range []string{"--provenance=true", "--sbom=true", "--attest", "--provenance"} {
		argv := argvAfterDocker(t, dockercache.Rewrite([]string{"build", flag, "."}))
		if slices.Contains(argv, "--provenance=false") {
			t.Errorf("%s was overridden by the shim: %v", flag, argv)
		}
	}
}

func TestTheSynthesizedReferenceIsActuallyPushable(t *testing.T) {
	withMaterial(t)
	// The name is synthesized, so nothing upstream validates it until the
	// client parses the command line — at which point the build fails before
	// it starts, with an error about a reference the user never wrote. This
	// checks it against the real grammar rather than a guess at one: a path
	// component must begin with an alphanumeric, which is what a name like
	// "_build" quietly violates.
	for _, argv := range [][]string{{"build", "."}, {"build", "-t", "app:v1", "."}} {
		got := dockercache.Rewrite(argv)
		if _, err := reference.ParseNormalizedNamed(got.RegistryRef); err != nil {
			t.Errorf("docker build %v pushes to an unparseable reference %q: %v", argv, got.RegistryRef, err)
		}
	}
}

func TestEachBuildPushesToItsOwnReference(t *testing.T) {
	withMaterial(t)
	// Two builds must not collide in the pool registry, or a concurrent build
	// could pull the other's result.
	first := dockercache.Rewrite([]string{"build", "."})
	second := dockercache.Rewrite([]string{"build", "."})
	if first.RegistryRef == second.RegistryRef {
		t.Errorf("two builds share a reference: %q", first.RegistryRef)
	}
}

func TestAnExplicitOutputChoiceIsNotOverridden(t *testing.T) {
	withMaterial(t)
	for _, args := range [][]string{
		{"build", "--push", "-t", "reg/app:1", "."},
		{"build", "--output", "type=oci,dest=out.tar", "."},
	} {
		got := dockercache.Rewrite(args)
		if got.Rewritten {
			t.Errorf("%v names its own output and was rewritten anyway", args)
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

func TestWithoutAPoolBuilderTheBuildStaysLocal(t *testing.T) {
	// No forwarder config means this pool runs no builder. Building locally is
	// worse than sharing a cache and far better than failing.
	dockercache.SetBridgeConfigPath(filepath.Join(t.TempDir(), "absent"))
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
