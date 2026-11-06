# 26-11-06-596 — Built-in harnesses have one declaration and one vendor adapter

- **Status**: Proposed (would narrow [26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md) §3's authored feature layout for built-ins: those files become generated bundle content; its provision protocol and script-file rule remain)
- **Date**: 2026-11-06
- **Issue**: [#745](https://github.com/discobox-ai/discobox/issues/745)

## Context

Built-ins already share a bundle contract, provisioning engine, capture protocol
and version store. Their authoring does not share a model. On the starting
checkout, `00541f9c`, `harness/codex-cli/provision.yaml` repeats installation of
capture assets, metadata, a launcher, its check, and `agent.conf`. The other
coding harnesses repeat that list. Each `Dockerfile` repeats the same packaging.
`harness/registry/registry.go` imports seven tiny Go driver packages which repeat
identity separately from `image.json` and the build watcher's harness list.

Launchers mix argument translation with trust, source-scoped memory, native auth
imports and environment setup. `harness/capture/runtime.cjs` shares the capture
transaction but still branches on harness IDs and loads a separately named
normalizer. OpenCode's `credentials.sh` implements native-store reconciliation;
Codex's `trust.cjs` writes the system trust layer; omp's `proxy-env.sh` derives a
vendor proxy variable. Hooks necessarily have different vendor entry points:
command configuration, extensions, or an OpenCode plugin.

Uniform filenames alone would hide this duplication. A general settings language
would instead obscure important differences: managed Claude policy outranks user
preferences, Codex system defaults do not, and pi has no system settings layer.
The refactor must preserve those differences and every existing account and file.

## Decision

### 1. One built-in authoring shape; the published contract stays a bundle

Every built-in directory, including shell, has this shape:

```text
harness/<directory>/
  harness.json         # sole declaration, schema-versioned and strictly checked
  adapter.cjs          # vendor operations; explicit empty implementation for shell
  DESIGN.md            # vendor semantics, evidence, and supported limitations
  assets/             # literal vendor policy/config and SDK entry points, if any
  testdata/           # fixtures, if any
  *_test.go           # tests of the integration, as needed
```

Only the first three files are required. Empty directories and dummy policy,
capture or hook files are unnecessary. No hand-authored per-harness Dockerfile,
Go driver, provision file, launcher, launcher check, capture wrapper, agent.conf,
normalizer, trust helper or credential sync shell script remains. Literal assets
have no independent lifecycle: each must be referenced by a declaration or adapter
and its purpose documented. Vendor SDK modules may remain assets; they may not
become another launcher, capture transaction, or installation implementation.

A shared Go build tool under `internal/cmd` validates declarations and stages the
bundle inputs. It generates the existing image label, provision steps and script
files, plus the small command entry points and version-store configuration.
Taskfile targets, release builds and the dev watcher use this same tool and
shared bundle Dockerfile. Generated staging output is disposable build content,
not a second maintained source tree. Changes to shared machinery invalidate all
bundles which include it. Output is deterministic for identical inputs, and is
published under content-addressed paths before any manifest points to it.

`harness` owns the declaration types and validation; the registry derives built-in
`Definition` values from embedded declarations, without importing one Go package
per harness. Build tooling consumes that same inventory. Identity distinguishes
directory, driver/hook provider, config slug and bundle name: `codex-cli`, `codex`
and `discobox-harness-codex` are intentionally different and must stay so.

This is an authoring contract for built-ins, not a new public registration format.
Third-party bundles still register through existing OCI labels, can extend a
harness bundle, and need not adopt our generator or Node adapter. Generated
features still use `discobox.ai/provision/v1`, scripts as files, existing feature
dependency/readiness rules, and the platform API compatibility check. The shared
integration runtime is bundled by each coding harness and requires Node; it does
not become a generic platform or server dependency. Shell installs no adapter
runtime, agent, hooks or capture and keeps its reserved login-shell behavior.

### 2. A declaration names parts, ownership and lifecycle

`harness.json` has explicit sections below. Unsupported operations are declared
as absent/none with a documented reason, never inferred by probing for a script.
Unknown keys, missing asset references, contradictory declarations and unsupported
schema versions fail the build. Version 1 covers the existing seven built-ins;
it is not an expression language or arbitrary ordered task list.

| Part | Declares | Runs when and where |
| --- | --- | --- |
| Identity | Existing IDs, names, bundle name and platform API requirement | Build/registration; never changes stored config identity |
| Install | Package, executable, feature dependencies, required OS packages, install environment and version-store metadata | Generated provision steps, machine-scoped; agent version selection remains the existing pool store's |
| Launch | Executable, fixed argv, first/resume argv, prompt encoding and empty-prompt behavior | Shared launcher, as sandbox user, every conventional launch |
| Settings | Literal portable seed files and capture exclusions; separately, vendor system/managed policy assets with explicit destinations and precedence | Policy at provisioning; portable files through existing runtime delivery and layer precedence |
| Local state | Trust/onboarding strategy and source memory strategy, including existing paths and fallback policy | Launch preparation with actual cwd, home, uid and source availability |
| Environment | Static defaults and named variable aliases with preserve-if-set semantics | Static manifest env; aliases in login-shell environment and launcher preparation |
| Credentials | Existing secret names, delivery modes, provider schemas, spending routes, rejection scopes and authoritative refresh metadata; file or native-store strategy | Existing runtime binding/file delivery; guarded native import before launch; export/handoff on capture |
| Hooks | Vendor transport/entry point, literal wiring assets and emitted vendor event names | Wiring at provisioning or declared home-file delivery; publication on vendor callback |
| Capture | Watched paths, credential/local exclusions, provider normalization strategy and installed protocol commands | Existing agent watch/export, reconsideration, save and acknowledged handoff |

Each fact has one authoritative home: package identity supplies both installation
and agent.conf; provider metadata supplies both label validation and capture;
launch declaration supplies both launcher and its drift check. Generated copies
are products, not separately editable definitions. Shared validation checks these
relationships. Egress hosts, credential spending routes, rejection evidence and
server-side refresh destinations remain distinct fields with distinct meanings.

### 3. Shared orchestration, explicit vendor operations

The shared runtime owns prompt joining, argv execution without shell evaluation,
environment aliasing, locking, safe file access, atomic writes, structured-file
parsing, baseline projection, reference resolution and the capture transaction.
The install generator handles the current npm installation pattern, including
private temporary npm caches and declared install environment. OS package needs
such as pi's `fd-find` are data, not a copied install script. A genuinely different
installer requires a named, tested implementation in the framework rather than
inline shell strings in JSON.

`adapter.cjs` exports named vendor operations selected by the declaration, with
required methods validated for each selected strategy. It replaces the existing
normalizer and vendor branches in capture, and supplies the irreducible trust,
auth-store and hook conversions. It does not own a second lifecycle scheduler.
The framework passes a bounded context (cwd, home, uid, source data location,
current delivery and capture input as appropriate), never a template context
exposing arbitrary sandbox configuration. It orchestrates these stages:

1. Provision the machine-scoped CLI, runtime, literal policy and hook assets.
   No provision step reads a sandbox account, source or secret.
2. Deliver effective files and sentinel references with the existing agent
   installation/repair path. Resolve credentials only in registered documents.
3. At launch, derive environment aliases, prepare source memory and local trust,
   then reconcile delivered native credentials under the existing capture lock.
   Locks cover each conflicting read/modify/write, not the lifetime of the CLI.
   The declared strategies retain current failure behavior: signed-out is valid,
   guarded imports protect local logins, memory failures retain local fallback,
   and required trust preparation failures do not silently proceed.
4. Encode argv and exec the vendor CLI. Joining prompt words, dash and `@`
   handling, resume behavior, exit status and terminal signals remain tested
   behavior. The platform's pre-harness source gate continues outside this wrapper.
5. On capture, the framework reads/projects state and invokes vendor normalization.
   Only after durable save acceptance does acknowledged handoff retire or replace
   the captured login, retaining digest checks and idempotent retry.

Trust is a framework lifecycle feature with vendor strategies, not one universal
trust-file format. Codex retains system-layer trust for later direct starts;
Claude retains serialized local onboarding/trust updates. Likewise, native auth
uses supported vendor export/import commands, never direct SQL. OpenCode keeps
its owned-entry IDs, active-account rules and crash journal; omp keeps its native
identity and guard. Live database processes still defer handoff until stopped.
The framework owns guard/lock/retry orchestration; vendor adapters own the native
command and document semantics. Diagnostics remain value-free.

Hook wiring similarly shares installation and publication, not an invented SDK.
Command hooks use generated command configuration; SDK hooks retain a thin
extension/plugin asset which translates callbacks and invokes the common
publisher. Root-session filtering, event payloads and settlement timing remain
vendor-specific and tested. Canonical event translation remains server-side in
`harness/hookevent.go` under ADR 0146; this refactor does not move that authority
into declarations or reinterpret stored events.

### 4. Preserve durable and observable behavior

This proposal changes authoring and orchestration, not the account model or
configuration format. It preserves:

- OCI labels, layering, bundle names, harness IDs/slugs, commands typed into
  terminals, supported platforms and signed-out starts. No configure flow returns
  (ADR 26-11-02-152).
- Literal user settings and exact JSON secret references, exclusions, supported
  login forms, route restrictions and server-owned refresh metadata (ADRs
  26-10-30-457 and 26-11-04-772). No new template evaluator or auto-save.
- `FileOverrides` over `ConfiguredFiles` over bundle `Files`; refresh never
  writes personal overrides, save writes only the saved layer, and plain reset
  keeps overrides (ADRs 26-11-04-028 and 26-11-04-711). Child configs follow only
  their parent's bundle snapshot, not its account (ADR 26-11-04-426).
- Existing capture protocol versions, receipts, baselines, checkpoints, old
  sentinel aliases, native-store journals, watched home paths and secret names;
  ordinary saves retain ADR 26-11-04-538's overwrite behavior.
- User-visible env defaults, clipboard behavior, source memory paths/uid
  partitioning and fallback behavior, native store selection and direct CLI
  starts after preparation. Version-store pinning and explicit harness upgrades
  retain ADR 0114 and current upgrade policy.

### 5. Upgrade from v0.19 in place

No harness config, secret, binding, database or home is deleted and recreated.
Bundle refresh only refreshes bundle-owned snapshots through existing services.
The existing file-override split/backfill remains responsible for older unsplit
rows; the generator must not bypass it or turn NULL into empty before it runs.
Existing copied/extended configs keep their IDs, accounts and inheritance.

Internal source filenames can change without renaming installed entry points,
home paths, capture command paths or credential-document shapes. Generate those
existing destinations first. This avoids adapters for obsolete call shapes and
keeps persisted save-back commands and already pinned bundles usable. Any path
that truly must change needs a separate explicit, idempotent migration which
preserves unknown local content and can resume after interruption; it cannot be
silently decided during the refactor.

Migration evidence must include a real v0.19 database/home fixture with each
built-in's saved settings/account documents and personal files, plus a child
config with its own account. After normal upgrade, refresh, restart and save,
assert the same effective files, binding ownership, default selection and login
state; reset must remove saved state while keeping personal overrides. Retain
unknown settings and literal template-like text. Test pending capture and prior
sentinel aliases across refresh, and interrupted native handoff. Evidence must
be redacted and use controlled credentials.

For each converted harness, keep its existing integration tests and exercise
first launch, signed-out sign-in, resume, direct start after preparation, trust,
hooks and capture/save/restart. Include argv metacharacters, source-less boxes,
shared memory, repeated provisioning and concurrent trust/import/capture. Tests
must establish semantic equivalence, not only generated-file equality. Build and
runtime verification cover both supported Linux architectures and Docker/VM
provisioning; machine snapshots must contain no sandbox identity or credentials.

## Alternatives rejected

- **Rename the existing files and document them.** It makes directories look
  alike while leaving install, capture, launcher and identity copies to drift.
- **Force every vendor detail into declarative JSON.** Native store recovery,
  token normalization and SDK callbacks need code. An embedded expression or
  shell language would be harder to review than a named vendor operation.
- **One script per lifecycle phase in every directory.** It provides a uniform
  name but still copies orchestration and lock rules. Share that machinery and
  retain only vendor semantics in the adapter.
- **Rewrite all capture and SDK integration in Go.** Existing tested JavaScript
  handles vendor document forms and hooks; a language migration adds risk to a
  behavior-preserving refactor. Go remains the contract/build owner; Node stays
  a coding-harness dependency, as it is today.
- **Require third-party bundles to use our authoring layout.** The existing
  provisioning and installed-command protocols are sufficient. Changing them
  would enlarge the migration without simplifying the built-ins.

## Consequences and implementation boundaries

Adding a built-in becomes a checklist: fill the declaration, implement selected
vendor operations, supply referenced literal assets, document semantics in its
DESIGN.md, and run the shared conformance suite plus vendor integration tests.
No separate registry driver, watcher inventory entry or copied shell wrapper is
needed. #744 can then prove the shape with an additional vendor.

The cost is a generator and a shared runtime whose changes affect multiple
harnesses; validation and cross-harness tests are required. A small file count
alone is not success: moving arbitrary scripts into a large adapter is rejected.

This ADR decides the ownership, layout, lifecycle and preservation requirements.
Exact JSON key/type definitions, adapter method signatures and generated staging
paths remain implementation work, constrained by these requirements. A vendor
capability outside the existing strategies must be documented and tested; a
change to trust policy, account semantics or public bundle protocols needs a
separate decision. Conversion order and CI checkpoints belong to the delivery
task, not this record. Implementation updates the current `harness/DESIGN.md`,
per-harness DESIGN.md files and add-harness skill with the code. This PR contains
only this proposal and its index entry; no refactor is authorized until accepted.
