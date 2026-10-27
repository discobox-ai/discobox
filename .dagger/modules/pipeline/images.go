package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"dagger/pipeline/internal/dagger"
)

type imageSpec struct {
	Name, Context, Dockerfile, Target, Metadata, MetadataArg string
	Bundle, Neutral                                          bool
}

func productImages() []imageSpec {
	images := []imageSpec{
		{Name: "sandbox-base", Context: "server/providers/dockerworker/sandbox-image", Dockerfile: "Dockerfile"},
		{Name: "pool-agent-bundle", Context: ".", Dockerfile: "pool-agent/Dockerfile", Target: "bundle", Bundle: true},
		{Name: "agent", Context: ".", Dockerfile: "sandbox-agent/Dockerfile", Target: "agent", Bundle: true},
		{Name: "platform", Context: ".", Dockerfile: "sandbox-agent/Dockerfile", Target: "platform", Metadata: "sandbox-agent/image.json", MetadataArg: "IMAGE_LAYER_METADATA", Bundle: true},
	}
	for _, name := range []string{"claude-code", "codex", "opencode", "copilot", "omp", "pi", "shell"} {
		dir := name
		if name == "codex" {
			dir = "codex-cli"
		}
		images = append(images, imageSpec{Name: "harness-" + name, Context: "harness/" + dir, Dockerfile: "Dockerfile", Metadata: "harness/" + dir + "/image.json", MetadataArg: "HARNESS_METADATA", Bundle: true, Neutral: true})
	}
	return images
}
func imageByName(name string) (imageSpec, error) {
	for _, image := range productImages() {
		if image.Name == name {
			return image, nil
		}
	}
	return imageSpec{}, fmt.Errorf("unknown image %q", name)
}
func validatePlatform(platform string) error {
	if platform != "linux/amd64" && platform != "linux/arm64" {
		return fmt.Errorf("unsupported image platform %q", platform)
	}
	return nil
}

// Build a product image or bundle using its product-owned Dockerfile.
func (m *Pipeline) Image(ctx context.Context, name string,
	// +default="linux/amd64"
	platform string,
) (*dagger.Container, error) {
	if err := validatePlatform(platform); err != nil {
		return nil, err
	}
	spec, err := imageByName(name)
	if err != nil {
		return nil, err
	}
	if spec.Neutral {
		platform = "linux/amd64"
	}
	source := m.Source
	if spec.Context != "." {
		source = source.Directory(spec.Context)
	}
	args := []dagger.BuildArg{}
	epoch, err := m.bundleEpoch(ctx)
	if err != nil {
		return nil, err
	}
	if spec.Bundle {
		source = source.WithTimestamps(epoch)
		args = append(args, dagger.BuildArg{Name: "SOURCE_DATE_EPOCH", Value: strconv.Itoa(epoch)})
	}
	metadata := ""
	if spec.Metadata != "" {
		raw, err := m.Source.File(spec.Metadata).Contents(ctx)
		if err != nil {
			return nil, err
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, []byte(raw)); err != nil {
			return nil, err
		}
		metadata = compact.String()
		args = append(args, dagger.BuildArg{Name: spec.MetadataArg, Value: metadata})
	}
	if spec.Name == "sandbox-base" {
		if err := m.Emulation(ctx, platform); err != nil {
			return nil, err
		}
	}
	built := source.DockerBuild(dagger.DirectoryDockerBuildOpts{Dockerfile: spec.Dockerfile, Target: spec.Target, Platform: dagger.Platform(platform), BuildArgs: args})
	if !spec.Bundle {
		// The beta frontend retains ARG values in the final image environment.
		// This single-stage Dockerfile's build-only variables must not become
		// runtime configuration. Keep variables explicitly promoted with ENV.
		dockerfile, err := source.File(spec.Dockerfile).Contents(ctx)
		if err != nil {
			return nil, err
		}
		for _, name := range buildOnlyArgs(dockerfile) {
			built = built.WithoutEnvVariable(name)
		}
		return built, nil
	}
	// Recompose per-feature directories with deterministic file times. The
	// Dockerfile still owns the product's content; no runtime packages are
	// installed by this graph and neutral harnesses stay single manifests.
	bundle := built.WithRootfs(dag.Directory().WithNewDirectory("features", dagger.DirectoryWithNewDirectoryOpts{Permissions: 0755}).WithTimestamps(epoch))
	roots, err := built.Rootfs().Entries(ctx)
	if err != nil {
		return nil, err
	}
	for _, root := range roots {
		root = strings.TrimSuffix(root, "/")
		if root != "features" {
			return nil, fmt.Errorf("bundle %s contains unexpected root entry %s", name, root)
		}
		features, err := built.Directory("/features").Entries(ctx)
		if err != nil {
			return nil, err
		}
		for _, feature := range features {
			feature = strings.TrimSuffix(feature, "/")
			bundle = bundle.WithDirectory("/features/"+feature, built.Directory("/features/"+feature).WithTimestamps(epoch))
		}
	}
	labels, err := built.Labels(ctx)
	if err != nil {
		return nil, err
	}
	for _, label := range labels {
		key, err := label.Name(ctx)
		if err != nil {
			return nil, err
		}
		value, err := label.Value(ctx)
		if err != nil {
			return nil, err
		}
		bundle = bundle.WithLabel(key, value)
	}
	// COPY creates destination directories with current timestamps in this
	// engine beta. Normalize the exported layers, not just source files, while
	// retaining the per-feature layer boundaries and image configuration.
	archive := bundle.AsTarball()
	normalized := m.Toolchain().
		WithFile("/normalize.go", dag.CurrentModule().Source().File("cmd/normalize-oci/main.go")).
		WithExec([]string{"go", "build", "-o", "/normalize", "/normalize.go"}).
		WithFile("/input.tar", archive).
		WithExec([]string{"/normalize", "-input=/input.tar", "-output=/normalized.tar", fmt.Sprintf("-epoch=%d", epoch)})
	return dag.Container(dagger.ContainerOpts{Platform: dagger.Platform(platform)}).Import(normalized.File("/normalized.tar")), nil
}

