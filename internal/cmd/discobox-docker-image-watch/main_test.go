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

	"github.com/discobox-ai/discobox/harness"
	"github.com/discobox-ai/discobox/pkg/devimage"
)

func loadDockerImageSpecs(t *testing.T) ([]imageSpec, string) {
	t.Helper()
	// findRepoRoot rather than a count of "..": the command has moved down the
	// tree once already, and a hardcoded depth fails the whole package the next
	// time it does.
	repoRoot, err := findRepoRoot()
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
	if len(specs) != 18 {
		t.Fatalf("image specs = %d, want base, worker, sandbox, platform, and seven harnesses' bundles and twins", len(specs))
	}

	gotNames := make([]string, 0, len(specs))
	gotEnvKeys := map[string]bool{}
	for _, spec := range specs {
		if spec.bundle && !strings.Contains(strings.Join(spec.buildArgs, " "), "--builder "+*bundleBuilder+" ") {
			t.Errorf("%s does not select the OCI bundle builder: %v", spec.name, spec.buildArgs)
		}
		if !spec.bundle && contains(spec.buildArgs, "--builder") {
			t.Errorf("%s overrides the runnable-image builder: %v", spec.name, spec.buildArgs)
		}
		gotNames = append(gotNames, spec.name)
		if spec.envImageKey == "" {
			t.Fatalf("%s does not update an image reference in .env", spec.name)
		}
		gotEnvKeys[spec.envImageKey] = true
		if spec.envDigestKey != "" {
			gotEnvKeys[spec.envDigestKey] = true
		}
	}
	wantNames := []string{baseSpecName, "pool-agent", "sandbox-agent", "platform"}
	for _, name := range []string{"codex", "claude-code", "opencode", "copilot", "omp", "pi", "shell"} {
		wantNames = append(wantNames, "harness-"+name, "harness-"+name+"-image")
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("image build order = %#v, want %#v", gotNames, wantNames)
	}
	for _, key := range []string{
		"DISCOBOX_DOCKER_POOL_IMAGE",
		"DISCOBOX_DOCKER_POOL_IMAGE_DIGEST",
		"DISCOBOX_DEFAULT_SANDBOX_IMAGE",
		"DISCOBOX_DEFAULT_SANDBOX_IMAGE_DIGEST",
		"DISCOBOX_PLATFORM_BUNDLE",
		"DISCOBOX_HARNESS_CODEX_BUNDLE",
		"DISCOBOX_HARNESS_CODEX_IMAGE",
		"DISCOBOX_HARNESS_CLAUDE_CODE_BUNDLE",
		"DISCOBOX_HARNESS_CLAUDE_CODE_IMAGE",
		"DISCOBOX_HARNESS_OPENCODE_BUNDLE",
		"DISCOBOX_HARNESS_OPENCODE_IMAGE",
		"DISCOBOX_HARNESS_COPILOT_BUNDLE",
		"DISCOBOX_HARNESS_COPILOT_IMAGE",
		"DISCOBOX_HARNESS_OMP_BUNDLE",
		"DISCOBOX_HARNESS_OMP_IMAGE",
		"DISCOBOX_HARNESS_PI_BUNDLE",
		"DISCOBOX_HARNESS_PI_IMAGE",
		"DISCOBOX_HARNESS_SHELL_BUNDLE",
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
		dockerfile := filepath.Join("harness", harnessDir(name), "Dockerfile")
		bundle, ok := specNamed(specs, "harness-"+name)
		if !ok {
			t.Fatalf("missing bundle watcher for %s", name)
		}
		args := strings.Join(bundle.buildArgs, " ")
		if !strings.Contains(args, dockerfile) || !strings.Contains(args, "--target bundle") {
			t.Fatalf("bundle watcher for %s does not build its Dockerfile's bundle stage: %#v", name, bundle)
		}
		// Exported as a layout, never into an image store.
		layoutDir := filepath.Join(repoRoot, "build", "assets", "discobox-harness-"+name)
		if bundle.layoutDir != layoutDir || !strings.HasPrefix(args, "buildx build ") ||
			!strings.Contains(args, "--output type=oci,dest="+partialLayout(layoutDir)) || contains(bundle.buildArgs, "-t") {
			t.Fatalf("bundle watcher for %s does not export its bundle into %s: %#v", name, layoutDir, bundle)
		}
		// FROM scratch: nothing of the sandbox agent is an input of the bundle.
		if contains(bundle.files, sandboxDockerfile) || strings.Contains(args, "SANDBOX_AGENT_IMAGE") || bundle.parent != "" {
			t.Fatalf("%s bundle is built on the sandbox agent image: %#v", name, bundle)
		}
		if !bundle.bundle {
			t.Fatalf("%s bundle is not marked as one, so copy-mode would copy it to pool daemons", name)
		}

		image, ok := specNamed(specs, "harness-"+name+"-image")
		if !ok {
			t.Fatalf("missing runnable image watcher for %s", name)
		}
		args = strings.Join(image.buildArgs, " ")
		if !strings.Contains(args, dockerfile) || !strings.Contains(args, "--target image") ||
			!strings.Contains(args, "SANDBOX_AGENT_IMAGE=discobox-sandbox-agent:local") ||
			!strings.Contains(args, "-t discobox-harness-"+name+"-image:local") {
			t.Fatalf("image watcher for %s does not build its Dockerfile's image stage on the sandbox base: %#v", name, image)
		}
		if image.bundle {
			t.Fatalf("%s runnable image is marked as a bundle, so copy-mode would not copy it to pool daemons", name)
		}
		if !contains(image.files, sandboxDockerfile) {
			t.Fatalf("%s image watcher does not rebuild when the sandbox base Dockerfile changes", name)
		}
		for _, spec := range []imageSpec{bundle, image} {
			for _, file := range spec.files {
				if strings.HasSuffix(file, "image.json") && strings.Contains(file, string(filepath.Separator)+"harness"+string(filepath.Separator)) &&
					!strings.Contains(file, filepath.Join("harness", harnessDir(name), "image.json")) {
					t.Fatalf("%s watcher includes unrelated metadata %s", spec.name, file)
				}
			}
		}
	}
}

