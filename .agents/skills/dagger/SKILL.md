---
name: dagger
description: Use Dagger 1.0 (workspaces-era) conventions when running, configuring, or authoring Dagger pipelines. Use when working with dagger commands, dagger.toml, dagger-module.toml, dagger.json, Dagger modules/checks/generators/services, or setting up CI with Dagger.
---

# Dagger 1.0

Dagger changed significantly at 1.0 ("workspaces"). Training data and most online examples show the old 0.x style. **Always prefer the 1.0 conventions below.** When unsure about a command, run `dagger <cmd> --help` rather than guessing from memory.

## Version check (do this first)

```bash
dagger version
```

- If it reports `v1.x`: use everything below directly.
- If it reports `v0.21.3+` and the project should use 1.0: the beta is opt-in via
  `export DAGGER_X_RELEASE=v1.0.0-beta.6` (or per-command: `dagger --x-release=v1.0.0-beta.6 ...`).
  The stable CLI auto-downloads and runs the beta. Unset the var to return to stable.
  Check for a newer beta tag: `git ls-remote https://github.com/dagger/dagger 'refs/tags/v1.0.0-beta*'`.
- If it reports `v0.x` and the project has a legacy `dagger.json` it wants to keep: fall back to 0.x idioms (`dagger -m`, `dagger functions`), but flag to the user that the project is on the old style.

## Core model

- **Workspace** = the project using Dagger. Config lives in **`dagger.toml`** (committed, shared contract): installed modules, their names, settings, entrypoints, lock state.
- **Module** = a package of code. Its own metadata lives in **`dagger-module.toml`**.
- Old `dagger.json` did both jobs; it is legacy. Toolchains, blueprints, and customizations are gone as concepts:
  - toolchain → just a module installed in the workspace
  - blueprint → workspace module with `entrypoint = true` in `dagger.toml`
  - customizations → `[modules.<name>.settings]` in `dagger.toml`
- Three first-class function kinds a module exposes (marked `@check` / `@generate` / `@up` or the SDK equivalent):
  - **Checks** — validate (tests, lint, scans). Never modify files. Run via `dagger check`.
  - **Generators** — produce a **changeset** (a reviewable diff). Run via `dagger generate`.
  - **Services** — long-running deps. Run via `dagger up`.
- **Changesets**: commands that would modify files return a diff and prompt before applying. Use `-y` / `--auto-apply` in non-interactive/agent contexts.

## Everyday commands

```bash
dagger setup                    # scan project, recommend + install modules, create dagger.toml
dagger search <term>            # find modules (registry)
dagger install github.com/dagger/<mod>   # add module to dagger.toml (creates it if needed)
dagger uninstall <mod>
dagger installed                # list workspace modules

dagger check                    # run all checks in parallel; non-zero exit on failure
dagger check -l                 # list checks
dagger check eslint:* *:lint    # filter (module:check patterns)
dagger check --failfast         # stop on first failure
dagger check --no-generate      # skip generators-as-checks; --generate = only those

dagger generate                 # run generators; review + apply changesets (-y to auto-apply)
dagger generate -l

dagger up                       # start workspace services (ports tunneled locally)
dagger up -l / dagger up web api

dagger call <mod> <fn> [--arg=..] [chain...]   # call functions directly (= dagger api call)
dagger api functions [module]   # discover functions
dagger call <mod> --help        # per-function args

dagger settings                 # aliased as `dagger ws settings`; list module settings
dagger settings <mod> <key> <value>            # stored in dagger.toml
dagger --env staging check      # apply env overlay (env.<name>.* in dagger.toml)
dagger settings --env staging <mod> <key> <value>

dagger workspace init --here    # nested workspace; `ws` is the alias
dagger workspace migrate        # convert legacy dagger.json → dagger.toml
dagger update                   # update installed modules / lockfile
```

Cloud/CI: `dagger cloud login`, `dagger cloud check on` (enable PR checks for the repo), `dagger activity`, `dagger cloud analyze` / `dagger cloud rerun` for failed traces. In CI providers, the whole job is just `- run: dagger check` — CI systems are only "triggers".

## Old → new mapping (don't emit the old forms)

| 0.x (legacy) | 1.0 |
|---|---|
| `dagger.json` | `dagger.toml` (workspace) + `dagger-module.toml` (module) |
| `dagger -m <ref>` (toolchains) | `dagger -W <ref>` |
| `dagger toolchain install X` | `dagger install X` |
| `dagger install <dep>` (module code dep) | `[[dependencies]]` in `dagger-module.toml` (`dagger module deps add`) |
| `dagger functions` | `dagger api functions` |
| `blueprint` field | `entrypoint = true` under `[modules.<name>]` |
| `customizations` / `.env` constructor defaults | `[modules.<name>.settings]` |
| `dagger develop` / `dagger init` | SDK-module functions (`dagger module init`, SDK scaffold via changesets) |

## Authoring modules (pointers, not memory)

- Each SDK is itself a Dagger module (`github.com/dagger/go-sdk`, `dang-sdk`, typescript/python). Scaffold, codegen, and dep management are functions on the SDK module; the loop: scaffold → write functions → regenerate bindings → `dagger check`.
- **Dang** is Dagger's native DSL (no codegen/build step) — preferred for modules that mostly orchestrate containers/files/other modules. Go/TS/Python for real logic.
- **Learning/writing Dang** — the language's home repo is `vito/dang`; its `docs/lit/` tree and `docs/lit/reference/grammar.md` are the real syntax reference (the dagger docs' `extending/sdks/dang.mdx` covers module usage, not the language). Style guide: `DANG_MODULE_DEVELOPER_MANUAL.md` in `dagger/go`; worked examples: `go.dang` in `dagger/go`, `shellcheck.dang` in `dagger/shellcheck`. Syntax notes not obvious from examples: strings are `"..."`, `"""multiline"""` (no interpolation — safe for shell scripts with `${...}`), and backtick templates with `${expr}` interpolation (single-line; use a longer ```-fence for multiline templates); `pub`/`let` = public/private; the primary `type` is the module entry point; scalar types like `Platform` don't auto-coerce from `String` in function signatures — declare the scalar type.
- Design rules the docs push: name for caller intent; take typed Dagger objects (`Directory`, `Secret`, `Service`), not string paths/env vars; return rich objects (`Container`, `Directory`, `Changeset`); side effects explicit; expose at least one check/generator/service.
- Pin engine requirements with `dagger module engine require*`.

## Authoritative references

- `https://docs.dagger.io/llms.txt` — LLM-ready docs index (fetch subpages from it).
- Full CLI: `dagger --help` and `docs/current_docs/reference/cli/index.mdx` in the dagger repo.
- Local source checkout (this machine): `~/src/dagger` — fetch the latest `v1.0.0-beta.*` tag and read `docs/current_docs/` for current behavior; `docs/current_docs/reference/upgrade-to-workspaces.mdx` for migration details.
- BuildKit is gone (replaced by Dagger's own e-graph cache engine in v0.21); don't reason about Dagger in BuildKit terms.
