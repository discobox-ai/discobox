package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"dagger/pipeline/internal/dagger"
)

// GitHub's CLI is an API client, never a workflow dispatcher.
func (m *Pipeline) github(token *dagger.Secret) *dagger.Container {
	return m.work().WithSecretVariable("GH_TOKEN", token).
		WithEnvVariable("GH_REPO", m.Repository)
}
func (m *Pipeline) publishable(ctx context.Context, version string, token *dagger.Secret) error {
	if err := validateVersion(version); err != nil {
		return err
	}
	if m.Dirty {
		return fmt.Errorf("publication requires a clean source tree")
	}
	if !revisionPattern.MatchString(m.Revision) {
		return fmt.Errorf("publication requires a Git commit revision")
	}
	nonce, err := dag.CurrentTimestamp(ctx)
	if err != nil {
		return err
	}
	got, err := m.github(token).WithEnvVariable("PUBLICATION_ATTEMPT", nonce).WithExec([]string{"gh", "api", "repos/" + m.Repository + "/commits/" + version, "--jq", ".sha"}).Stdout(ctx)
	if err != nil {
		return err
	}
	if strings.TrimSpace(got) != m.Revision {
		return fmt.Errorf("release tag %s does not point at source revision %s", version, m.Revision)
	}
	return nil
}

// Publish an assembled prerelease. Missing or unverified native artifacts cannot pass assembly.
// Publication checks external state each invocation instead of caching gh writes.
func (m *Pipeline) ReleasePublish(ctx context.Context, version string, artifacts *dagger.Directory, token *dagger.Secret) (string, error) {
	assembled, err := m.ReleaseAssemble(ctx, version, artifacts)
	if err != nil {
		return "", err
	}
	if err := m.publishable(ctx, version, token); err != nil {
		return "", err
	}
	// A fresh invocation id keeps state-changing CLI operations out of exec cache.
	nonce, err := dag.CurrentTimestamp(ctx)
	if err != nil {
		return "", err
	}
	ctr := m.github(token).WithEnvVariable("PUBLICATION_ATTEMPT", nonce).WithDirectory("/artifacts", assembled).
		WithEnvVariable("RELEASE_VERSION", version).
		WithFile("/pipeline/release-publish.sh", dag.CurrentModule().Source().File("release-publish.sh")).
		WithExec([]string{"bash", "/pipeline/release-publish.sh"})
	return ctr.Stdout(ctx)
}

// Recompute both Homebrew channels from published releases and update the tap directly.
func (m *Pipeline) BrewRefresh(ctx context.Context, token *dagger.Secret,
	// +default="discobox-ai/homebrew-tap"
	tap string,
) (string, error) {
	nonce, err := dag.CurrentTimestamp(ctx)
	if err != nil {
		return "", err
	}
	ctr := m.github(token).WithEnvVariable("PUBLICATION_ATTEMPT", nonce).WithEnvVariable("TAP_REPOSITORY", tap).
		WithFile("/pipeline/brew-refresh.sh", dag.CurrentModule().Source().File("brew-refresh.sh"))
	return ctr.WithExec([]string{"bash", "/pipeline/brew-refresh.sh"}).Stdout(ctx)
}

// Explicit stable promotion. Does not submit winget while automatic submission is disabled.
func (m *Pipeline) Promote(ctx context.Context, version string, token, tapToken *dagger.Secret) (string, error) {
	if !dotVersion.MatchString(version) {
		return "", fmt.Errorf("only vMAJOR.MINOR.PATCH may be promoted")
	}
	nonce, err := dag.CurrentTimestamp(ctx)
	if err != nil {
		return "", err
	}
	client := m.github(token).WithEnvVariable("PUBLICATION_ATTEMPT", nonce)
	state, err := client.WithExec([]string{"gh", "release", "view", version, "--json", "isPrerelease,isDraft,tagName"}).Stdout(ctx)
	if err != nil {
		return "", err
	}
	edit, err := promotionRequired(version, state)
	if err != nil {
		return "", err
	}
	if edit {
		if err := run(ctx, client, "gh", "release", "edit", version, "--prerelease=false", "--latest"); err != nil {
			return "", err
		}
	}
	return m.BrewRefresh(ctx, tapToken, "discobox-ai/homebrew-tap")
}

