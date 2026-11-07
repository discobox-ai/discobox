# 26-11-06-578 — Antigravity uses a native bundle and server-renewed credentials

- **Status**: Proposed (would extend 0114 §§1, 7 beyond npm; preserves its pin, refresh and retention rules)
- **Date**: 2026-11-06
- **Issue**: [#744](https://github.com/discobox-ai/discobox/issues/744)
- **Related**: [0114](0114-a-sandbox-pins-its-agent-version-from-a-pool-cached-store.md),
  [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md).

## Context and evidence

Antigravity is a native terminal agent, `agy`. A Linux box has no desktop
keyring. Sharing its entire home directory would mix login, local trust,
conversations, installation state and preferences. Discobox instead saves a
reviewed harness change and delivers credential sentinels into subsequent boxes.

Repository facts checked at `00541f9c90fefeabe1b61134a737fab287861803`:

- `harness/registry/registry.go` registers six coding harnesses and shell.
  `harness/DESIGN.md` specifies provisioning bundles, signed-out first launch,
  literal settings and capture/handoff. There is no configure operation
  ([26-11-02-152](26-11-02-152-a-harness-is-set-up-in-the-box-it-runs-in-and-configure-is-removed.md)).
- `sandbox-agent/features/node/agent-store` implements package/version cache
  directories, npm discovery and installation, persistent pins and bounded
  background refresh. It cannot install this native distribution as declared.
- `pkg/harnesscapture/capture.go:OAuth` declares token URL, client ID and
  encoding, but no client secret. `server/internal/resources/harnessconfigs/capture.go`
  installs that registered metadata. `server/internal/resources/secrets/oauth.go`
  already supports form refresh with a client secret.
- `harness/credential-destinations.md` distinguishes spending routes from
  server refresh. Its existing multiprovider `google-antigravity` entry is
  evidence about other clients, not the new CLI's complete route table.
- `sandbox-agent/shimruntime` negotiates OSC 7501; `sandbox-agent/agentstatus`
  derives program state from it. Hooks and canonical event names belong to
  `harness/hookevent.go` and are a separate integration.

### Measurements on Linux amd64, 1.5.2

The official [release manifest](https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/linux_amd64.json)
served `1.5.2` and the following archive; its SHA-512 was independently checked
before running it in isolated temporary homes:

```text
https://storage.googleapis.com/antigravity-public/antigravity-cli/1.5.2-4951595792859136/linux-x64/cli_linux_x64.tar.gz
sha512: aa876c0813572efaf85ed47983cc937414705d40490cbdb54f6e255656a4f8744ae38e457823e924fa8270b348124e0521d3cda3fec6824bb2d14b98a7a05baa
```

The archive contains `antigravity`, exposed upstream as `agy`. Help provides
`--continue`, `--prompt-interactive`, `--dangerously-skip-permissions` and
session-only `--settings` (inline JSON or a file). A fresh interactive home
reached the login-method screen without a trust prompt before authentication.
That does not prove workspace trust after login.

With `SSH_CONNECTION` set, Google OAuth printed a URL and requested a pasted
code. Its redirect is `https://antigravity.google/oauth-callback`; no localhost
callback is needed for that flow. The log reported file storage because SSH
was detected. The authorization request included `cloud-platform`,
`userinfo.email`, `userinfo.profile`, `cclog`, `experimentsandconfigs`, `aicode`
and `openid` scopes.

A local TLS-terminating fake answered the token and Google API calls; no real
account or credential was used. The binary wrote this native file:

```text
~/.gemini/antigravity-cli/antigravity-oauth-token
```

```json
{
  "token": {
    "access_token": "fake-access-744",
    "token_type": "Bearer",
    "refresh_token": "fake-refresh-744",
    "expiry": "2026-11-06T18:36:47.837947751Z",
    "expires_in": 3600
  },
  "auth_method": "consumer"
}
```

Expiring that file and restarting produced `POST https://oauth2.googleapis.com/token`
with form fields `grant_type=refresh_token`, `refresh_token`, `client_id` and
`client_secret`. Both authorization and refresh used client ID
`1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com`.
The vendor-embedded client secret is client metadata, not a person's login;
its value is intentionally omitted here.

The fake observed bearer authentication at `GET www.googleapis.com/oauth2/v2/userinfo`
and at `daily-cloudcode-pa.googleapis.com` for `loadCodeAssist`,
`retrieveUserQuotaSummary`, `fetchUserInfo`, `fetchAvailableModels` and
`listExperiments` under `/v1internal:`. These establish startup and refresh
behavior, not successful inference or an exhaustive destination inventory.

After fake sign-in the TUI asked for a color scheme, then displayed terms/data
use with interaction collection checked by default. The probe stopped without
completing that screen. A login alone therefore does not meet unattended
first-launch requirements. The initial default run also spawned an updater;
`AGY_CLI_DISABLE_AUTO_UPDATE` exists in the binary and needs a behavioral
no-network control before relying on it.

### Documented, still requiring behavioral controls

Official [authentication documentation](https://antigravity.google/docs/install/)
provides API-key mode only with both `modelProvider: "gemini"` and
`GEMINI_API_KEY`; the environment variable alone has no effect.
[Settings](https://antigravity.google/docs/settings?tab=cli) live in
`~/.gemini/antigravity-cli/settings.json`; `trustedWorkspaces` records local
folders. Telemetry choice is account-backed rather than an ordinary preference
in that file.

[Hooks](https://antigravity.google/docs/hooks/) load from
`~/.gemini/config/hooks.json`, workspace `.agents/hooks.json`, or plugins;
hooks in CLI `settings.json` are ignored. Events are `SessionStart`,
`PreToolUse`, `PostToolUse`, `PreInvocation`, `PostInvocation`, and `Stop`.
No documented managed system hook layer has been established.
[Customization discovery](https://antigravity.google/docs/customization-discovery/)
includes workspace `AGENTS.md`/`GEMINI.md`, personal `~/.gemini/AGENTS.md` and
`~/.gemini/GEMINI.md`, and skills in standard `.agents`/Gemini directories.
These paths are also described by the binary's extracted built-in guidance.

## Decision proposed

### 1. One Linux harness, one native installation lifecycle

Use `antigravity` for the harness ID, folder, bundle suffix and hook provider;
its executable is `agy`. Publish one platform-neutral bundle with Linux amd64 and arm64 provisioning
selectors once each architecture's official artifact and runtime have been verified. A macOS or
Windows host still launches a Linux box and does not supply its local keyring,
installation or token files. Native non-Linux boxes are outside this decision.

The bundle carries an explicit release version and architecture-specific immutable
artifact URLs and digests. At authoring time verify each archive against its
upstream SHA-512 and record a SHA-256 for the provisioning download contract.
Provisioning selects and downloads the pinned artifact, verifies it, and installs
`agy`; it never executes the moving upstream installer. Check archive shape,
executable version and architecture. This preserves 26-10-16-423 §2: large
downloads are provisioning steps, not bundle contents. Initial provisioning can
require a download; a provisioned harness launch does not discover a version or
download an executable. Prove idempotence on restart.

Extend the existing agent store with an explicit distribution kind, `npm` or
native artifact, with each source's required metadata validated together.
Existing declarations keep their npm meaning. Native discovery/fetch is owned
by this store, not a second updater in the harness launcher, an npm wrapper,
or optional runtime interface. Move store ownership out of the Node feature
when implementing native support so Antigravity need not install Node just to
manage its executable. Its capture command may still declare Node if using the
existing JavaScript helpers.

Preserve 0114's per-box version pin, no foreground startup network dependency,
12-hour discovery window, lock, atomic staging/rename, explicit upgrade,
rollback handling, and newest-two plus recent-pin retention. Native cache
identity includes distribution, harness and Linux architecture so artifacts
cannot collide. The provisioned pinned executable is the offline fallback. Disable the vendor updater
in a layer outside portable settings, proven against the pinned binary.

### 2. Authentication uses explicit save-back, never a shared keyring

**Owner choice pending:** recommend supporting personal Google OAuth plus
Gemini API keys, subject to the gates below. The alternative is API-key-only
initial support with personal OAuth deferred. Acceptance must settle this
choice; neither the issue's request nor a route allowlist is consent to the
OAuth scope expansion. Enterprise/ADC/WIF, cloud profiles and plugin accounts
are outside automatic capture in either case.

For OAuth, use the CLI's native file-backed headless path, made deterministic
for console and SSH starts. Do not start a secret-service daemon, copy an OS
keyring, or fabricate an SSH peer as the permanent configuration. Establish a
supported file-storage switch or reliable headless fallback with both transports;
that mechanism remains an open release gate.

A signed-out launch reaches native sign-in. The browser URL and pasted code
stay in that terminal; no configure screen or host keyring access. Fresh raw
credentials are briefly readable by that box until the person explicitly saves
and handoff replaces them, the exposure already accepted by 26-11-02-152.
An unattended Discobox action must not accept vendor legal terms or opt a person
into interaction collection. Establish which first-run choices can be safely
preseeded (theme, local onboarding) and which require the person's action;
if vendor consent is mandatory, expose that honestly as sign-in/onboarding and
have the owner settle any conflict with #744's no-extra-prompt criterion.

Capture personal `consumer` OAuth as a server-renewed secret, not an expiring
plain token. Extend the registered `pkg/harnesscapture.OAuth` metadata with the
vendor client-secret field and carry it through validation, capture, refresh
and tests. Registered bundle metadata is authoritative; never accept a token
URL, client ID or client secret chosen by a sandbox candidate. Old providers
with no client secret retain their current behavior. Server refresh uses the
measured form endpoint and client metadata; preserve refresh tokens when an
endpoint omits a replacement.

Deliver the native JSON credential document with a sentinel at
`token.access_token`, placeholder refresh material and a future expiry so the
CLI spends the sentinel rather than attempting renewal. Only the control plane
holds saved refresh material. Before shipping, prove the file is accepted,
no token/claim inspection defeats a sentinel, and no startup/401 path renews
or overwrites it. Do not fall back to copying real refresh tokens into boxes.

Capture/handoff follows the existing shared lock, exact captured-value digest,
idempotent replacement and newer-login protection contract. No empty credential
file on first launch. Unknown auth methods or incompatible token formats emit
value-free diagnostics and protect local state. Store credentials as strict
credential documents, never ordinary settings or a whole `.gemini` volume.

API-key mode uses a separately declared `GEMINI_API_KEY` token binding and
explicit `modelProvider: "gemini"`; preserve the provider choice as literal
preferences. A key may be manually bound through the existing secret flow.
If no native file stores the key, do not claim automatic capture from arbitrary
shell variables: define and test an explicit supported persistence path before
promising sign-in-once API-key save-back. Do not export shell profiles.

### 3. Destination policy follows the credential, with no Google wildcard

For saved consumer OAuth, the measured starting policy is:

| Destination | Permission supported by this probe |
| --- | --- |
| `www.googleapis.com` | `GET /oauth2/v2/userinfo` only |
| `daily-cloudcode-pa.googleapis.com` | Startup/account/model service; determine all required operations before finalizing path limits |
| `oauth2.googleapis.com/token` | Server refresh only; no delivered refresh credential or sandbox spending grant |

For a Gemini API key, investigate `generativelanguage.googleapis.com` and the
actual header/query placement through the real Discobox proxy. No wildcard
`*.googleapis.com`, no automatic grant from a custom URL, and no copying another
harness's similarly named provider routes. Inference, auxiliary calls and any
fallback hosts must be established before finalizing the policy; unmeasured
hosts remain denied. Method/path limits must reflect required operations rather
than guessed inference paths.

Static secrets and capture provider metadata agree, and bundle refresh preserves
historical restrictions as 26-11-04-772 requires. OAuth browser authorization
hosts and download hosts are egress considerations, not access-token spending
routes. These routes constrain saved sentinel use; they do not reduce Google's
issued scopes or constrain a raw local login before handoff.

### 4. Preferences travel; trust and generated state stay local

Capture literal `settings.json` preferences and verified user keybindings;
exclude trust, onboarding, histories, installation IDs, updater state,
conversations, caches, project records, token stores and credential-bearing
custom-model/MCP/environment/header fields. Do not export all of
`~/.gemini/config`, which also contains other products' and plugin state.
Preserve unknown ordinary preference fields using the existing normalizer rules.

Before starting `agy`, set trust for the actual sandbox workspace paths as data
in a launcher-owned policy/local layer; never trust `/` as a proxy for children.
`--settings` is a candidate session override, not a proven trust mechanism.
Prove trusted and untrusted controls, including direct starts after launch and
a changed working directory. Do not let saved settings replace that baseline.
Use `--dangerously-skip-permissions` for the existing Discobox auto-approval
posture only after the remaining first-run behavior is established. Prompts
must reach `--prompt-interactive` as one argument, including dash-led input;
resume translates to `--continue`. Test a missing prior conversation.

### 5. Instructions, hook ownership and status have distinct contracts

Deliver platform guidance through the verified personal instruction path and
install requested skills in a supported personal skill directory. Preserve the
repository's `AGENTS.md`, `GEMINI.md` and `.agents` content unchanged. Use
behavioral marker controls to prove both platform and repository guidance load;
do not infer this from paths alone. No shared memory format is introduced.

Own Discobox hook code in the bundle. If the global hook file is the only
working layer, reserve one Discobox entry and merge it under the capture lock,
preserving user entries; exclude only that owned entry from save-back. Verify
workspace hooks and user enable/disable settings cannot accidentally suppress
it. A configurable plugin is not a managed hook layer merely because its code
is in the bundle. If no robust layer exists, record that limitation rather than
claiming full hook support or rewriting repository hooks.

Publish only allowlisted scalar identifiers and event metadata, with a short
timeout and no transcript, prompt, artifacts or unrestricted tool arguments.
Map `SessionStart`, `PreToolUse`, `PostToolUse`, and root `Stop` only after
matching their actual semantics and filtering child conversations. Keep
`PreInvocation` and `PostInvocation` provider-native: a model invocation is
not a user prompt or the end of a turn. There is no established permission-wait
hook; do not invent one.

The binary negotiates OSC 7501 in the measured TUI. Verify its actual idle,
working, input/permission-blocked and done messages against Discobox's existing
status reader. This is the preferred status path if it works, independently of
hook completeness. If a state is not reported, disclose it; do not infer state
by scraping changing TUI text or equating every tool hook with waiting.

## Alternatives rejected

- **Persist a shared keyring or whole config directory.** Shares unrelated
  accounts and local state, bypasses reviewed capture and retains refresh
  material in every box.
- **Deliver real refresh tokens to avoid changing capture metadata.** Grants
  boxes renewable broad Google access and loses control-plane renewal ownership.
- **Save only the access token.** Expiry defeats sign-in-once behavior; pretending
  a far-future expiry does not extend the vendor token's actual lifetime.
- **Execute the upstream installer at provisioning or launch.** Resolves a moving release,
  depends on network and bypasses the store's version pin.
- **Hide the native download inside an npm package or parallel updater.** Keeps
  a false package contract or duplicates locks, pinning and retention policy.
- **Reuse other clients' Google destinations or allow all Google APIs.** Neither
  the matching vendor name nor broad issued scopes establish required spending.
- **Save trust or a hook baseline as user preferences.** Leaks per-box paths and
  allows a saved config to replace integration policy.

## Consequences and open release gates

This is design only: no runtime, manifest, registry or current-state DESIGN.md
changes. It does not claim #744's completion. Acceptance must resolve the owner
choice in §2. The following remain explicit implementation/probing gates:

- arm64 artifact/version verification and both Linux runtime controls;
  deterministic file storage without invented SSH identity;
- a successful consumer/API-key model request through the real pool proxy,
  complete credential destinations, API-key persistence and replacement;
- sentinel replay across a second box, server renewal including the registered
  client secret, refresh omission/rotation, rejection and concurrent re-login;
- first-run theme, consent and telemetry behavior, trust with controls, updater
  suppression, instructions and skill discovery;
- hook ownership/merge semantics, root-session attribution and actual OSC states.

If a probe contradicts the proposed storage or policy, amend this proposal
before implementation ships; do not silently choose a broader route or copy raw
refresh material. If OAuth cannot meet these gates, return the API-key-only
scope decision to the owner rather than counting Google sign-in as complete.

Implementation must test the native store's atomicity and architecture isolation,
launcher arguments, strict credential schemas, normalization and handoff,
static/provider route agreement, allowed and denied proxy spending, and the
fresh/saved-login TUI flows. Update the affected DESIGN.md files with that code.
