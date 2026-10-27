package main

import "dagger/pipeline/internal/dagger"

// Format application sources and return an applicable workspace diff.
func (m *Pipeline) Format() *dagger.Changeset {
	ctr := m.work().WithExec([]string{"bash", "-ec", `git ls-files -z '*.go' | xargs -0 gofmt -w`})
	return ctr.Directory("/src/discobox").WithoutDirectory(".git").Changes(m.Source)
}

// Tidy workspace modules in isolation and return an applicable workspace diff.
func (m *Pipeline) Tidy() *dagger.Changeset {
	ctr := m.work().WithExec([]string{"scripts/go-mod-tidy.sh"})
	return ctr.Directory("/src/discobox").WithoutDirectory(".git").Changes(m.Source)
}

// Regenerate bundled harness settings parsers and licenses.
func (m *Pipeline) HarnessSettings() *dagger.Changeset {
	ctr := m.harnessSettingsContainer()
	return ctr.Directory("/src/discobox").WithoutDirectory(".git").WithoutDirectory("node_modules").Changes(m.Source)
}

func (m *Pipeline) harnessSettingsContainer() *dagger.Container {
	return m.work().WithMountedCache("/root/.local/share/pnpm/store", dag.CacheVolume("discobox-pnpm")).
		WithExec([]string{"pnpm", "install", "--frozen-lockfile"}).
		WithExec([]string{"node", "scripts/build-harness-settings.mjs"})
}

// Regenerate application contracts, schemas and bundled harness parsers.
func (m *Pipeline) Generate() *dagger.Changeset {
	ctr := m.harnessSettingsContainer().WithExec([]string{"bash", "-ec", `mapfile -t modules < <(go work edit -json | jq -r '.Use[].DiskPath'); for mod in "${modules[@]}"; do (cd "$mod" && go generate ./...); done`})
	return ctr.Directory("/src/discobox").WithoutDirectory(".git").WithoutDirectory("node_modules").Changes(m.Source)
}
