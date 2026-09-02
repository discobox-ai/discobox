package main

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/discobox-ai/discobox/devimage"
	"github.com/discobox-ai/discobox/harness"
)

func loadDockerImageSpecs(t *testing.T) ([]imageSpec, string) {
	t.Helper()
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	specs, err := dockerImageSpecs(context.Background(), repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	return specs, repoRoot
}

func TestDockerImageSpecsBuildAllDevImagesAndUpdateEnv(t *testing.T) {
	specs, _ := loadDockerImageSpecs(t)
	if len(specs) != 6 {
		t.Fatalf("image specs = %d, want base, worker, sandbox, and three harnesses", len(specs))
	}

	gotNames := make([]string, 0, len(specs))
	gotEnvKeys := map[string]bool{}
	for _, spec := range specs {
		gotNames = append(gotNames, spec.name)
		// The shared base is the one image nothing runs, so it is also the one
		// image with no reference for the server to resolve.
		if spec.intermediate {
			if spec.envImageKey != "" {
				t.Fatalf("%s is an intermediate but publishes %s", spec.name, spec.envImageKey)
			}
			continue
		}
		if spec.envImageKey == "" {
			t.Fatalf("%s does not update an image reference in .env", spec.name)
		}
		gotEnvKeys[spec.envImageKey] = true
		if spec.envDigestKey != "" {
			gotEnvKeys[spec.envDigestKey] = true
		}
	}
	wantNames := []string{baseSpecName, "pool-agent", "sandbox-agent", "harness-codex", "harness-claude-code", "harness-shell"}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("image build order = %#v, want %#v", gotNames, wantNames)
	}
	for _, key := range []string{
		"DISCOBOX_DOCKER_POOL_IMAGE",
		"DISCOBOX_DOCKER_POOL_IMAGE_DIGEST",
		"DISCOBOX_DEFAULT_SANDBOX_IMAGE",
		"DISCOBOX_DEFAULT_SANDBOX_IMAGE_DIGEST",
		"DISCOBOX_HARNESS_CODEX_IMAGE",
		"DISCOBOX_HARNESS_CLAUDE_CODE_IMAGE",
		"DISCOBOX_HARNESS_SHELL_IMAGE",
	} {
		if !gotEnvKeys[key] {
			t.Errorf("watcher does not update %s", key)
		}
	}
}

func TestDockerImageSpecsIncludeIndependentlyWatchedHarnesses(t *testing.T) {
	specs, repoRoot := loadDockerImageSpecs(t)
	sandboxDockerfile := filepath.Join(repoRoot, "sandbox-agent", "Dockerfile")
	for _, name := range []string{"codex", "claude-code", "shell"} {
		var found *imageSpec
		for i := range specs {
			if specs[i].name == "harness-"+name {
				found = &specs[i]
				break
			}
		}
		if found == nil {
			t.Fatalf("missing watcher for %s", name)
		}
		args := strings.Join(found.buildArgs, " ")
		if !strings.Contains(args, filepath.Join("harness", harnessDir(name), "Dockerfile")) ||
			!strings.Contains(args, "SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local") {
			t.Fatalf("watcher for %s does not use its harness Dockerfile and sandbox base: %#v", name, found)
		}
		if !contains(found.files, sandboxDockerfile) {
			t.Fatalf("%s watcher does not rebuild when the sandbox base Dockerfile changes", name)
		}
		for _, file := range found.files {
			if strings.HasSuffix(file, "image.json") && strings.Contains(file, string(filepath.Separator)+"harness"+string(filepath.Separator)) &&
				!strings.Contains(file, filepath.Join("harness", harnessDir(name), "image.json")) {
				t.Fatalf("%s watcher includes unrelated metadata %s", name, file)
			}
		}
	}
}

func TestUpdateEnvUpdatesAllImageReferencesWithoutDroppingUserValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("USER_SETTING=keep\nDISCOBOX_DEFAULT_SANDBOX_IMAGE=old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"DISCOBOX_DOCKER_POOL_IMAGE":            "discobox-pool-agent:dev-worker",
		"DISCOBOX_DOCKER_POOL_IMAGE_DIGEST":     "sha256:worker",
		"DISCOBOX_DEFAULT_SANDBOX_IMAGE":        "discobox-sandbox-agent:dev-sandbox",
		"DISCOBOX_DEFAULT_SANDBOX_IMAGE_DIGEST": "sha256:sandbox",
		"DISCOBOX_HARNESS_CODEX_IMAGE":          "discobox-harness-codex:dev-codex",
		"DISCOBOX_HARNESS_CLAUDE_CODE_IMAGE":    "discobox-harness-claude-code:dev-claude",
		devimage.SyncEnv:                        "true",
		devimage.ManifestEnv:                    "/tmp/discobox-dev-images.json",
	}
	if err := updateEnv(path, values); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	env := string(data)
	if !strings.Contains(env, "USER_SETTING=keep\n") {
		t.Fatalf("user value was not preserved:\n%s", env)
	}
	for key, value := range values {
		if !strings.Contains(env, key+"="+value+"\n") {
			t.Errorf("%s was not updated in .env:\n%s", key, env)
		}
	}
}

