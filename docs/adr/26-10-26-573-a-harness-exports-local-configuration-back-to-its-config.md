# 26-10-26-573 — A harness exports local configuration back to its config

- **Status**: Accepted (narrows 0011 §2 for a credential's
  originating sandbox, 26-10-14-718 §2 for pending local credentials, and
  26-10-15-848 §3 for pending and accepted local credentials)
- **Date**: 2026-10-26

## Context

A harness config stores files and secret bindings shared by the sandboxes that
use it. Configure runs the harness in an ephemeral sandbox, captures settings
and credentials, and commits them to that config. Subsequent sandboxes receive
templated files and credential sentinels; the proxy resolves a sentinel and
substitutes the real credential on an authorized request.

A user can change the model, preferences, or account inside a working sandbox,
but those changes do not return to the harness config. Copying the files back
is insufficient: settings can contain secrets, workspace trust is local, two
harnesses keep credentials in databases, and a rendered file contains this
sandbox's sentinels and paths rather than a portable template.

The sandbox that signs in has real credentials before the server has them.
After saving, it should consume the same sentinel-backed configuration as the
other sandboxes. Writing the sentinel to disk does not prove that its running
harness discarded a cached OAuth refresh token. The harness and the control
plane can both refresh it, spending a rotating token the other still holds.

Today `terminal.WatchSecretFiles` restores a sentinel-bearing file when the
sentinel disappears. That recovers from a harness clearing its login after a
401, but can also erase a new login before it is captured. omp imports the
delivered credentials on launch; OpenCode synchronizes its image-owned
credentials and can change which account is active. Those writes need to
cooperate with capture as well.

## Decision

**A harness declares what to observe and how to export portable configuration.
The sandbox reports changes; a user saves a specific candidate into the harness
config. Credentials are persisted separately and acknowledged before the
originating sandbox replaces them with sentinels. OAuth refresh overlap with
that sandbox is an accepted limitation.**

### 1. Observation belongs to the sandbox; export belongs to the harness

The harness manifest gains a versioned save-back declaration containing
home-relative watch inputs and fixed, noninteractive export and handoff commands
installed by the bundle. Inputs can be files or bounded directories. An input is a
change trigger, not permission to save its bytes: a database can trigger an
export without ever becoming a harness config file.

The sandbox agent runs the exporter as the sandbox user, with bounded runtime
and output. Its watcher and file collection confine access to the declared
home-relative inputs and reject path escapes and symlink escapes. The exporter
has the sandbox user's ordinary access; all its output is untrusted.
The command executes inside the sandbox only; no command or path supplied by
the sandbox executes on the client, pool, or control plane.

An export contains portable settings files, logical credential entries, and
diagnostics for unsupported changes. A logical credential identifies its
provider/account and existing binding where known, and carries either a keep
reference or a newly observed secret value. Files referring to credentials use
templates, never literal credentials or the originating sandbox's sentinels.
Unknown sentinel-like values are rejected, not stored as new credentials.

Configure and save-back use the same harness-owned normalization and credential
conversion. Configure retains its interactive setup and verification and its
authoritative output semantics (ADR 0009); incremental save-back has patch
semantics (§3). Neither path calls the other's interactive flow.

Credential provider definitions are authored once in each harness bundle's
versioned metadata, beside its save-back declaration. They name supported
credential kinds, OAuth refresh parameters, host constraints, and route rules.
Build installs the same definitions for configure/export to consume; the server
snapshots and validates them at harness registration, through the existing
manifest ownership boundary. Dynamic account entries refer to a provider ID,
not an exporter-supplied refresh URL. This moves the relevant constants out of
configure scripts rather than creating a second server-only provider table.
Definition changes carry the manifest revision and invalidate pending captures
against an older contract. A sandbox cannot register or amend these definitions
by exporting them. Extending supported providers requires a user-authorized
harness registration/update, not approval of an arbitrary URL at save time.

### 2. Detect portable changes, not writes

The agent records a normalized baseline after initial file installation and
credential delivery, together with the source harness-config revision and
credential revisions. It observes atomic replacements and directory changes,
debounces bursts, and periodically rescans to recover missed notifications.
Database-backed inputs include WAL activity, but exports use the harness's own
supported export command and are rate-limited.

Only a change to the normalized export produces a candidate. Timestamps,
session history, caches, generated trust entries, and known derived tokens do
not. A partial write, parse failure, timeout, or unsupported format produces a
diagnostic and retains the last valid state; it never becomes an empty export
or an instruction to delete anything.