func promotionRequired(version, state string) (bool, error) {
	if !dotVersion.MatchString(version) {
		return false, fmt.Errorf("only vMAJOR.MINOR.PATCH may be promoted")
	}
	var release struct {
		IsPrerelease bool
		IsDraft      bool
		TagName      string
	}
	if err := json.Unmarshal([]byte(state), &release); err != nil {
		return false, err
	}
	if release.IsDraft || release.TagName != version {
		return false, fmt.Errorf("promotion requires the published release %s", version)
	}
	return release.IsPrerelease, nil
}

// Produce winget manifests from the actual published Windows archives.
func (m *Pipeline) WingetManifests(ctx context.Context, version string, token *dagger.Secret) (*dagger.Directory, error) {
	if err := validateVersion(version); err != nil {
		return nil, err
	}
	nonce, err := dag.CurrentTimestamp(ctx)
	if err != nil {
		return nil, err
	}
	ctr := m.github(token).WithEnvVariable("PUBLICATION_ATTEMPT", nonce).WithExec([]string{"scripts/winget-manifests.sh", version, m.Repository, "/winget"})
	if _, err := ctr.Sync(ctx); err != nil {
		return nil, err
	}
	return ctr.Directory("/winget"), nil
}

// Manually open/update the winget submission. Never called by release or promotion.
func (m *Pipeline) WingetPublish(ctx context.Context, version string, token *dagger.Secret,
	// +default=false
	allowPrerelease bool,
) (string, error) {
	if !dotVersion.MatchString(version) && !allowPrerelease {
		return "", fmt.Errorf("winget serves one channel; explicit prereleases require --allow-prerelease")
	}
	manifests, err := m.WingetManifests(ctx, version, token)
	if err != nil {
		return "", err
	}
	nonce, err := dag.CurrentTimestamp(ctx)
	if err != nil {
		return "", err
	}
	return m.github(token).WithEnvVariable("PUBLICATION_ATTEMPT", nonce).WithEnvVariable("RELEASE_VERSION", version).
		WithDirectory("/winget", manifests).WithFile("/pipeline/winget-publish.sh", dag.CurrentModule().Source().File("winget-publish.sh")).
		WithExec([]string{"bash", "/pipeline/winget-publish.sh"}).Stdout(ctx)
}

// End-to-end release rehearsal: real Linux/Windows builds, optional real Mac artifacts,
// optional real OCI builds, and an inventory of the publication steps. No writes.
func (m *Pipeline) Preview(ctx context.Context,
	// +default="v0.0.0-preview"
	version string,
	// +default=false
	images bool,
	// Optional real macOS output from the native build command.
	// +optional
	nativeArtifacts *dagger.Directory,
) (*dagger.Directory, error) {
	out, err := m.ReleaseBuild(ctx, version, "all", "all")
	if err != nil {
		return nil, err
	}
	var assembled *dagger.Directory
	if nativeArtifacts != nil {
		out, err = m.mergeNative(ctx, version, out, nativeArtifacts)
		if err != nil {
			return nil, err
		}
		assembled, err = m.ReleaseAssemble(ctx, version, out)
	} else {
		assembled, err = m.assembleAvailable(ctx, version, out)
	}
	if err != nil {
		return nil, err
	}
	out = out.WithDirectory(".", assembled)
	if images {
		built, err := m.ImagesBuild(ctx, "linux/amd64")
		if err != nil {
			return nil, err
		}
		out = out.WithDirectory("oci", built)
	}
	report := map[string]any{"version": version, "revision": m.Revision, "repository": m.Repository, "publishable": false, "dirty": m.Dirty, "incompletePlatforms": []string{"darwin/arm64"}, "reason": "macOS artifacts were not supplied; build them with the native command", "native": []nativeReport{native("darwin/arm64"), native("windows/amd64")}, "publication": []string{"validate all platform receipts and manifests", "stamp installers and SHA256SUMS", "publish OCI version tags", "upload GitHub prerelease assets", "move latest tags for dot releases", "recompute Homebrew dev formula"}, "promotion": []string{"explicitly mark a dot release stable", "recompute Homebrew stable formula", "winget remains manual"}}
	if nativeArtifacts != nil {
		report["incompletePlatforms"] = []string{}
		report["reason"] = "complete build rehearsal; publication disabled"
	}
	data, _ := json.MarshalIndent(report, "", "  ")
	return out.WithNewFile("preview.json", string(data)+"\n"), nil
}