func buildOnlyArgs(dockerfile string) []string {
	var args []string
	env := map[string]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(dockerfile, "\\\n", " "), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "ARG":
			args = append(args, strings.SplitN(fields[1], "=", 2)[0])
		case "ENV":
			env[strings.SplitN(fields[1], "=", 2)[0]] = true
			for _, field := range fields[2:] {
				if strings.Contains(field, "=") {
					env[strings.SplitN(field, "=", 2)[0]] = true
				}
			}
		}
	}
	var only []string
	for _, name := range args {
		if !env[name] {
			only = append(only, name)
		}
	}
	return only
}

// Build every image and export OCI archives; no registry credentials or writes.
func (m *Pipeline) ImagesBuild(ctx context.Context,
	// +default="linux/amd64"
	platform string,
) (*dagger.Directory, error) {
	if err := validatePlatform(platform); err != nil {
		return nil, err
	}
	images := productImages()
	files := make([]*dagger.File, len(images))
	jobs := make([]func() error, len(images))
	for i, spec := range images {
		jobs[i] = func() error {
			ctr, err := m.Image(ctx, spec.Name, platform)
			if err != nil {
				return err
			}
			files[i] = ctr.AsTarball()
			epoch, err := m.bundleEpoch(ctx)
			if err != nil {
				return err
			}
			checker := m.work().WithFile("/image.tar", files[i]).WithDirectory("/verify-oci", dag.CurrentModule().Source().Directory("cmd/verify-oci"))
			return run(ctx, checker, "go", "run", "/verify-oci/main.go", "-archive=/image.tar", fmt.Sprintf("-bundle=%t", spec.Bundle), fmt.Sprintf("-epoch=%d", epoch))
		}
	}
	if err := parallel(jobs...); err != nil {
		return nil, err
	}
	out := dag.Directory()
	for i, spec := range images {
		out = out.WithFile("discobox-"+spec.Name+".tar", files[i])
	}
	return out, nil
}

// Publish all product images to version tags. The registry and credentials are explicit.
func (m *Pipeline) ImagesPublish(ctx context.Context, version string, registry string, username string, password *dagger.Secret) (*dagger.File, error) {
	if m.Dirty {
		return nil, fmt.Errorf("image publication requires a clean source tree")
	}
	if err := validateVersion(version); err != nil {
		return nil, err
	}
	if registry == "" || strings.ContainsAny(registry, " \n\t") {
		return nil, fmt.Errorf("registry namespace is required")
	}
	refs := map[string]string{}
	for _, spec := range productImages() {
		amd, err := m.Image(ctx, spec.Name, "linux/amd64")
		if err != nil {
			return nil, err
		}
		opts := dagger.ContainerPublishOpts{}
		if !spec.Neutral {
			arm, err := m.Image(ctx, spec.Name, "linux/arm64")
			if err != nil {
				return nil, err
			}
			opts.PlatformVariants = []*dagger.Container{arm}
		}
		authHost := strings.Split(registry, "/")[0]
		ref, err := amd.WithRegistryAuth(authHost, username, password).Publish(ctx, registry+"/discobox-"+spec.Name+":"+version, opts)
		if err != nil {
			return nil, err
		}
		refs[spec.Name] = ref
	}
	data, _ := json.MarshalIndent(refs, "", "  ")
	return dag.Directory().WithNewFile("images.json", string(data)+"\n").File("images.json"), nil
}

