# Claude Code Harness Design

The `claude-code` bundle: Claude Code on the shared harness contract. What every
harness shares (the bundle contract, managed layers, canonical hook names,
source-scoped memory, save-back) is in [`harness/DESIGN.md`](../DESIGN.md);
this is what is particular to Claude Code.

## Image

- **`egressHosts`** names the four hosts Claude Code's network documentation
  lists for the API, sign-in, token refresh, and claude.ai connectors.
  Plugins, the changelog, artifacts, and Chrome are optional and left to a
  level a person sets.
- **Claude Code sends no telemetry.** Its env layer sets `DISABLE_TELEMETRY`,
  `DISABLE_ERROR_REPORTING` and `DISABLE_FEEDBACK_COMMAND` for every discobox
  on it, restricted or not: telemetry, error reports, and `/feedback` (`/bug`)
  are off, from an unrestricted discobox too. Its other background calls are
  not held back (pull request status through `gh`, quota probes, plugin and
  MCP directory lookups); a restricted discobox's proxy refuses those its
  egress does not allow. Not `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`, which
  holds those back as well: it also stops Claude Code titling the session, and
  that title is what names a box while its agent is idle (ADR 26-10-31-618).
- **The updater is off**: `DISABLE_AUTOUPDATER` in the env layer, since the
  agent version store is the version source.
- **`FORCE_HYPERLINK=1`.** An exec the console opens is given
  `TERM=xterm-256color` and nothing that says its terminal takes OSC 8
  hyperlinks, which Claude Code asks for before it writes one, so the env
  layer sets it and its markdown links are written as links, which `termpane`
  draws. The env layer reaches every exec in a claude-code discobox, and what
  `claude` runs from them: anything else following the `supports-hyperlinks`
  convention reads the variable too, ahead of asking whether it writes to a
  terminal, and may put OSC 8 into a pipe. That cost is kept to the one image
  whose agent needs the hint, rather than made every exec's default.

## Hooks

The managed layer is `/etc/claude-code/managed-settings.json` on Linux/WSL.
The image bakes this file beside the Claude Code binary, making the hook
definition an image-owned compatibility unit; the Go driver does not
construct or merge Claude Code's hook format.

Claude Code's hook vocabulary is the canonical one, so `claude-code` is absent
from `CanonicalHookEvent`'s table and passes through (see
[Canonical hook event names](../DESIGN.md#canonical-hook-event-names)).

## Memory and temp tree

`autoMemoryDirectory` is `.../users/<uid>/harnesses/claude-code/memories`
under the source-data mount (see
[Source-scoped memory](../DESIGN.md#source-scoped-memory)). A path naming the
uid cannot be baked into the image's `/etc/claude-code/managed-settings.json`,
so the launcher records it as the managed drop-in
`/etc/claude-code/managed-settings.d/discobox-memory.json` before starting
`claude`. That makes it the sandbox's environment rather than a launch flag:
the harness terminal launches at sandbox start, and a `claude` typed into
any shell afterwards finds the same memory. The policy layer also keeps it
out of `.claude/settings.json`, which a saved harness change replaces with
the user's own settings, and outranks anything that file says.

Claude Code's temp tree — session scratchpads among it — is environmental
too: the image's env sets `CLAUDE_CODE_TMPDIR=/var/lib/claude-code` and
declares that path a `data` volume owned by the sandbox user, so scratchpads
survive an upgrade and travel with an export. Claude Code adds `claude-<uid>`
beneath it and refuses a directory another uid owns.

## Credentials

`/login` inside a box writes one of two credentials, both read by the export:

- `~/.claude/.credentials.json` — a subscription login's rotating OAuth blob
  (access token + **refresh token** + expiry), saved as an `oauth` secret
  (`CLAUDE_CODE_OAUTH_TOKEN`) with the fixed Anthropic `tokenUrl`/`clientId`
  from the provider definition, so the control plane refreshes the access
  token as it expires (see `resources/harnessconfigs/DESIGN.md` → OAuth
  secrets). Only `/login` yields a refresh token; `claude setup-token` does
  not. The blob's `scopes` and `subscriptionType` travel with it. They are not
  credentials — they say what the login may do — and Claude Code gates Remote
  Control on finding `user:profile` recorded beside the token, so they are
  kept rather than assumed.
- `primaryApiKey` in `~/.claude.json` — an Anthropic Console account's
  long-lived managed key, saved as a plain `token` secret
  (`ANTHROPIC_API_KEY`). A later sandbox gets its sentinel back **in the same
  field**, through a credential document whose `primaryApiKey` is an explicit
  secret reference. Startup also initializes this field from the current
  sentinel context when absent or carrying a known old alias; it preserves a
  newer local login.

Neither credential is exported as an environment variable. Both are declared
`delivery: file` (`harness.SecretDeliveryFile`), so the sentinel is minted and
rendered into the file the CLI reads, and the variable is withheld — a CLI
that finds both prefers the variable, and the variable carries none of the
metadata the file does. The delivered credentials file's expiry is far out,
so Claude Code never tries to rotate a sentinel.

- Both credentials declare `rejectionScope` (see
  [Credentials](../DESIGN.md#credentials)): `api.anthropic.com`,
  `/v1/messages`. Anthropic's runner poll refuses a subscription login that
  works and its event logging accepts anything, so neither is evidence
  ([ADR 26-10-13-595](../../docs/adr/26-10-13-595-a-harness-names-the-requests-whose-refusal-means-its-login-is-dead.md)).

## Launcher and save-back

- The launcher runs `claude-code-capture init`. It merges onboarding, the
  auto-mode nudge marker, and trust for the actual current directory into
  `.claude.json`, preserving other projects, preferences and newer local
  credentials. Malformed state is left intact and startup fails. The
  initializer takes the capture lock while merging, serializing with
  credential handoff; trust is never exported into shared settings.
- **A resume continues only a conversation there is.** `claude --continue`
  with none for its directory exits 1, leaving the login shell, and a primary
  relaunched before anyone sent a message has none. So the launcher continues
  only when a conversation file exists where Claude Code looks for this
  directory's (`launch.sh` says how it names them), and otherwise starts a
  fresh session with the prompt, which no conversation means was never sent.
  It never retries on exit 1, which would restart after real failures. Nor
  does it retry a failed model request at its start: Claude Code stays on its
  screen and retries the request itself.
- `.claude/settings.json` is exported as the user left it, and remains
  literal. The bundle defaults to bypass-permission mode and disables
  claude.ai connectors with `disableClaudeAiConnectors: true`. A saved or
  explicitly configured settings file takes precedence over the bundle default.
  The OAuth route to `mcp-proxy.anthropic.com` remains available when connectors
  are enabled. `~/.claude.json` carries the credential and per-workspace trust
  map and stays local to the box.
- Cleared OAuth tokens in an export represent sign-out and leave delivery
  restorable; a remaining refresh token stays protected.