The baseline survives agent restart. For an existing sandbox with no baseline,
the installed configuration and its provenance are the comparison point;
pre-existing local differences remain candidates, not silently acknowledged
defaults. A source harness-config revision is not advanced merely because the
server changed: it advances only for content actually delivered or explicitly
saved from this box.

The user sees the source sandbox, target harness config, changed settings, and
credential metadata with values redacted. A candidate has an immutable identity
covering its settings, credential changes, and baseline. Saving approves that
candidate only. A later local edit remains pending even if an earlier save is
still in flight. Merely editing a file does not authorize changing a shared
harness config or granting a newly discovered credential.

### 3. Save is an authorized, conditional patch

The control plane accepts save-back through an authenticated user operation
authorized to update the target harness config and manage the affected secret
bindings and grants. The sandbox can report a candidate but cannot grant
itself shared configuration-write authority. The server checks the source
sandbox's project and current harness assignment against the target; a box
cannot select another config by putting its ID in an export. Notification and
status routes carry metadata only. Secret-bearing candidate retrieval and
submission use explicitly authorized routes with body logging disabled.

The request names the immutable candidate, an idempotency key, and the baseline
config and credential revisions. It explicitly sets or removes entries in the
configured overlay; omission means unchanged. A file removal removes the
configured override, revealing any bundle baseline. A local logout or missing
credential is a notification, not automatic deletion or revocation of a shared
secret. Credential deletion remains the existing user operation.

The server validates the entire patch before committing files, secrets,
bindings, grants, revisions, the idempotency result, and required reconcile
dirty marks in one transaction. The acknowledged result survives a lost
response. Repeating an idempotency key returns that result; using the same key
with different content is refused. A failed transaction acknowledges nothing.

A concurrent config edit, account replacement, deconfigure, harness switch,
or OAuth refresh causes a revision conflict. The server does not silently
retry the old credential against the new revision. The user can reconsider a
fresh candidate; the conflict response never returns stored secret values.
Configure commits and hand edits participate in the same revision discipline,
so they cannot overwrite an intervening save-back unnoticed.

Delivered provenance and the conditional-write revision are separate. Conflict
resolution reads current remote settings and redacted credential metadata and
compares them with the original delivery baseline and local patch. The user
chooses which local settings to keep against that current state; the result is
a new immutable candidate with the current conditional-write revision and a
new idempotency key. This does not pretend those remote settings were delivered
to the sandbox or discard unrelated local edits. A credential conflict requires
an explicit choice to keep the remote binding or replace it with the captured
local account; a fresh login can supply that replacement. Selecting a captured
OAuth value is a deliberate replacement that may already be stale (§5), never
an automatic retry after observing a server refresh. Another intervening write
conflicts again.

Existing config-owned credentials retain their secret IDs when replaced, so
sentinel bindings remain stable. A new credential creates a config-owned
secret and its explicit harness-config grant under the same ownership and
route rules as configure. Hand-bound project secrets are not overwritten or
deleted by save-back; replacing such a binding requires the user to explicitly
choose a new config-owned credential. Keep references resolve only against
the source sandbox's delivered bindings and the approved target config.

Provider refresh endpoints, clients, hosts, and credential routes are
validated against the registered harness/provider contract on the server;
an exporter cannot choose an arbitrary URL for server-side refresh or widen
an existing grant. Unsupported custom providers remain diagnostics until a
trusted provider definition exists. Secrets use the secret store's sealing
path, never ordinary configured-file content, notifications, audit bodies,
error messages, or logs. Ordinary settings export is schema-aware: recognized
credential fields are extracted, unsupported credential-bearing or unknown
fields are reported rather than copied under an assumption that they are safe.

### 4. Capture precedes restoration; acknowledgment precedes handoff

Credential observation and sentinel restoration share one per-credential
state machine. It distinguishes a delivered sentinel, a new supported
credential, an expected derived token, a cleared login, and an unsupported
value. It captures a new credential before any restoration or launch-time
import can overwrite it. Pending candidates are private to the sandbox and
survive agent restart; any spool is bounded, mode 0600 in a private directory,
excluded from sandbox export, and removed when acknowledged or discarded.
Fingerprints used for local comparison never enter public status or logs.

An unsaved, rejected, conflicting, or unacknowledged candidate is not evidence
that its credential reached the server. Preserve it and expose its state.
Export failures and unsupported values also inhibit destructive restoration
for that entry until the user resolves or discards the change. A recognized
cleared login with no pending new credential can still be repaired from its
delivered sentinel, retaining today's recovery from a 401.