// Every image built FROM another must both declare that parent and carry the
// build argument buildImage rewrites, or it silently builds on the mutable
// :local tag instead of the exact base it was meant to pin.
func TestSpecsThreadTheImageTheyBuildFrom(t *testing.T) {
	wantParents := map[string]string{
		baseSpecName:          "",
		"pool-agent":          baseSpecName,
		"sandbox-agent":       baseSpecName,
		"harness-codex":       sandboxAgentSpecName,
		"harness-claude-code": sandboxAgentSpecName,
		"harness-shell":       sandboxAgentSpecName,
	}
	wantArgs := map[string]string{
		"pool-agent":          "BASE_IMAGE",
		"sandbox-agent":       "BASE_IMAGE",
		"harness-codex":       "SANDBOX_AGENT_IMAGE",
		"harness-claude-code": "SANDBOX_AGENT_IMAGE",
		"harness-shell":       "SANDBOX_AGENT_IMAGE",
	}
	specs, _ := loadDockerImageSpecs(t)
	for _, spec := range specs {
		want, ok := wantParents[spec.name]
		if !ok {
			t.Fatalf("unexpected image spec %q", spec.name)
		}
		if spec.parent != want {
			t.Errorf("%s parent = %q, want %q", spec.name, spec.parent, want)
		}
		wantArg := wantArgs[spec.name]
		if spec.parentArg != wantArg {
			t.Errorf("%s parentArg = %q, want %q", spec.name, spec.parentArg, wantArg)
		}
		if wantArg == "" {
			continue
		}
		hasArg := false
		for _, arg := range spec.buildArgs {
			if strings.HasPrefix(arg, wantArg+"=") {
				hasArg = true
			}
		}
		if !hasArg {
			t.Errorf("%s has no %s build arg to rewrite: %#v", spec.name, wantArg, spec.buildArgs)
		}
	}
}

func TestParentsFirstOrdersEveryBaseBeforeWhatBuildsOnIt(t *testing.T) {
	in := []imageSpec{
		{name: "harness-codex", parent: sandboxAgentSpecName},
		{name: "sandbox-agent", parent: baseSpecName},
		{name: "harness-claude-code", parent: sandboxAgentSpecName},
		{name: baseSpecName},
		{name: "pool-agent", parent: baseSpecName},
	}
	got := parentsFirst(in)
	if len(got) != len(in) {
		t.Fatalf("ordering dropped specs: got %d want %d", len(got), len(in))
	}
	at := map[string]int{}
	for i, spec := range got {
		at[spec.name] = i
	}
	for _, spec := range in {
		if spec.parent == "" {
			continue
		}
		if at[spec.parent] > at[spec.name] {
			t.Errorf("%s built before its base %s: %v", spec.name, spec.parent, specNames(got))
		}
	}
}

// A pass that rebuilds only a child must not stall waiting for a parent that is
// not in the set: that is the common case, one harness edited on an unchanged
// base.
func TestParentsFirstPlacesSpecsWhoseParentIsNotInTheSet(t *testing.T) {
	in := []imageSpec{{name: "harness-codex", parent: sandboxAgentSpecName}}
	got := parentsFirst(in)
	if len(got) != 1 || got[0].name != "harness-codex" {
		t.Fatalf("parentsFirst = %v, want [harness-codex]", specNames(got))
	}
}

func TestDevImageTag(t *testing.T) {
	got := devImageTag("discobox-sandbox-agent:dev-", "sha256:0123456789abcdef0000")
	if got != "discobox-sandbox-agent:dev-0123456789ab" {
		t.Fatalf("devImageTag = %q", got)
	}
}

