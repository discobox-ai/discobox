# Harness Design

This package owns harness hook registration for sandbox terminals.

## Driver Model

- `harness.Driver` wires one harness provider's hook integration via
  `InstallHooks` (writes managed hook config) and describes that harness's built-in
  config via `Definition()` (install/run/relaunch argv and seed files). All
  harness-specific defaults live with the driver, never in the control plane.
- A `Definition` may set `Configure` to describe an ephemeral sandbox the CLI
  runs interactively before creating a `HarnessConfig`. The configure process
  writes files and collected secret values to
  `/run/discobox/harness-configure.json`; definitions without interactive setup
  leave it nil. Configure files use the same home-relative contract as all
  harness files; configure commands run from the sandbox workdir and must use
  `$HOME` when invoking one of those files.
- `InstallHooks` (hook wiring) is unrelated to `Definition.InstallCommand`, which
  is the argv that installs the harness CLI itself.
- Provider-specific implementations live in one folder per harness:
  - `claude-code`
  - `codex-cli`
  - `opencode`
- `registry` selects the driver from the terminal's configured harness ID or
  command, can install all drivers for image/bootstrap workflows, and exposes
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

## Claude Code configure flow

`claude-code/configure.sh` runs Claude Code's interactive onboarding, captures
its settings and credentials, and writes the configure result contract. The
CLI owns the ephemeral sandbox lifecycle and applies the returned files and
secret bindings only after the configure terminal exits successfully.