// Full release entry point: checks, builds, native boundary, assembly, images,
// GitHub prerelease, latest aliases, and Homebrew. Fails before side effects
// without complete real macOS artifacts from the native command.
func (m *Pipeline) Release(ctx context.Context, version, username string, nativeArtifacts *dagger.Directory, registryPassword, token, tapToken *dagger.Secret) (string, error) {
	if err := m.publishable(ctx, version, token); err != nil {
		return "", err
	}
	if _, err := m.Ci(ctx); err != nil {
		return "", err
	}
	// Ci builds untagged smoke artifacts. Release builds carry the actual version.
	artifacts, err := m.ReleaseBuild(ctx, version, "all", "all")
	if err != nil {
		return "", err
	}
	artifacts, err = m.mergeNative(ctx, version, artifacts, nativeArtifacts)
	if err != nil {
		return "", err
	}
	if _, err := m.ReleaseAssemble(ctx, version, artifacts); err != nil {
		return "", err
	}
	if _, err := m.ImagesPublish(ctx, version, m.registry(), username, registryPassword); err != nil {
		return "", err
	}
	url, err := m.ReleasePublish(ctx, version, artifacts, token)
	if err != nil {
		return "", err
	}
	if dotVersion.MatchString(version) {
		if err := m.latest(ctx, version, username, registryPassword); err != nil {
			return "", err
		}
		if _, err := m.BrewRefresh(ctx, tapToken, "discobox-ai/homebrew-tap"); err != nil {
			return "", err
		}
	}
	return url, nil
}

func (m *Pipeline) latest(ctx context.Context, version, username string, password *dagger.Secret) error {
	// Reuse built containers when assigning aliases: native Publish is a registry
	// operation, and all versioned artifacts were successfully published first.
	for _, spec := range productImages() {
		amd, err := m.Image(ctx, spec.Name, "linux/amd64")
		if err != nil {
			return err
		}
		opts := dagger.ContainerPublishOpts{}
		if !spec.Neutral {
			arm, err := m.Image(ctx, spec.Name, "linux/arm64")
			if err != nil {
				return err
			}
			opts.PlatformVariants = []*dagger.Container{arm}
		}
		if _, err := amd.WithRegistryAuth("ghcr.io", username, password).Publish(ctx, m.registry()+"/discobox-"+spec.Name+":latest", opts); err != nil {
			return err
		}
	}
	return nil
}

// Generate one Homebrew formula from the release's downloaded assets.
func (m *Pipeline) BrewFormula(ctx context.Context, version string, token *dagger.Secret,
	// +default=false
	dev bool,
) (*dagger.File, error) {
	if err := validateVersion(version); err != nil {
		return nil, err
	}
	args := []string{"scripts/brew-formula.sh"}
	if dev {
		args = append(args, "--dev")
	}
	args = append(args, version, m.Repository, "/formula.rb")
	nonce, err := dag.CurrentTimestamp(ctx)
	if err != nil {
		return nil, err
	}
	ctr := m.github(token).WithEnvVariable("PUBLICATION_ATTEMPT", nonce).WithExec(args)
	if _, err := ctr.Sync(ctx); err != nil {
		return nil, err
	}
	return ctr.File("/formula.rb"), nil
}

// Explicitly override one Homebrew formula using a published release's assets.
// Automatic release/promotion always recomputes channels through BrewRefresh.
func (m *Pipeline) BrewPublish(ctx context.Context, version string, token *dagger.Secret,
	// +default=false
	dev bool,
	// +default=false
	allowPrerelease bool,
	// +default="discobox-ai/homebrew-tap"
	tap string,
) (string, error) {
	if !dotVersion.MatchString(version) && !allowPrerelease {
		return "", fmt.Errorf("Homebrew channels require a dot release unless --allow-prerelease is explicit")
	}
	formula, err := m.BrewFormula(ctx, version, token, dev)
	if err != nil {
		return "", err
	}
	name := "discobox"
	if dev {
		name = "discobox-dev"
	}
	nonce, err := dag.CurrentTimestamp(ctx)
	if err != nil {
		return "", err
	}
	return m.github(token).WithEnvVariable("PUBLICATION_ATTEMPT", nonce).WithEnvVariable("RELEASE_VERSION", version).WithEnvVariable("TAP_REPOSITORY", tap).WithEnvVariable("FORMULA_NAME", name).
		WithFile("/formula.rb", formula).WithFile("/pipeline/brew-publish.sh", dag.CurrentModule().Source().File("brew-publish.sh")).WithExec([]string{"bash", "/pipeline/brew-publish.sh"}).Stdout(ctx)
}
