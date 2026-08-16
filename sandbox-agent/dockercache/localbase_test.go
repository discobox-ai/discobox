package dockercache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func bases(t *testing.T, dockerfile string, args map[string]string) []string {
	t.Helper()
	return scanBases(dockerfile, args)
}

// The shape this repository's own harness images use: a base named by a
// build-arg, threaded in from the command line.
func TestABaseNamedByABuildArgIsFound(t *testing.T) {
	got := bases(t, `
ARG SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local
FROM ${SANDBOX_AGENT_IMAGE}
RUN true
`, map[string]string{})

	if len(got) != 1 || got[0] != "discobox-sandbox-agent:local" {
		t.Fatalf("bases = %v, want the ARG default expanded", got)
	}
}

func TestACommandLineBuildArgBeatsTheDefault(t *testing.T) {
	got := bases(t, "ARG BASE=fallback:local\nFROM $BASE\n",
		map[string]string{"BASE": "chosen:local"})

	if len(got) != 1 || got[0] != "chosen:local" {
		t.Fatalf("bases = %v, want the command line's value", got)
	}
}

// A later stage building on an earlier one names a stage, not an image.
func TestAStageNameIsNotABase(t *testing.T) {
	got := bases(t, `
FROM debian:12-slim AS builder
RUN true
FROM builder
RUN true
FROM scratch
`, nil)

	if len(got) != 1 || got[0] != "debian:12-slim" {
		t.Fatalf("bases = %v, want only the real image", got)
	}
}

// A COPY --from naming a stage must not drag the stage in either, and a FROM
// split over continuations is still one instruction.
func TestContinuationsAndCommentsAreRead(t *testing.T) {
	got := bases(t, `
# a comment mentioning FROM notanimage:local
FROM \
  base-one:local AS first
COPY --from=first /a /b
FROM base-two:local
`, nil)

	if strings.Join(got, ",") != "base-one:local,base-two:local" {
		t.Fatalf("bases = %v, want both real bases and nothing from the comment", got)
	}
}

// A base still holding an unresolved variable is left alone: acting on half a
// name would be worse than leaving the build as it was.
func TestAnUnresolvedVariableIsSkipped(t *testing.T) {
	if got := bases(t, "FROM ${NOT_SET}\n", nil); len(got) != 0 {
		t.Fatalf("bases = %v, want nothing for a name that cannot be resolved", got)
	}
}

func TestTheDefaultFormOfAVariableIsHonoured(t *testing.T) {
	got := bases(t, "FROM ${BASE:-fallback:local}\n", nil)
	if len(got) != 1 || got[0] != "fallback:local" {
		t.Fatalf("bases = %v, want the default", got)
	}
}

// A reference that says where it comes from is not ours to redirect, whatever
// is in the local store.
func TestAReferenceNamingARegistryIsLeftAlone(t *testing.T) {
	for _, ref := range []string{
		"ghcr.io/example/thing:v1",
		"discobox-pool-proxy:5000/x/y:build",
		"docker.io/library/debian:12",
	} {
		if !namesARegistry(ref) {
			t.Errorf("%s names a registry and should be left alone", ref)
		}
	}
	for _, ref := range []string{"discobox-sandbox-agent:local", "debian:12-slim", "myimage"} {
		if namesARegistry(ref) {
			t.Errorf("%s is unqualified and is the case worth redirecting", ref)
		}
	}
}

func TestTheNamespacedReferenceKeepsTheNameAndTag(t *testing.T) {
	got, err := namespacedRef("sbx-abc123", "discobox-sandbox-agent:local")
	if err != nil {
		t.Fatalf("namespacedRef: %v", err)
	}
	want := PoolRegistry + "/sbx-abc123/discobox-sandbox-agent:local"
	if got != want {
		t.Fatalf("ref = %q, want %q", got, want)
	}
}

