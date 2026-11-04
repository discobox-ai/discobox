# 26-11-04-253 — Git credential delivery is an explicit client policy

- **Status**: Proposed (would supersede [26-10-23-948](26-10-23-948-a-pool-pulls-a-cached-source-first-and-a-client-supplies-what-the-pull-could-not.md) §5's implicit noninteractive fetch-and-push fallback and narrow §4's `needs_credential` fallback to the policy below)
- **Date**: 2026-11-04
- **Issue**: [#614](https://github.com/discobox-ai/discobox/issues/614)
- **Relates to**: [26-10-09-389](26-10-09-389-the-cli-reads-one-schema-checked-configuration-file.md), [0128](0128-a-private-remote-source-is-fetched-with-a-credential-the-client-lends.md) (rejected).

## Context

Checked against `main` at `0b74d90a131a6cde9fbf600a109d5c0ad0070b59`:

- `cli/internal/cli/run.go` exposes `--lend-git-credentials`. It supplies
  consent, not a demand to fetch on the pool when the client holds the commit.
  Explicit false still permits a question. `cli/internal/clientconfig/config.go`
  has no saved Git policy.
- `cli/internal/sandboxcreate/sourcepull.go` asks only after the pool records
  `needs_credential` and the client lacks the commit. Without consent or a
  question callback it falls back to local fetch/push. Inside a sandbox it
  always takes that fallback. Attach and start also deliver owed sources.
- `cli/internal/cli/sourcepull.go` and `cli/internal/tui/gitcredentials.go`
  offer “Use Git credentials for this pull” and “Fetch here and push”, without
  explaining where the credential goes or the difference between request
  lifetime and token expiry.
- Lending calls local `git credential fill`, sends username/password through
  `server/internal/resources/sandboxes/source_pull.go` to
  `pool-agent/server/source_pull.go`, and fetches synchronously.
  `pool-agent/gitcache/pull.go` passes the values in the Git process environment;
  its temporary askpass file contains variable references. It disables Git
  credential helpers and redirects. The request is not persisted as resource
  intent or cache Git configuration. Success calls local `credential approve`,
  which can save credentials in the client's helper; authentication failure
  calls `reject`. Nothing revokes or narrows the original credential.
- Local fetch uses this machine's Git configuration before pushing objects.
  `cachedeliver.go` uses `sandboxgit.NoPromptEnv`, but the package's
  `REVIEW.md` explicitly notes askpass and SSH questions. Merely hiding the
  consent dialog is not enough to make automation noninteractive.

## Decision

### 1. One client policy, resolved once and shared by every frontend

Add top-level `git.credentials` to `client.yaml`, with exactly `ask`, `lend`,
`local`; omission means `ask`. Generate its schema and reference from
`clientconfig.Config` as other settings are. This is a machine-wide preference
for this client, across servers and repositories; its documentation must say
that saving `lend` authorizes future eligible pulls without another consent
question. Repository settings, project defaults and sandbox metadata cannot
supply this consent. Selecting an option in a question does not save it.

Expose `--git-credentials=ask|lend|local` on `run`, `attach`, `start`, and the
console entrypoints (including bare `discobox`). A console invocation's explicit
choice covers deliveries it initiates until it exits; a one-command window
inherits that command's choice. Otherwise the console uses the saved preference.
The New Discobox command preview includes the effective non-default policy so
copying it preserves consent. The policy covers primary, reference and skills
Git sources alike; each `ask` question identifies its own remote.

Precedence is an explicit flag, then the saved setting, then `ask`. An explicit
`ask` overrides a saved `lend`. Invalid values fail before source work starts.
Pass the resolved typed policy into the shared sandbox creation/delivery code;
frontends supply question/process callbacks, not their own fallback rules.
Nothing adds a delivery mode to the API or persists this client policy on a box.
`admin box create` keeps its existing contract: no client preference or automatic
credential lending; it can leave a box awaiting a later delivering client.

Keep `run --lend-git-credentials` as a deprecated input spelling: explicit true
means `lend`, explicit false means `ask`, matching its current consent semantics.
Either explicit value overrides the saved setting. Supplying both old and new
flags is an error, even if they agree. Translate the old spelling at parsing;
keep only the new policy in the delivery model. Help directs users to the new
flag; no new environment variable or second saved preference is introduced.

### 2. The policy selects consent, not where every Git byte must move

Keep the anonymous pool pull, existing cache/push shortcuts and delivery of local
unpublished commits. Public sources and commits already held need no question.
At the point a host client would otherwise answer `needs_credential`:

| Policy | Behavior |
| --- | --- |
| `ask`, interactive | Explain both options and ask for this source. Cancel aborts this delivery attempt; it does not silently choose local. |
| `ask`, no interactive input | Fail before invoking a credential helper or starting an authenticated fallback. Name the source and show `--git-credentials=local`, `--git-credentials=lend`, and `git.credentials` in the actual configuration path. |
| `lend` | Attempt the existing synchronous lending flow without the consent question. |
| `local` | Fetch on this machine and push, without lending the upstream credential. |

An unavailable helper, exhausted authentication retries, or a pool unable to
finish the credentialed pull retains the existing local-fetch fallback under
`lend`; status output explains the switch. Local authentication can still fail.
`local` does not stop an anonymous pool pull already started by creation, and
`lend` does not force a pool pull when the client already holds the commit.
Neither option grants a box access to the client's upstream credentials.

A sandbox client still never lends: it uses its existing local-fetch/push path
through its own proxy under `ask` or `local`, without a consent question. Explicit
or saved `lend` in a sandbox is rejected with instructions to use `local`, rather
than pretending the request was honored. The server's sandbox-principal refusal
remains mandatory. This preserves 948 §5's sandbox exception.

Failure after creation leaves the box owed its source, available for a later
attach/start with a chosen policy; do not delete it or extend its existing
source timeout. Report its identity and the retry command. No missing policy
error is required for a public source, cached commit, or other path needing no
credential decision. Ref resolution may already have used local authentication:
the pool-consent question is not consent to authenticate locally.

### 3. Explain the highlighted option before confirmation

Use the same labels and explanation content in the inline picker and console:

- **Let the pool use my Git credentials.** Git's credential helper runs on this
  machine. Its username/password or token is sent through the control plane to
  the pool, which authenticates to the recorded HTTPS remote for this pull.
  Discobox does not save the credential in a server/pool resource, cache Git
  configuration, or credential store. Git receives it in its process environment
  during the synchronous request. The client's helper may save it locally on
  success. Discobox's use ends when the request finishes; the original token
  keeps its provider scope and expiry and is not revoked or restricted to this
  repository by this choice. The server and pool must be trusted with it.
- **Download on this machine and send to the pool.** This machine authenticates
  using its Git configuration and fetches missing objects, then sends those
  objects to the pool. It does not lend the upstream credential to the pool.
  Objects can remain cached here and on the pool; the initial download can
  travel through this machine twice. Normal local helper storage rules apply.

These are required facts, not a requirement for this exact prose. Avoid “never
saved” without qualification, token-expiry promises, or secure-memory-erasure
claims. Pool/server application code must not log credentials or request bodies;
this is not a promise about independent infrastructure administrators or their
logging. The request can contain several Git fetches to supply one source.

Render a persistent description area for the currently highlighted option,
including the initial highlight. Keyboard movement updates it; supported mouse
motion over either row previews that row's explanation. Hover never confirms.
A terminal without hover reporting retains keyboard access to the same text.
Keep the complete explanation readable by wrapping/scrolling on small terminals;
it must be accessible before Enter/click confirms. Sanitize remote text as today.

### 4. Noninteractive means no authentication questions, throughout the operation

Use the existing frontend capability checks for whether a question can be drawn;
a TUI disabled by flags, redirected streams, or absent terminal is not interactive.
An explicit/saved strategy skips consent, not authentication requirements.

For noninteractive operations, apply a shared no-question execution policy to
ref resolution, lending's `fill`, and local fetch/push, including retries and Git
URL rewrites. Disable Git terminal prompts, GUI/askpass prompts and credential
manager interaction; SSH must use batch mode and refuse unknown host keys instead
of asking. Existing noninteractive helpers and configured SSH credentials can
still answer. Never install the frontend terminal-handoff callback in this mode.
Missing credentials fail actionably; authentication retries must be finite and
context cancellation must terminate child processes. Do not change interactive
askpass behavior globally: IDE helpers are a valid local authentication source.
A configured helper is executable user code; this policy prevents Discobox/Git
questions, not arbitrary helper programs from misbehaving.

## Alternatives rejected

- **Only rename the existing boolean flag.** It cannot express a local choice or
  override saved lending with an explicit request to ask.
- **Keep implicit local fetch without a terminal.** It avoids credential lending
  but hides an authentication/data-transfer choice and cannot tell automation
  how to select a strategy. Existing scripts hitting this case must opt in.
- **Always fail noninteractive operations lacking a setting before creation.**
  Public sources and held commits need no consent; forcing their callers to
  configure private-repository policy adds no protection.
- **Force all fetches onto the selected machine.** This changes cache delivery,
  requires a server-side control over anonymous pulls, and lends credentials
  unnecessarily when a local push already suffices. That is a different feature.
- **Store per-repository consent or infer it from repository content.** That
  adds another precedence/store model, and untrusted source content cannot
  authorize release of the user's credential. A user can choose `local` or
  `ask` globally and use `lend` explicitly for one command.
- **Mint a repository-limited expiring token.** Git helpers return arbitrary
  credentials; no common forge-independent narrowing/revocation API exists in
  this flow. Describe its real trust boundary rather than implying attenuation.

## Consequences and acceptance evidence

Implementation must cover CLI, console, saved configuration/schema/reference,
shared delivery, help and user documentation together. Current-state design docs
change with that implementation, not this proposal. No credential transport or
server resource schema changes are required by the consent policy.

Acceptance needs focused tests for the precedence table and legacy flag conflict;
all delivering entrypoints and source kinds; absent-policy failure before helper
execution at the credential decision; public/cached success without a setting;
sandbox prohibition; finite retries, cancellation, askpass/GCM/SSH refusal in
noninteractive ref resolution and delivery; and keyboard/mouse explanation
preview before confirmation. Exercise actual terminal flows as well as tests.
Keep the pool test proving credentials absent from cache config and unavailable
to a later anonymous pull; check local approve/reject without claiming revocation.

## Open implementation details

The dialog layout/scroll mechanics and wording can be refined while preserving
§3's facts and pre-confirmation access. Platform-specific SSH command composition
and credential-manager controls need verification on supported platforms,
including Git URL rewrites and IDE environments. These are required behaviors to
prove during implementation, not grounds to assume today's no-prompt environment
already implements §4. No product or trust choice is left implicit.