// Build and validate the guest or libkrun kernel; their release lines stay separate.
func (m *Pipeline) VmBuild(ctx context.Context,
	// +default="guest"
	kind string,
	// +default="linux/amd64"
	platform string,
) (*dagger.Directory, error) {
	if err := validatePlatform(platform); err != nil {
		return nil, err
	}
	if err := m.Emulation(ctx, platform); err != nil {
		return nil, err
	}
	dockerfile, script := "vm-image/Dockerfile", "verify-vm.sh"
	if kind == "kernel" {
		if platform != "linux/amd64" {
			return nil, fmt.Errorf("libkrun kernel is amd64-only")
		}
		dockerfile = "vm-image/kernel/Dockerfile"
		script = "verify-kernel.sh"
	} else if kind != "guest" {
		return nil, fmt.Errorf("unknown VM artifact kind %q", kind)
	}
	var built *dagger.Container
	if kind == "kernel" {
		var err error
		built, err = m.kernel(ctx)
		if err != nil {
			return nil, err
		}
	} else {
		built = m.Source.DockerBuild(dagger.DirectoryDockerBuildOpts{Dockerfile: dockerfile, Platform: dagger.Platform(platform)})
	}
	ctr := m.Toolchain().WithDirectory("/artifacts", built.Rootfs()).WithFile("/verify.sh", dag.CurrentModule().Source().File(script)).WithExec([]string{"bash", "/verify.sh", platform})
	if _, err := ctr.Sync(ctx); err != nil {
		return nil, err
	}
	return built.Rootfs(), nil
}

// Verify both guest architectures, or the amd64-only kernel, without publishing.
func (m *Pipeline) VmCheck(ctx context.Context,
	// +default="guest"
	kind string,
) error {
	platforms := []string{"linux/amd64", "linux/arm64"}
	if kind == "kernel" {
		platforms = platforms[:1]
	}
	jobs := []func() error{}
	for _, platform := range platforms {
		jobs = append(jobs, func() error { _, err := m.VmBuild(ctx, kind, platform); return err })
	}
	return parallel(jobs...)
}

// Publish verified VM artifacts and return the digest to pin in the server.
func (m *Pipeline) VmPublish(ctx context.Context, kind, version, registry, username string, password *dagger.Secret) (string, error) {
	if m.Dirty {
		return "", fmt.Errorf("VM publication requires a clean source tree")
	}
	prefix := "vm/"
	if kind == "kernel" {
		prefix = "vm-kernel/"
	}
	version = strings.TrimPrefix(version, prefix)
	if err := validateVersion(version); err != nil {
		return "", err
	}
	amd, err := m.VmBuild(ctx, kind, "linux/amd64")
	if err != nil {
		return "", err
	}
	image := dag.Container(dagger.ContainerOpts{Platform: "linux/amd64"}).WithRootfs(amd)
	opts := dagger.ContainerPublishOpts{}
	name := "discobox-vm-kernel"
	if kind == "guest" {
		name = "discobox-vm"
		arm, err := m.VmBuild(ctx, kind, "linux/arm64")
		if err != nil {
			return "", err
		}
		opts.PlatformVariants = []*dagger.Container{dag.Container(dagger.ContainerOpts{Platform: "linux/arm64"}).WithRootfs(arm)}
	}
	return image.WithRegistryAuth(strings.Split(registry, "/")[0], username, password).Publish(ctx, registry+"/"+name+":"+version, opts)
}