After durable acknowledgment, deliver the accepted sentinel representation to
the originating sandbox using its current binding. Compare the local input
with the captured generation before replacing it: a newer login must be
captured separately, not erased by acknowledgment of an older one. File
delivery, `WatchSecretFiles`, and database imports use this same coordination.
For a database, use the CLI's import/logout interface, never edit its tables.
Only the credential/account the candidate saved may be converted; unrelated
local accounts remain local. Failure to install the sentinel is retried and
reported as pending delivery, without resubmitting the real secret.

The agent invokes the manifest's handoff command as the sandbox user, with the
acknowledged candidate ID, captured local generation, exact local credential
identity, and accepted sentinel delivery on a private input channel. It passes
no server-held real secret. The harness-owned command implements the selective
file rewrite or CLI import/logout and returns `applied`, `local-conflict`, or
`retryable-failure`. It is idempotent for the same acknowledgment. A local
conflict triggers another export and leaves the newer credential untouched;
a retryable failure remains pending delivery. The command owns the
storage-specific check-and-replace: where a CLI cannot safely replace a live
entry conditionally, it must defer until a safe handoff point and report that
state rather than overwrite an account it cannot identify. Launchers and the
secret-file repair loop consult this shared state before mutating credentials.
This is a required companion to export for a save-back declaration, not
harness-specific database logic embedded in the generic agent.

A harness that rereads the file sees the sentinel; one that caches credentials
can keep using its access token. No forced process restart is required for
successful save-back. Other sandboxes receive only sentinel-backed delivery,
subject to their existing bindings and grants. Server-side refresh material
is never sent back to the originating sandbox or any consumer.

### 5. Accept the originating sandbox's OAuth refresh race

ADR 0011's server-side on-resolve refresh, singleflight, and conditional store
write remain. Its §2 guarantee that the control plane is the only refresher is
narrowed for a sandbox that originated a credential and still caches it.
The proxy triggers server resolution; it does not become a refresh owner.

The originating harness may refresh before or after the control plane. Either
can invalidate the refresh token the other holds, and a provider may invalidate
the token family. A database revision check prevents stale writes to Discobox;
it cannot serialize requests already made to the OAuth provider. Handoff on
disk does not end this risk until the harness stops using cached refresh
material. There is no promised time bound on that overlap.

A newly written token set is another observable candidate, not proof that it
is newer than the server's token set or safe to overwrite it. Retransmitting
an accepted candidate is a no-op; a changed one follows the revision and user
authorization rules above. Refresh failures remain visible through existing
credential-failure reporting; recovery can require a harness restart or a new
login. Saving does not claim to prove that an OAuth credential will remain
refreshable.

We accept this failure mode to support a working sandbox as a credential
origin. Revisit coordinated refresh ownership if observed failures make that
workflow unreliable. Provider-specific token ordering, refresh interception,
and a mandatory harness restart are not prerequisites for this decision.

### 6. Portable settings are defaults, not a live broadcast

Saving settings updates the harness config's configured overlay. Future
sandboxes receive it. Existing sandboxes retain local settings; applying new
defaults to one is an explicit action that observes its dirty state and
conflicts. Watched local edits survive terminal recreation and agent restart.
Sentinel installation and template rendering are recognized delivery writes,
not changes to offer back to the user.

Generated workspace trust stays local. A captured template retains its
portable variables rather than freezing rendered paths or sentinels. A
project file overlay is not promoted into shared defaults: export uses file
provenance to omit project-owned paths and reports unsupported mixed cases.
Credential replacement is different from settings propagation: updating a
shared secret changes what existing consumers resolve, subject to existing
proxy cache lifetime. The save preview names that shared-account effect.

### 7. Each built-in owns its storage conversion

The supported surface is the portable settings and credentials already
understood by configure, with the stricter field classification in §3.
Unknown storage versions fail visibly rather than falling back to raw copy.

