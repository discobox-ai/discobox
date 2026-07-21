# 0009 — Previous configure secrets are offered as prefixed sentinels

- **Status**: Proposed
- **Date**: 2026-07-21

## Context

A harness's configure command runs interactively in an ephemeral
`harnessMode=config` sandbox. Its contract is two fixed paths: it **writes** its
result to `ConfigureOutputPath`, and it **may read** the previous run's
configuration from `ConfigurePreviousConfigPath`, which the control plane seeds
before the command starts (`harness/driver.go`). Output is authoritative and
replaces the previous configuration wholesale, so a command that means to keep
something must re-emit it.

The point of the seed is reconfigure. A user who runs `disco configure` a second
time — to change a model, fix a setting, re-run after an image update — should
not be forced through a browser login for a credential that is still perfectly
good. To offer that choice honestly, the command must be able to *verify* the
existing credential first: it may have been revoked upstream since.

The existing seed could not support that. `previousConfiguration` put
`secret.EncryptedValue` into the seed's `value` field, gated on a live grant to
the harness config. That is broken in both directions:

- **With a sealer configured**, `EncryptedValue` is ciphertext. The command
  receives bytes it cannot use, so it re-prompts anyway. The field is dead
  weight.
- **Without a sealer** (`store.OpenSecretValue` passes plaintext rows through),
  it is the credential itself, written to a file inside the sandbox.

So the mechanism either does nothing or leaks, depending on deployment
configuration — and the grant check guarding it was a second, divergent copy of
enforcement that already exists at the point of use.

Meanwhile the platform already has a way to give a sandbox the *use* of a secret
without the value: sentinels. `mintSentinel` puts a format-shaped placeholder in
the sandbox env; the proxy calls `ResolveSandboxSecret`, which swaps it for the
decrypted value on an outbound request only while a live grant covers the
sandbox at sandbox, harness-config, or project scope. Every other secret in
every other sandbox already works this way.

## Decision

**The seed carries secret metadata only. The values are offered separately as
sentinels under `ConfigurePreviousEnvPrefix` (`PREV_`) + the bound env name.**

### 1. The seed says which secrets exist, not what they are

`configureSecret` loses `value` on the seed path and gains `usePrevious`.
`previousConfiguration` emits `envName`, `name`, `type`, `host`, and
`usePrevious: true` for each secret a previous configure run created — that is,
each binding whose secret ID is in `ConfiguredSecretIDs`. Secrets the user bound
by hand are not the configure flow's to replay and are left out.

The seed is therefore itself a valid output: writing it back unchanged is an
exact no-op reconfigure, which is the baseline a command edits rather than
rebuilds.

### 2. Values arrive as `PREV_`-prefixed sentinels

`applyPreviousConfigureSecrets` injects one sandbox secret per seeded secret,
bound to `PREV_` + its env name — a secret bound to `ANTHROPIC_API_KEY` arrives
as `PREV_ANTHROPIC_API_KEY`. These are ordinary sandbox secrets: the value is a
sentinel, resolution goes through the proxy, and the harness-config-scoped grant
the previous configure run created is what covers them.

A configure command can therefore *exercise* the old credential — which is how
it verifies one — without being able to read, print, or log it.

### 3. The prefix is load-bearing

Seeding the credential under its own name (`ANTHROPIC_API_KEY`) would let the
harness CLI pick it up from the environment and silently authenticate. The flow
would then offer a choice it had already made, and its verification step would
prove nothing about the credential actually being configured. `PREV_` forces the
command to opt in.

### 4. Keeping a secret is resolved at commit, two ways

`applyConfigureOutput` treats a secret as kept — carrying over the existing
secret row, binding, and grant — when either holds:

- `usePrevious: true` with no value, resolved against the config's current
  configure-created bindings (`previousSecretIDsByEnv`).
- The value contains one of the sandbox's `PREV_` sentinels, i.e. the command
  wrote `X=$PREV_X` straight through (`previousSentinels`,
  `matchPreviousSentinel`).

Kept IDs join `createdSecretIDs`, which is what spares them from the sweep that
deletes the previous generation of configure-created secrets.

### 5. Grant enforcement stays at use

`previousConfiguration` no longer checks grant liveness. A revoked credential's
sentinel simply fails to resolve at the proxy, and the command's own
verification fails.

## Alternatives rejected

