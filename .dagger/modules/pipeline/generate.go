package main

import "dagger/pipeline/internal/dagger"

// Format application sources and return an applicable workspace diff.
func (m *Pipeline) Format() *dagger.Changeset {
	ctr := m.work().WithExec([]string{"bash", "-ec", `git ls-files -z '*.go' | xargs -0 gofmt -w`})
	return ctr.Directory("/src/discobox").WithoutDirectory(".git").WithoutDirectory("node_modules").Changes(m.Source)
}

// Tidy workspace modules in isolation and return an applicable workspace diff.
func (m *Pipeline) Tidy() *dagger.Changeset {
	ctr := m.work().WithExec([]string{"scripts/go-mod-tidy.sh"})
	return ctr.Directory("/src/discobox").WithoutDirectory(".git").WithoutDirectory("node_modules").Changes(m.Source)
}

// Regenerate application contracts and schemas and return an applicable diff.
func (m *Pipeline) Generate() *dagger.Changeset {
	ctr := m.work().WithExec([]string{"bash", "-ec", `pnpm install --frozen-lockfile; node scripts/build-harness-settings.mjs; mapfile -t modules < <(go work edit -json | jq -r '.Use[].DiskPath'); for mod in "${modules[@]}"; do (cd "$mod" && go generate ./...); done`})
	return ctr.Directory("/src/discobox").WithoutDirectory(".git").WithoutDirectory("node_modules").Changes(m.Source)
}