| Harness | Settings and observation | Credential conversion |
| --- | --- | --- |
| Claude Code | `.claude/settings.json`; observe `.claude.json` for credentials without exporting its workspace state | Extract `primaryApiKey` or `.claude/.credentials.json` OAuth material; preserve supported OAuth metadata and render the existing sentinel templates. |
| Codex | `.codex/config.toml`, removing the local project-trust map and retaining the working-directory template | Extract `.codex/auth.json` through the existing API-key/OAuth conversion; preserve the account metadata needed beside a sentinel. |
| OpenCode | Global `opencode.json`, `opencode.jsonc`, `config.json`, `cli.json`, `.local/state/opencode/model.json`, and Discobox's harness settings | Observe the database and WAL, export with `auth export --standalone`, and capture the active credential per provider. Import the accepted sentinel and retire the exact saved local credential so it cannot stay active with a real refresh token on disk; preserve unrelated accounts and legacy `auth.json` support. |
| Copilot | `.copilot/settings.json`, omitting configure-only `storeTokenPlaintext` | Extract the selected account's token from `.copilot/config.json`; retain the existing github.com-only and credential-route rules. After acceptance remove only that captured plaintext token. Delivery remains `COPILOT_GITHUB_TOKEN`; a running process can retain its previous environment until restart. |
| omp | `.omp/agent/config.yml` and Discobox's harness settings | Observe `agent.db` and WAL, use `omp auth export`/`import`, and match multiple accounts by sentinel or provider/account identity rather than export order. Pending local credentials survive launch-time import. |
| pi | `.pi/agent/settings.json`, `keybindings.json`, and Discobox's harness settings | Read `auth.json`. A Copilot entry whose `refresh` still holds the delivered sentinel and whose `access` changed is an expected exchange, not a new GitHub login. Preserve existing unsupported-login diagnostics. |
| shell | No default declaration | Nothing to capture; custom harnesses can declare their own export contract. |

omp's `models.yml` and pi's `models.json`/`mcp.json` remain outside the portable
surface: literal keys, commands, and machine-local provider setup require
their own supported conversion. This decision does not expand which providers
can work with sentinels or which OAuth flows the control plane can refresh.

### 8. Configure remains; capture does not depend on it

Configure keeps its command, sandbox mode, verification, and enablement flow.
Save-back can create a credential when no previous binding exists, and its
contract does not require a configure-run ID or a completed configure session.
The current product gate for starting an unconfigured harness remains.

Removing configure is a separate follow-up once settings and credential
save-back work, including first-credential creation and handoff. That follow-up
must decide initial enablement and login/callback UX. It need not introduce a
second mechanism for capturing credentials.

## Alternatives rejected

- **Copy every changed file.** It copies rendered sentinels, secrets, local
  trust, and databases into defaults, and cannot distinguish token exchange
  from account replacement. Export needs harness-specific semantics.
- **Save settings only.** It leaves the credential-origin problem unsolved and
  prevents save-back from replacing configure later.
- **Automatically promote every observed change.** A sandbox agent can edit
  settings or login files; doing so is not authority to change shared defaults
  or replace the account used by other sandboxes. Save approves a candidate.
- **Reuse configure's wholesale replacement commit.** An incremental export
  omits unchanged or unsupported entries. Treating omission as deletion loses
  credentials and unrelated settings.
- **Restore sentinels immediately and capture afterward.** It destroys the
  only copy of a new credential. Restoration must await capture and durable
  acceptance; retrying an upload is not an acknowledgment.
- **Require perfect OAuth refresh ownership before saving.** Generic harnesses
  expose neither a cache-eviction acknowledgment nor a refresh lock Discobox
  can share. Forcing every login through a disposable process or restarting
  the working harness would avoid some overlap at the cost of the intended
  workflow. The race in §5 is accepted explicitly instead.
- **Give the server's refresh token back to the origin to repair a race.** It
  extends real-secret delivery and creates another rotation owner. Recovery
  uses sentinel delivery, restart, or a new user login.

## Consequences

The manifest and sandbox config contracts carry observation/export metadata;
the agent owns watching, normalization baselines, and handoff coordination;
the harness-config service owns authorized patch commits and conflicts. API
changes are contract-first in the canonical OpenAPI documents. Clients expose
pending changes and explicit save without receiving plaintext credentials.

Persisted revisions, baselines, and idempotency results need additive migrations
and defaults for existing configs. No database is recreated, no configured
secret is renamed, and no existing config requires reconfiguration merely to
gain save-back. A bundle without a save-back declaration continues to work.

An originating sandbox holds a real credential from login until handoff, and
possibly in process memory afterward. Sentinel replacement is not secure
erasure and cannot retract a credential already read by sandbox code. Consumer
sandboxes continue to receive sentinels. This is an explicit extension of where
login can originate, not a guarantee that the originating sandbox never saw a
secret.

The principal costs are harness-specific export support, durable conflict and
retry handling, and an accepted OAuth recovery burden. Live design documents
change with implementation; this proposal does not describe an implemented
watcher or remove configure.