**Keep seeding the secret value in the previous-config file, encrypted.** The
status quo. Rejected because it does not work: sealed, the command gets
unusable ciphertext; unsealed, it gets a live credential in a file. There is no
configuration under which the field does the job it appears to do.

**Seed the decrypted value in the file instead, and accept it.** Honest about
what it is, and simplest for the command to consume. Rejected because it makes
the control plane write a plaintext credential into a sandbox filesystem — the
one thing the sentinel/proxy design exists to prevent. A configure command needs
to *use* the old credential, not hold it, and sentinels already draw exactly
that line. It would also be the only path in the system that serializes a
credential out of the control plane.

**Seed the sentinel under the original env name, no prefix.** Keeps the credential
out of the sandbox and requires no changes in a configure command — the harness
CLI just works. Rejected: that is the failure mode, not the feature. The CLI
authenticating on its own means the user is never offered the choice, and a
"verification" that passes because an unrelated variable was in the environment
verifies nothing about the credential being configured. Reconfigure would also
become impossible to escape — there would be no way to *not* use the old
credential short of unsetting a variable the command never set.

**Offer nothing; always re-prompt.** Simplest, and correct in the sense that a
fresh login always works. Rejected because it makes every reconfigure a browser
round trip, which is a real cost for a flow users hit to change a non-credential
setting. It also pushes users toward not reconfiguring at all.

**Re-check grant liveness while building the seed.** Retained from the old code;
it drops a secret whose grant is gone so the flow re-prompts. Rejected as a
divergent second copy of enforcement. `ResolveSandboxSecret` is the single place
that decides whether a secret may be used, and it runs at the moment of use with
the sandbox's full scope set. A check at seed time can disagree with it — grants
expire between the two — and its only effect would be to hide an option the
command is about to verify anyway. A revoked credential failing verification is
the same outcome by a shorter path.

**Accept only `usePrevious`, not the echoed sentinel.** One way to say one thing.
Rejected because the sentinel echo is what a naive shell script does naturally
(`X=$PREV_X`), and the alternative to accepting it is storing a sentinel string
as a credential — configuring the harness with something that resolves to
nothing, silently. Recognizing the sentinel turns a plausible mistake into the
right behavior. Conversely `usePrevious` is kept as the explicit form, so a
command that never touches the value can still say "keep it."

**Let output be a patch over the seed rather than a replacement.** Would make
"keep everything" the default and remove the need for `usePrevious` entirely.
Rejected to preserve one rule for the whole output contract: files are already
wholesale-replaced, and a command that re-emits its files but patches its
secrets is harder to reason about than one that always states the full result.
The seed being a valid no-op output gives the same ergonomics without a second
merge semantics.

## Consequences

- `harness/driver.go` gains `ConfigurePreviousEnvPrefix`, part of the fixed
  image contract alongside the two paths.
- `configureSecret.Value` becomes `omitempty` and `UsePrevious` joins it.
  `previousConfiguration` stops reading `EncryptedValue` and stops calling
  `FindLiveGrant`.
- `CreateSandbox` branches on `harnessMode`: `config` sandboxes get
  `applyPreviousConfigureSecrets` instead of `applyHarnessConfigSecrets`.
  Required-secret enforcement stays off in config mode — this flow is how those
  secrets come to exist.
- `applyConfigureOutput` takes the configure sandbox ID, since resolving an
  echoed sentinel means reading that sandbox's secret assignments.
- A configure output that reuses an env name with no previously configured
  secret bound to it, or that supplies neither a value nor `usePrevious`, is a
  commit error rather than a silent empty secret.
- **A revoked credential prompts rather than fails outright.** An unresolvable
  sentinel makes `ResolveSandboxSecret` open a pending secret request, so the
  user sees an approval prompt for the old credential during configure. That is
  consistent with every other sandbox secret, but it is a visible behavior in a
  flow whose whole purpose is replacing that credential.
- Configure commands must opt in: a command that ignores the seed and the
  `PREV_` variables still works, and simply re-prompts on every reconfigure. All
  three included harnesses opt in.
- Verification is per-credential and must isolate the one being checked, since a
  configure sandbox may hold several `PREV_` variables at once — opencode checks
  each provider with the other's variable unset for exactly this reason.
- The stub harness (`test/harness-stub/configure.sh`) exercises both the seed
  and the keep path via `STUB_CONFIGURE_KEEP`.
