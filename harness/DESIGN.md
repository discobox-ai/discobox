# Harness Design

This package owns the shared harness image contract and hook registration for
sandbox terminals.

## Image Contract

- One sandbox image contains at most one harness. Its immutable identity,
  run/relaunch commands, seed files, secret declarations, optional config
  command, env defaults, and declarative volumes are published in the
  `io.discobox.image.v1` OCI image label (`harness.ImageMetadata`) for
  server-side registration. There is no baked-in file inside the image
  carrying this data — `image.json` is the build-time authoring source the
  label is compacted from (see `Taskfile.yml`'s `build:harness-image`), not a
  runtime artifact.
- The label is snapshotted onto the harness config at registration and
  re-snapshotted when the image's config digest changes — by `SeedBuiltIns` for
  built-ins, by `RefreshHarnessConfigImage` for user-registered images. A
  snapshot is a cache of a mutable tag's current contents, not a permanent
  record (ADR 0016).
- Harness CLIs are installed at image build time. Runtime commands are never
  supplied by the server or pool-agent.
- Each provider folder owns its `Dockerfile`, `image.json`, configure script,
  and other image-specific assets. Harness images extend the sandbox-agent base
  selected by the `SANDBOX_AGENT_IMAGE` build argument, and must repeat any
  base env/volumes they still need (the harness's own `image.json` is the
  final image's sole metadata source; nothing merges with the base image's).
- `harnessMode: config` selects the image-owned interactive config command;
  normal or omitted mode selects the image-owned run/relaunch commands.

## Driver Model

- `harness.Driver` wires one harness provider's hook integration via
  `InstallHooks` and identifies its included image through `Definition()`. The
  public definition catalog is an image shortcut; runtime metadata comes from
  the registered image label and the copy inside that image.
- A `Definition` sets `Configure` to enable an ephemeral sandbox the CLI
  runs interactively after registering a `HarnessConfig`. The configure process
  writes files and collected secret values to `ConfigureOutputPath`; definitions
  without interactive setup leave it nil. Configure files use the same
  home-relative contract as all harness files; configure commands run from the
  sandbox workdir and must use `$HOME` when invoking one of those files. All
  three included harnesses support config mode — see
  [Configure flows](#configure-flows).
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

A configure command's contract is two fixed paths and one env prefix:

- It **writes** its result to `ConfigureOutputPath`
  (`/run/discobox/harness-configure.json`).
- It **may read** the previous run's output from `ConfigurePreviousConfigPath`
  (`/run/discobox/harness-previous-config.json`), seeded before the command
  starts. Same shape, but **no secret values** — it says which secrets exist, not
  what they are.
- Each of those secrets is offered as `ConfigurePreviousEnvPrefix` + its env
  name: a secret bound to `ANTHROPIC_API_KEY` arrives as
  `PREV_ANTHROPIC_API_KEY`.

**A credential never enters the sandbox.** `PREV_*` holds a sentinel that the
proxy swaps for the real value on an outbound request, and only while a live
grant covers it. So a configure command can *use* the old credential — that is
how it verifies one — without being able to read, print, or log it. Nothing
along this path decrypts a secret, and a revoked credential simply fails the
command's own verification.

The prefix is not decoration. Seeding `ANTHROPIC_API_KEY` itself would let the
harness CLI silently authenticate with the old credential: the flow would offer
a choice it had already made, and its verification would prove nothing about the
credential being configured.

Output is authoritative and replaces the previous configuration wholesale, so a
command that means to keep a file must re-emit it. To keep a *secret*, a command
returns it with `usePrevious: true` and no value; handing the `PREV_` sentinel
back as the value (`X=$PREV_X`) means the same thing, since storing a sentinel
as a credential would only configure the harness with something that resolves to
nothing.

The seed is therefore itself a valid output: writing it back unchanged is an
exact no-op reconfigure, which is the baseline a command edits rather than
rebuilds.

Every configure command should offer to keep an existing credential rather than
force a re-login, and should verify it first — it may have been revoked since.

The CLI applies returned files and encrypted secret bindings only after the
configure terminal exits successfully. No configure flow stores credentials in a
public harness file.

### Claude Code

`claude-code/configure.sh` collects exactly one Anthropic credential and returns
it as a secret:

- The user picks an API key (`ANTHROPIC_API_KEY`, a `bearer` secret), a Claude
  subscription login (`CLAUDE_CODE_OAUTH_TOKEN`, an `oauth` secret), or — when
  the seed lists a secret whose `PREV_` variable is set — the existing
  credential, which is kept with `usePrevious` and never handled by the script.
- The subscription path runs `claude /login` (equivalent to starting claude and
  typing `/login`), which writes the rotating OAuth blob (access token +
  **refresh token** + expiry) to `~/.claude/.credentials.json`. The script
  returns that whole blob plus the fixed Anthropic `tokenUrl`/`clientId` as an
  `oauth`-typed secret value, so the control plane can refresh the access token
  as it expires (see `resources/harnessconfigs/DESIGN.md` → OAuth secrets). It
  deliberately does **not** use `claude setup-token`: that mints a single
  long-lived token with no refresh token, which cannot rotate.
- The configure sandbox has no source, so the image's `.claude.json` template
  trusts no directory. The script first merges `hasTrustDialogAccepted` for the
  workspace into `~/.claude.json`, so `claude /login` (and the `claude -p`
  verification) run without stopping at the trust dialog. This touches only
  trust/onboarding, never a credential, and is not returned as a harness file.
- Every path ends in a `claude -p` check with only the chosen variable in the
  environment (and the credentials file moved aside), so a credential that
  cannot actually talk to the API never reaches a `HarnessConfig`. A failed
  check returns to the menu; the script exits non-zero rather than looping when
  stdin is closed, which fails the configure flow.
- It returns **no files**: credential files must never become public harness
  files, and Claude Code's non-secret settings (`.claude.json`,
  `.claude/settings.json`) belong to the image's declared harness files. A
  snapshot of the ephemeral configure sandbox would override them — including
  the per-sandbox trust template — for every later sandbox.

### Codex CLI

`codex-cli/configure.sh` collects one OpenAI API key without echoing it. When the
seed lists `OPENAI_API_KEY` and its `PREV_` variable is set, keeping the existing
key is the default choice. Every path ends in a `codex exec` check with the
chosen key in the environment; a failed check returns to the prompt. Nothing in
this flow performs a ChatGPT login, so there is no auth file to move aside as the
claude-code flow does.

### opencode

`opencode/configure.sh` settles the two providers independently — Anthropic and
OpenAI — and requires at least one. Each provider offers keep / replace / remove
when the seed lists it, and a skippable prompt when it does not. Because output
is authoritative, a provider left out is removed from the harness config.

Each key is checked alone, with the other provider's variable unset, so a key
cannot pass on the strength of the other one. The verification model is
discovered rather than hardcoded: `opencode models` lists only the providers
whose credential is present in the environment, so asking with just the one key
set both picks a model that exists in this image's opencode and proves the key is
wired to the expected provider.
