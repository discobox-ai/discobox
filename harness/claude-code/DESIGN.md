# Claude Code harness

Anthropic's Claude Code CLI. Its declaration is [`harness.json`](harness.json) and its vendor
operations are [`adapter.ts`](adapter.ts); what every built-in shares, and
when each part runs, is [`../DESIGN.md`](../DESIGN.md#built-in-harness-shape).
This file is what is particular to this vendor.

| Identity | |
| --- | --- |
| Directory | `claude-code` |
| Harness ID and built-in config slug | `claude-code` |
| Hook provider | `claude-code` |
| Bundle | `discobox-harness-claude-code` |
| Package and command | `@anthropic-ai/claude-code`, `claude` |

## What it declares

- **Launch**: the prompt as one positional; a relaunch is `--continue`.
- **Environment**: telemetry, error reports, `/feedback` and the updater off,
  `FORCE_HYPERLINK=1`, and `CLAUDE_CODE_TMPDIR` on a `data` volume (see
  [`../DESIGN.md`](../DESIGN.md#image-contract) and [source-scoped
  memory](../DESIGN.md#source-scoped-memory)).
- **Settings**: the seed `.claude/settings.json` (dark theme, bypass
  permissions, claude.ai connectors off); no policy asset: the managed layer is the generated hooks
  file.
- **Local state**: `local-state` trust (onboarding, the auto-mode nudge, the
  working directory's trust, in `.claude.json`); `managed-setting` memory, the
  drop-in `/etc/claude-code/managed-settings.d/discobox-memory.json`.
- **Credentials**: `ANTHROPIC_API_KEY` and `CLAUDE_CODE_OAUTH_TOKEN`,
  file-delivered, with routes and a rejection scope; a `file` store,
  `.claude/.credentials.json`.
- **Hooks**: `command`, Claude Code's format, at
  `/etc/claude-code/managed-settings.json`, every documented event.

## Adapter operations

- `entries`, `material`, `login` (the renewable subscription login, `oauth`),
  `files`, `compose`, `apply`.
- `moreEntries`, `documentEntries`, `secondDocument`, `wanted`,
  `reconsidered`: the API key is a second document, `.claude.json`, which also
  holds the box's trust map and is never exported as a file.
- `signedOut`: a signed-out credentials file is no login to keep.
- `prepareLocalState`: the trust and onboarding merge, and the API key's
  sentinel, preserving a newer local login.
- `oauth` and `credentialsFile` are also what `scripts/nested-claude-auth.sh`
  builds a nested login from.

## Credentials and settings

`/login` inside a box writes one of two credentials, both read by the export:

- `~/.claude/.credentials.json` — a subscription login's rotating OAuth blob
  (access token + **refresh token** + expiry), saved as an `oauth` secret
  (`CLAUDE_CODE_OAUTH_TOKEN`) with the fixed Anthropic `tokenUrl`/`clientId`
  from the provider definition, so the control plane refreshes the access
  token as it expires (see `server/internal/resources/harnessconfigs/DESIGN.md` → OAuth
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

- The launcher's preparation (`localState.trust: local-state`, the adapter's
  `prepareLocalState`, under the capture lock) merges onboarding, the
  auto-mode nudge marker, and trust for the actual current directory into
  `.claude.json`, preserving other projects, preferences and newer local
  credentials. Malformed state is left intact and startup fails.
- `.claude/settings.json` is exported as the user left it, and remains
  literal. The bundle defaults to bypass-permission mode and disables
  claude.ai connectors with `disableClaudeAiConnectors: true`. A saved or
  explicitly configured settings file takes precedence over the bundle default.
  The OAuth route to `mcp-proxy.anthropic.com` remains available when connectors
  are enabled. `~/.claude.json` carries the credential and per-workspace trust
  map and stays local to the box.
- Both credentials declare `rejectionScope`: `api.anthropic.com`,
  `/v1/messages`. A
  secret's `rejectionScope` (`harness.RejectionRule`: a host, and path prefixes
  on a segment boundary) names the requests that answer for the credential;
  only what an upstream makes of one of them draws or clears the refused
  credential's band. Anthropic's runner poll refuses a subscription login that
  works and its event logging accepts anything, so neither is evidence. A
  harness whose API answers the same everywhere declares none
  ([ADR 26-10-13-595](../../docs/adr/26-10-13-595-a-harness-names-the-requests-whose-refusal-means-its-login-is-dead.md)).

## Hooks and policy layer

`/etc/claude-code/managed-settings.json` on Linux/WSL.
  The image bakes this file beside the Claude Code binary, making the hook
  definition an image-owned compatibility unit, generated from the
  declared events.