// An unqualified name with a path is flattened rather than becoming a
// namespace inside the sandbox's own.
func TestAPathInTheNameIsFlattened(t *testing.T) {
	got, err := namespacedRef("sbx-abc123", "team/thing:local")
	if err != nil {
		t.Fatalf("namespacedRef: %v", err)
	}
	if want := PoolRegistry + "/sbx-abc123/team_thing:local"; got != want {
		t.Fatalf("ref = %q, want %q", got, want)
	}
}

func TestAnUntaggedBaseGetsLatest(t *testing.T) {
	got, err := namespacedRef("sbx-abc123", "myimage")
	if err != nil {
		t.Fatalf("namespacedRef: %v", err)
	}
	if want := PoolRegistry + "/sbx-abc123/myimage:latest"; got != want {
		t.Fatalf("ref = %q, want %q", got, want)
	}
}

func TestTheDockerfileIsFoundWhereTheBuildSaysItIs(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"default in the context", []string{"build", "-t", "x:local", "somedir"}, "somedir/Dockerfile"},
		{"explicit -f", []string{"build", "-f", "a/B.dockerfile", "."}, "a/B.dockerfile"},
		{"joined --file=", []string{"build", "--file=a/B.dockerfile", "."}, "a/B.dockerfile"},
		{"context defaults to the working directory", []string{"build", "."}, "Dockerfile"},
		{"a flag value is not the context", []string{"build", "--build-arg", "X=y", "ctx"}, "ctx/Dockerfile"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dockerfilePath(tc.args); got != tc.want {
				t.Fatalf("dockerfilePath = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildArgsAreReadInBothSpellings(t *testing.T) {
	got := buildArgs([]string{"build", "--build-arg", "A=1", "--build-arg=B=2", "."})
	if got["A"] != "1" || got["B"] != "2" {
		t.Fatalf("buildArgs = %v, want both spellings read", got)
	}
}

// A sandbox with no namespace staged gets no redirects: the pool has not given
// this sandbox a place to publish to, and a build is better off unchanged than
// pointed somewhere that does not exist.
func TestWithoutANamespaceNothingIsRedirected(t *testing.T) {
	namespacePath = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { namespacePath = registryNamespaceFile })

	if got := localBaseContexts(t.Context(), []string{"build", "."}); got != nil {
		t.Fatalf("contexts = %v, want none without a namespace", got)
	}
}

// A Dockerfile that cannot be read is not a build to interfere with.
func TestAnUnreadableDockerfileIsLeftAlone(t *testing.T) {
	dir := t.TempDir()
	namespacePath = filepath.Join(dir, "ns")
	if err := os.WriteFile(namespacePath, []byte("sbx-abc123\n"), 0o600); err != nil {
		t.Fatalf("stage namespace: %v", err)
	}
	t.Cleanup(func() { namespacePath = registryNamespaceFile })

	args := []string{"build", "-f", filepath.Join(dir, "nope.Dockerfile"), dir}
	if got := localBaseContexts(t.Context(), args); got != nil {
		t.Fatalf("contexts = %v, want none for a Dockerfile that is not there", got)
	}
}

// Nothing is published for a build whose bases all name a registry: they say
// where they come from and the pool builder can reach the same place.
func TestARemoteOnlyBuildPublishesNothing(t *testing.T) {
	dir := t.TempDir()
	namespacePath = filepath.Join(dir, "ns")
	if err := os.WriteFile(namespacePath, []byte("sbx-abc123\n"), 0o600); err != nil {
		t.Fatalf("stage namespace: %v", err)
	}
	t.Cleanup(func() { namespacePath = registryNamespaceFile })

	dockerfile := filepath.Join(dir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte("FROM ghcr.io/example/thing:v1\nRUN true\n"), 0o600); err != nil {
		t.Fatalf("stage Dockerfile: %v", err)
	}
	if got := localBaseContexts(t.Context(), []string{"build", "-f", dockerfile, dir}); got != nil {
		t.Fatalf("contexts = %v, want none for a base that names its registry", got)
	}
}
