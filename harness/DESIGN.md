# Harness Design

This package owns the shared harness image contract and hook registration for
sandbox terminals.

## Image Contract

- One sandbox image contains at most one harness. Its immutable identity,
  run/relaunch commands, seed files, secret declarations, and optional config
  command live in `/usr/share/discobox/image.json`.
- The same non-secret harness object is published in the
  `io.discobox.harness.v1` OCI image label for server-side registration.
- Harness CLIs are installed at image build time. Runtime commands are never
  supplied by the server or worker-agent.
- `harnessMode: config` selects the image-owned interactive config command;
  normal or omitted mode selects the image-owned run/relaunch commands.

## Driver Model

- `harness.Driver` wires one harness provider's hook integration via
  `InstallHooks` and identifies its included image through `Definition()`. The
  public definition catalog is an image shortcut; runtime metadata comes from
  the registered image label and the copy inside that image.
- A `Definition` sets `Configure` to enable an ephemeral sandbox the CLI
  runs interactively after registering a `HarnessConfig`. The configure process
  writes files and collected secret values to
  `/run/discobox/harness-configure.json`; definitions without interactive setup
  leave it nil. Configure files use the same home-relative contract as all
  harness files; configure commands run from the sandbox workdir and must use
  `$HOME` when invoking one of those files. All three included harnesses support
  config mode: Codex collects an OpenAI API key, OpenCode collects one or both
  provider API keys, and Claude Code converts interactive credentials into
  encrypted secret values.
- Provider-specific implementations live in one folder per harness:
  - `claude-code`
  - `codex-cli`
  - `opencode`
- `registry` selects the driver from the image harness type ID, can install all
  drivers for hook/bootstrap workflows, and exposes
  `Definitions()` for the control plane to surface built-in harness configs.

## Managed Layers

Prefer managed or system-owned configuration layers so hook capture is not
subject to repo trust prompts or user/project override:

- Claude Code: `/etc/claude-code/managed-settings.json` on Linux/WSL.
- Codex CLI: `/.codex/hooks.json` as a system hook layer. System hooks are
  treated as managed and trusted by policy.
- opencode: `/etc/opencode/opencode.json` as managed config, plus
  `/etc/opencode/plugins/` with `OPENCODE_CONFIG_DIR=/etc/opencode` for the
  launched terminal so the root-owned plugin directory is loaded.

Drivers must be idempotent and preserve unrelated settings where the harness uses
a single shared JSON object.

## Configure flows

`claude-code/configure.sh` runs Claude Code's interactive onboarding, captures
its non-secret settings, converts credentials into secret values, and writes the
configure result contract. Credential files never become public harness files.
The CLI applies returned files and encrypted secret bindings only after the
configure terminal exits successfully.

`codex-cli/configure.sh` and `opencode/configure.sh` collect API keys without
echoing them and return the same secret result contract. No configure flow
stores credentials in a public harness file.