// The platform bundle is the sandbox agent Dockerfile's FROM scratch stage, so
// it is built from the same inputs as the runnable image less the shared base.
func TestDockerImageSpecsBuildThePlatformBundleFromItsOwnStage(t *testing.T) {
	specs, repoRoot := loadDockerImageSpecs(t)
	platform, ok := specNamed(specs, platformSpecName)
	if !ok {
		t.Fatal("missing platform bundle watcher")
	}
	args := strings.Join(platform.buildArgs, " ")
	if !strings.Contains(args, "-f sandbox-agent/Dockerfile --target platform") ||
		!strings.Contains(args, "--output type=oci,dest="+partialLayout(filepath.Join(repoRoot, "build", "assets", "discobox-platform"))) ||
		platform.envImageKey != "DISCOBOX_PLATFORM_BUNDLE" || !platform.bundle || platform.parent != "" {
		t.Fatalf("platform bundle spec = %#v", platform)
	}
	for _, rel := range []string{"sandbox-agent/Dockerfile", "sandbox-agent/image.json", "harness/prompt-answer.sh"} {
		if !contains(platform.files, filepath.Join(repoRoot, filepath.FromSlash(rel))) {
			t.Errorf("platform bundle watcher does not rebuild when %s changes", rel)
		}
	}
	if contains(platform.files, filepath.Join(repoRoot, "server", "providers", "dockerworker", "sandbox-image", "Dockerfile")) {
		t.Error("platform bundle is FROM scratch but rebuilds when the sandbox base image changes")
	}
	sandbox, _ := specNamed(specs, sandboxAgentSpecName)
	if !strings.Contains(strings.Join(sandbox.buildArgs, " "), "--target sandbox-agent") || sandbox.target != "sandbox-agent" {
		t.Fatalf("sandbox agent spec does not build the runnable stage by name: %#v", sandbox.buildArgs)
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

func TestDevelopmentImageEnvPublishesTheCompleteManifestAfterAPartialBuild(t *testing.T) {
	specs := []imageSpec{
		{name: baseSpecName, devPrefix: "m-base:dev-", envImageKey: "BASE"},
		{name: "sandbox-agent", devPrefix: "z-sandbox:dev-", envImageKey: "SANDBOX", envDigestKey: "SANDBOX_DIGEST"},
		{name: "platform", devPrefix: "platform:dev-", envImageKey: "PLATFORM", bundle: true},
		{name: "pool-agent", devPrefix: "a-pool:dev-", envImageKey: "POOL", envDigestKey: "POOL_DIGEST"},
	}
	// A bundle is not among the images a pool daemon is given, but its
	// reference is still published: the server serves it from its layout.
	platformDigest := "sha256:" + strings.Repeat("ab", 32)
	manifest, err := devimage.NewManifest([]devimage.Image{
		{Reference: "z-sandbox:dev-new", ID: "sha256:sandbox-new"},
		{Reference: "a-pool:dev-new", ID: "sha256:pool-new"},
		{Reference: "m-base:dev-new", ID: "sha256:base-new"},
	}, []devimage.Bundle{{Reference: "platform:dev-new", Digest: platformDigest, Layout: filepath.Join(t.TempDir(), "platform.tar")}})
	if err != nil {
		t.Fatal(err)
	}

	values, err := developmentImageEnv(specs, manifest, "/tmp/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"POOL": "a-pool:dev-new", "POOL_DIGEST": "sha256:pool-new",
		"SANDBOX": "z-sandbox:dev-new", "SANDBOX_DIGEST": "sha256:sandbox-new",
		"PLATFORM":       "platform:dev-new",
		"BASE":           "m-base:dev-new",
		devimage.SyncEnv: "true", devimage.ManifestEnv: "/tmp/manifest.json",
	}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("environment = %#v, want complete manifest values %#v", values, want)
	}
}

// Every image built FROM another must both declare that parent and carry the
// build argument buildImage rewrites, or it silently builds on the mutable
// :local tag instead of the exact base it was meant to pin.
func TestSpecsThreadTheImageTheyBuildFrom(t *testing.T) {
	// Bundles are FROM scratch, so only the agents and the harnesses' twins
	// build FROM another image.
	wantParents := map[string]string{
		baseSpecName:     "",
		"pool-agent":     baseSpecName,
		"sandbox-agent":  baseSpecName,
		platformSpecName: "",
	}
	wantArgs := map[string]string{
		"pool-agent":    "BASE_IMAGE",
		"sandbox-agent": "BASE_IMAGE",
	}
	for _, harnessImage := range harnessImages {
		wantParents["harness-"+harnessImage.name] = ""
		wantParents["harness-"+harnessImage.name+"-image"] = sandboxAgentSpecName
		wantArgs["harness-"+harnessImage.name+"-image"] = "SANDBOX_AGENT_IMAGE"
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
// label must be built with that argument. The Dockerfiles refuse an empty one,
// so a miss is a failed build rather than a quietly empty label — but only
// once the watcher runs it, and for the platform's layer it would be every
// harness at once.
//
// Checked per build target, since one Dockerfile now builds two artifacts: a
// target needs the argument when any stage it builds labels with it.
// An argument passed to a Dockerfile that declares no such ARG is just as
// broken, and reads as working.
func TestSpecsPassImageMetadataToEveryLabeledImage(t *testing.T) {
	specs, _ := loadDockerImageSpecs(t)
	labeled := map[string]string{}
	for _, spec := range specs {
		data, err := os.ReadFile(filepath.Join(spec.contextDir, spec.dockerfile))
		if err != nil {
			t.Fatal(err)
		}
		dockerfile := string(data)
		built := builtStages(dockerfile, spec.target)
		if len(built) == 0 {
			t.Fatalf("%s builds target %q, which %s has no stage for", spec.name, spec.target, spec.dockerfile)
		}
		declares := strings.Contains(built, "LABEL "+harness.ImageLabel+"=") || strings.Contains(built, "LABEL "+harness.ImageLayerLabelPrefix)
		if spec.metadataArg == "" {
			if declares {
				t.Errorf("%s builds a manifest label but its spec passes no metadata to fill it", spec.name)
			}
			continue
		}
		labeled[spec.name] = spec.metadataArg
		// A runnable image labels the manifest in its own stage, not only in
		// one it builds: its digest, which a sandbox pins, has to move with
		// every edit to the manifest the server composes, and a label in a
		// stage it merely copies from never reaches its config.
		if !spec.bundle {
			own := builtStages(onlyStage(dockerfile, spec.target), "")
			if !strings.Contains(own, "LABEL "+harness.ImageLabel+"=") && !strings.Contains(own, "LABEL "+harness.ImageLayerLabelPrefix) {
				t.Errorf("%s is runnable but its %q stage labels no manifest, so its digest does not move with the manifest", spec.name, spec.target)
			}
		}
		if !strings.Contains(dockerfile, "ARG "+spec.metadataArg+"\n") {
			t.Errorf("%s is built with %s but %s declares no such ARG", spec.name, spec.metadataArg, spec.dockerfile)
		}
		if spec.metadataFile == "" {
			t.Errorf("%s passes %s but its spec has no image.json to fill it from", spec.name, spec.metadataArg)
			continue
		}
		if !contains(spec.buildArgs, spec.metadataArg+"=") {
			t.Errorf("%s has no %s= placeholder for buildImage to fill in: %#v", spec.name, spec.metadataArg, spec.buildArgs)
		}
	}
	// The platform's layer, on the bundle and on the runnable image whose
	// digest moves with it, and every harness's own, `shell` included, on its
	// bundle and its twin.
	want := map[string]string{
		platformSpecName:     harness.LayerMetadataBuildArg,
		sandboxAgentSpecName: harness.LayerMetadataBuildArg,
	}
	for _, harnessImage := range harnessImages {
		want["harness-"+harnessImage.name] = harness.MetadataBuildArg
		want["harness-"+harnessImage.name+"-image"] = harness.MetadataBuildArg
	}
	if !maps.Equal(labeled, want) {
		t.Fatalf("labeled images = %v, want %v", labeled, want)
	}
}

// onlyStage returns the named stage of dockerfile, its FROM line included, as
// a Dockerfile of its own.
func onlyStage(dockerfile, name string) string {
	var out []string
	in := false
	for _, line := range strings.Split(dockerfile, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[0], "FROM") {
			in = len(fields) >= 4 && strings.EqualFold(fields[len(fields)-2], "AS") && fields[len(fields)-1] == name
		}
		if in {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// builtStages returns the text of every stage building target means building
// (the last stage when target is empty): the stage itself, and every stage it
// is built FROM or copies or mounts from, transitively.
func builtStages(dockerfile, target string) string {
	type stage struct {
		name, from string
		lines      []string
	}
	var stages []*stage
	for _, line := range strings.Split(dockerfile, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[0], "FROM") {
			current := &stage{}
			for i, field := range fields[1:] {
				if strings.HasPrefix(field, "--") {
					continue
				}
				if current.from == "" {
					current.from = field
				}
				if strings.EqualFold(field, "AS") && i+2 < len(fields) {
					current.name = fields[i+2]
				}
			}
			stages = append(stages, current)
			continue
		}
		if len(stages) > 0 {
			stages[len(stages)-1].lines = append(stages[len(stages)-1].lines, line)
		}
	}
	if len(stages) == 0 {
		return ""
	}
	byName := map[string]*stage{}
	for _, s := range stages {
		if s.name != "" {
			byName[s.name] = s
		}
	}
	start := stages[len(stages)-1]
	if target != "" {
		start = byName[target]
		if start == nil {
			return ""
		}
	}
	var out strings.Builder
	seen := map[*stage]bool{}
	var visit func(*stage)
	visit = func(s *stage) {
		if s == nil || seen[s] {
			return
		}
		seen[s] = true
		out.WriteString(strings.Join(s.lines, "\n"))
		out.WriteString("\n")
		visit(byName[s.from])
		for _, line := range s.lines {
			for _, field := range strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == ',' }) {
				if from, ok := strings.CutPrefix(field, "--from="); ok {
					visit(byName[from])
				} else if from, ok := strings.CutPrefix(field, "from="); ok {
					visit(byName[from])
				}
			}
		}
	}
	visit(start)
	return out.String()
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
			"--build-arg", "BASE_IMAGE=discobox-sandbox-base:local",
			"-t", "discobox-sandbox-agent:local", "."},
	}
	got, err := renderBuildArgs(spec, "discobox-sandbox-base:dev-0123456789ab")
	if err != nil {
		t.Fatal(err)
	}
	if !contains(got, "BASE_IMAGE=discobox-sandbox-base:dev-0123456789ab") {
		t.Fatalf("parent reference not pinned: %#v", got)
	}
	if contains(got, "BASE_IMAGE=discobox-sandbox-base:local") {
		t.Fatalf("mutable base tag survived: %#v", got)
	}
	if contains(spec.buildArgs, "BASE_IMAGE=discobox-sandbox-base:dev-0123456789ab") {
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
// build:sandbox-base-image, the Dockerfile hook — fails the child's build with a
// registry error rather than a missing-image one.
func TestResolveParentImageTagsTheBaseItResolves(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script on PATH")
	}
	calls := filepath.Join(t.TempDir(), "calls")
	repoRoot := fakeDockerPath(t, "sha256:c2afa1b19563247d43a4b02802287dc907cd62e04039f0652488f45a7d07b9c9", calls)
	specs := []imageSpec{{name: baseSpecName, baseImage: "discobox-sandbox-base:local", devPrefix: "discobox-sandbox-base:dev-"}}

	got, err := resolveParentImage(t.Context(), repoRoot, specs, baseSpecName)
	if err != nil {
		t.Fatalf("resolve parent image: %v", err)
	}
	if want := "discobox-sandbox-base:dev-c2afa1b19563"; got != want {
		t.Fatalf("resolved = %q, want %q", got, want)
	}
	if want := "tag discobox-sandbox-base:local discobox-sandbox-base:dev-c2afa1b19563"; !strings.Contains(readFile(t, calls), want) {
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