// The beta Dockerfile frontend cannot enforce HTTP ADD checksums. Fetch the
// kernel's declared pinned inputs with native API files and verify them before
// unpacking. The product Dockerfile remains the source of version/digest pins.
func (m *Pipeline) kernel(ctx context.Context) (*dagger.Container, error) {
	recipe, err := m.Source.File("vm-image/kernel/Dockerfile").Contents(ctx)
	if err != nil {
		return nil, err
	}
	pattern := regexp.MustCompile(`(?s)ADD --checksum=sha256:([0-9a-f]{64})\s*\\\s*(https://[^\s]+)\s*\\\s*(/tmp/[^\s]+)`)
	inputs := pattern.FindAllStringSubmatch(recipe, -1)
	if len(inputs) != 2 {
		return nil, fmt.Errorf("kernel recipe must declare both checksummed source archives")
	}
	ctr := dag.Container(dagger.ContainerOpts{Platform: "linux/amd64"}).From("public.ecr.aws/docker/library/debian:13-slim").
		WithExec([]string{"bash", "-ec", "apt-get update && apt-get install -y --no-install-recommends bc binutils bison build-essential ca-certificates cpio flex libelf-dev libssl-dev patch python3 python3-pyelftools xz-utils && rm -rf /var/lib/apt/lists/*"})
	kernelVersion := ""
	for _, input := range inputs {
		ctr = ctr.WithFile(input[3], dag.HTTP(input[2], dagger.HTTPOpts{Checksum: "sha256:" + input[1]})).WithExec([]string{"bash", "-ec", `printf '%s  %s\n' "$1" "$2" | sha256sum -c -`, "verify-source", input[1], input[3]})
		if strings.HasSuffix(input[3], ".tar.xz") {
			kernelVersion = strings.TrimSuffix(path.Base(input[3]), ".tar.xz")
		}
	}
	if kernelVersion == "" {
		return nil, fmt.Errorf("kernel version missing from declared sources")
	}
	ctr = ctr.WithWorkdir("/src").WithEnvVariable("KERNEL_VERSION", kernelVersion).
		WithExec([]string{"bash", "-ec", `tar -xzf /tmp/libkrunfw.tar.gz --strip-components=1; mkdir -p tarballs; ln -s "/tmp/$KERNEL_VERSION.tar.xz" "tarballs/$KERNEL_VERSION.tar.xz"; make "$KERNEL_VERSION"`}).
		WithFile("/usr/local/bin/configure-discobox-kernel", m.Source.File("vm-image/kernel/configure-kernel"), dagger.ContainerWithFileOpts{Permissions: 0755}).
		WithExec([]string{"configure-discobox-kernel", kernelVersion}).
		WithExec([]string{"make", "-C", kernelVersion, "-j4", "KBUILD_BUILD_TIMESTAMP=Mon Jun 1 14:28:39 UTC 2026", "KBUILD_BUILD_USER=root", "KBUILD_BUILD_HOST=discobox", "vmlinux"})
	out := dag.Directory().WithFile("vmlinux", ctr.File("/src/"+kernelVersion+"/vmlinux")).WithFile("kernel.config", ctr.File("/src/"+kernelVersion+"/.config"))
	return dag.Container(dagger.ContainerOpts{Platform: "linux/amd64"}).WithRootfs(out), nil
}

// Consume the watcher/runtime's canonical epoch instead of keeping another pin.
func (m *Pipeline) bundleEpoch(ctx context.Context) (int, error) {
	text, err := m.Source.File("harness/imagelayer.go").Contents(ctx)
	if err != nil {
		return 0, err
	}
	match := regexp.MustCompile(`BundleSourceDateEpoch\s*=\s*"([0-9]+)"`).FindStringSubmatch(text)
	if len(match) != 2 {
		return 0, fmt.Errorf("harness.BundleSourceDateEpoch is not declared")
	}
	return strconv.Atoi(match[1])
}

// Register binfmt for foreign runtime builds. Native builds need no setup.
// The same trusted helper was used by the former buildx path; registration
// must also cover pre-exec hooks, which Dagger's per-command QEMU cannot wrap.
func (m *Pipeline) Emulation(ctx context.Context,
	// +default="linux/arm64"
	platform string,
) error {
	if err := validatePlatform(platform); err != nil {
		return err
	}
	native, err := dag.DefaultPlatform(ctx)
	if err != nil {
		return err
	}
	if string(native) == platform {
		return nil
	}
	arch := strings.TrimPrefix(platform, "linux/")
	nonce, err := dag.CurrentTimestamp(ctx)
	if err != nil {
		return err
	}
	installer := dag.Container().From("mirror.gcr.io/tonistiigi/binfmt:latest").WithEnvVariable("REGISTRATION_ATTEMPT", nonce)
	_, err = installer.WithExec([]string{"--install", arch}, dagger.ContainerWithExecOpts{UseEntrypoint: true, InsecureRootCapabilities: true}).Sync(ctx)
	return err
}
