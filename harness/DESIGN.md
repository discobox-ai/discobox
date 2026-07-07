# Harness Design

This package owns coding-agent hook registration for sandbox terminals.

## Driver Model

- `harness.Driver` wires one agent provider's hook integration via
  `InstallHooks` (writes managed hook config) and describes that agent's built-in
  config via `Definition()` (install/run/relaunch argv and seed files). All
  agent-specific defaults live with the driver, never in the control plane.
- `InstallHooks` (hook wiring) is unrelated to `Definition.InstallCommand`, which
  is the argv that installs the agent CLI itself.
- Provider-specific implementations live in one folder per agent:
  - `claude-code`
  - `codex-cli`
  - `opencode`
- `registry` selects the driver from the terminal's configured agent ID or
  command, can install all drivers for image/bootstrap workflows, and exposes
  `Definitions()` for the control plane to surface built-in agent configs.

## Managed Layers

Prefer managed or system-owned configuration layers so hook capture is not
subject to repo trust prompts or user/project override:

- Claude Code: `/etc/claude-code/managed-settings.json` on Linux/WSL.
- Codex CLI: `/.codex/hooks.json` as a system hook layer. System hooks are
  treated as managed and trusted by policy.
- opencode: `/etc/opencode/opencode.json` as managed config, plus
  `/etc/opencode/plugins/` with `OPENCODE_CONFIG_DIR=/etc/opencode` for the
  launched terminal so the root-owned plugin directory is loaded.

Drivers must be idempotent and preserve unrelated settings where the agent uses
a single shared JSON object.