// An image whose Dockerfile turns a metadata build argument into a manifest
// label must be built with that argument, or it ships an empty label — and the
// failure is quiet. For a harness image's own label that means a harness config
// skipped at seeding; for the base image's layer it means every harness image
// built on it inherits nothing and is rejected as not built from the base
// (ADR 0086 §1), which is every harness at once.
//
// The pairing runs both ways: an argument with no LABEL to land in is just as
// broken as a LABEL with no argument, and reads as working.
func TestSpecsPassImageMetadataToEveryLabeledImage(t *testing.T) {
	specs, _ := loadDockerImageSpecs(t)
	labeled := map[string]string{}
	for _, spec := range specs {
		data, err := os.ReadFile(filepath.Join(spec.contextDir, spec.dockerfile))
		if err != nil {
			t.Fatal(err)
		}
		dockerfile := string(data)
		declaresOwn := strings.Contains(dockerfile, "LABEL "+harness.ImageLabel+"=")
		declaresLayer := strings.Contains(dockerfile, "LABEL "+harness.ImageLayerLabelPrefix)
		if !declaresOwn && !declaresLayer {
			if spec.metadataArg != "" {
				t.Errorf("%s is built with %s but its Dockerfile has no label to put it in", spec.name, spec.metadataArg)
			}
			continue
		}
		labeled[spec.name] = spec.metadataArg
		if spec.metadataFile == "" {
			t.Errorf("%s declares a manifest label but its spec has no image.json to fill it from", spec.name)
			continue
		}
		if !contains(spec.buildArgs, spec.metadataArg+"=") {
			t.Errorf("%s has no %s= placeholder for buildImage to fill in: %#v", spec.name, spec.metadataArg, spec.buildArgs)
		}
	}
	// The base layer, plus one per harness image that declares anything. `shell`
	// declares nothing at all: its whole manifest is the layer it inherits.
	want := map[string]string{
		"sandbox-agent":       harness.LayerMetadataBuildArg,
		"harness-claude-code": harness.MetadataBuildArg,
		"harness-codex":       harness.MetadataBuildArg,
	}
	if !maps.Equal(labeled, want) {
		t.Fatalf("labeled images = %v, want %v", labeled, want)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func harnessDir(name string) string {
	if name == "codex" {
		return "codex-cli"
	}
	return name
}

// The placeholder in a spec's argv must actually be replaced with the resolved
// parent reference. Missing it is silent: the build succeeds against the
// mutable :local tag, and the image is pinned to a base that can be replaced
// underneath it.
func TestRenderBuildArgsPinsTheResolvedParentReference(t *testing.T) {
	spec := imageSpec{
		name:      "sandbox-agent",
		parent:    baseSpecName,
		parentArg: "BASE_IMAGE",
		buildArgs: []string{"build", "-f", "sandbox-agent/Dockerfile",
			"--build-arg", "BASE_IMAGE=discobox-base:local",
			"-t", "discobox-sandbox-agent:local", "."},
	}
	got, err := renderBuildArgs(spec, "discobox-base:dev-0123456789ab")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(got, "BASE_IMAGE=discobox-base:dev-0123456789ab") {
		t.Fatalf("parent reference not pinned: %#v", got)
	}
	if contains(got, "BASE_IMAGE=discobox-base:local") {
		t.Fatalf("mutable base tag survived: %#v", got)
	}
	if contains(spec.buildArgs, "BASE_IMAGE=discobox-base:dev-0123456789ab") {
		t.Fatal("renderBuildArgs mutated the spec's own argv")
	}
}

// A pass that rebuilds a child while its parent is unchanged resolves the parent
// separately; when there is nothing to resolve the spec's own default must stand
// rather than being blanked.
func TestRenderBuildArgsLeavesTheDefaultWhenThereIsNoParentToPin(t *testing.T) {
	spec := imageSpec{
		name:      "harness-shell",
		parent:    sandboxAgentSpecName,
		parentArg: "SANDBOX_AGENT_IMAGE",
		buildArgs: []string{"build", "--build-arg", "SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local"},
	}
	got, err := renderBuildArgs(spec, "")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(got, "SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local") {
		t.Fatalf("default base tag was clobbered: %#v", got)
	}
}

// The dev tag a child's FROM pins has to exist locally, not merely be derivable:
// docker reads a tag it cannot find as a Docker Hub repository and goes off to
// pull it, so a base rebuilt by anything other than this watcher — task
// build:base-image, the Dockerfile hook — fails the child's build with a
// registry error rather than a missing-image one.
func TestResolveParentImageTagsTheBaseItResolves(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script on PATH")
	}
	calls := filepath.Join(t.TempDir(), "calls")
	repoRoot := fakeDockerPath(t, "sha256:c2afa1b19563247d43a4b02802287dc907cd62e04039f0652488f45a7d07b9c9", calls)
	specs := []imageSpec{{name: "base", baseImage: "discobox-base:local", devPrefix: "discobox-base:dev-"}}

	got, err := resolveParentImage(t.Context(), repoRoot, specs, "base")
	if err != nil {
		t.Fatalf("resolve parent image: %v", err)
	}
	if want := "discobox-base:dev-c2afa1b19563"; got != want {
		t.Fatalf("resolved = %q, want %q", got, want)
	}
	if want := "tag discobox-base:local discobox-base:dev-c2afa1b19563"; !strings.Contains(readFile(t, calls), want) {
		t.Fatalf("docker calls = %q, want the resolved tag applied", readFile(t, calls))
	}
}

// fakeDockerPath puts a docker on PATH that answers `image inspect` with imageID
// and records every invocation in calls, and returns a directory to run in.
func fakeDockerPath(t *testing.T, imageID, calls string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho \"$@\" >> " + calls + "\ncase \"$1 $2\" in\n'image inspect') echo " + imageID + " ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
