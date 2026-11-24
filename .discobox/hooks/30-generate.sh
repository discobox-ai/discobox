#!/bin/bash
#---
# name: Regenerate generated files
# type: file
# pattern: "{generate.go,pool-agent/generate.go,server/generate.go,cli/generate.go,api/openapi/server.yaml,api/gen/ogen.yml,api/internal/**,pool-agent/api/openapi/pool.yaml,pool-agent/api/gen/ogen.yml,pool-agent/api/internal/**,server/internal/config/**/*.go,cli/internal/clientconfig/**/*.go,pkg/sandboxconfig/**/*.go,pkg/configfile/**/*.go,pkg/installer/installer.go,cli/internal/tui/logo.json,internal/cmd/discobox-installer-logo/**,cli/internal/cmd/discobox-docs/**,harness/capture/settings-source.cjs,scripts/build-harness-settings.mjs,package.json,pnpm-lock.yaml}"
# ignore:
#   - "**/*_test.go"
#   - "**/testdata/**"
# notify_llm: false
#---
# The pattern is the inputs `go tool task generate` reads: the go:generate
# directives, the OpenAPI sources and ogen configs, and the generators with
# what they read (server and CLI config, sandbox settings, and pkg/configfile,
# which turns each into its schema and example; the installer logo; the
# harness settings parser, and the packages pnpm installs for it). It names no
# output (api/gen/*.go, api/sandboxgen, api/openapi/sandbox.yaml, derived from
# server.yaml, the *_gen.go aliases, the schemas and examples, settings.cjs,
# docs/public/cli.md), so a run does not set itself off; every generator also
# writes only a file whose bytes changed (pkg/genfile).
#
# Left out on purpose: cli/internal/cli, the command definitions
# docs/public/cli.md is generated from. Most CLI edits touch it and this target
# is slow, so a stale CLI reference is left to `task generate:verify` in CI.
# The install scripts are both read and rewritten by the logo stamp, so they
# are not inputs here either.
#
# test/repohooks fails when an entry of this pattern matches no tracked file,
# so moving an input listed here cannot silently drop it. A new input is not
# caught there: a change that adds one to what `task generate` reads adds it
# here too (REVIEW.md).

set -euo pipefail

go tool task generate
