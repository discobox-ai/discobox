# 0035 — Repair is one rebuild intent, plus a start instruction

- **Status**: Accepted
- **Amended by**: [0064](0064-repair-rebuilds-on-the-current-image.md) — §1's intent also
  re-pins the sandbox to its harness config's current image
- **Date**: 2026-08-12

## Context

A sandbox can end up wedged: its reconcile settled as a failure (`ErrorMessage`
latched, generations agreeing), its container gone or broken, its disposable
pool-host state (proxy material, certificates) missing or corrupt — while its
durable tree is intact. The concrete case that motivated this: a container
replace raced the pool agent's proxy-material reaper, the replacement create
failed on a pruned bind source, and the failure settled. By ADR 0017 §4 a
settled failure is converged by design and is never retried without new
intent, so the sandbox sits in `error` until the user does something.

The something available today is `delete` (archive) followed by `unarchive` —
two separate intents the user must sequence by hand, and neither starts the
sandbox afterward. Users read the sequence as one action: repair.

## Decision

Repair is a single API operation, `POST .../sandboxes/{id}/repair`, composed
from three existing mechanisms and no new ones:

1. **One existence intent.** Repair records ordinary present-intent through
   `recordSandboxIntent` — generation bump, error cleared — with one addition:
   the sandbox's `RepairGeneration` column is set to the new generation. The
   reconciler's `ensure`, when converging exactly that generation, tears the
   runtime down first (the provider's `Archive`: container and disposable
   state dropped, durable tree kept, ADR 0022 §6) and then falls into the
   ordinary create, which rebuilds against the retained tree. Retries within
   the generation are safe because `Archive` and create are both idempotent;
   later generations never tear down again because the marker names exactly
   one generation.
2. **Driven inline.** Like purge (ADR 0022 §3), the request records the
   intent durably and then runs that sandbox's reconcile in the request, so
   the caller learns whether the rebuild landed. A request that dies loses
   only the synchronous answer: the intent and dirty mark are already
   committed, and the rebuild converges in the background, leaving the
   sandbox stopped and on-demand start (ADR 0017 §12) covers first use.
3. **A trailing start instruction.** After the inline reconcile converges
   clean, the service forwards the same start instruction `POST .../start`
   would. Power stays unorchestrated (ADR 0017 §9): nothing stores "should be
   running", and a repair whose rebuild converged but whose start failed is a
   healthy stopped sandbox, not a failed repair.

Repair is refused with 409 for a sandbox whose desired state is `archived`
(unarchive is the recovery for archive) or `deleted`.

## Alternatives rejected

- **Client-side sequencing (archive → wait → unarchive → wait → start).**
  Three round trips racing the reconciler: unarchive conflicts until the
  archive converges, so the client must poll; a failure latches on an
  intermediate generation and can leave the sandbox archived — strictly worse
  than the error state repair was meant to fix; and no single generation owns
  the repair, so nothing in the record says what was asked for.
- **A transient desired state (`repairing`).** Desired state answers existence
  only and must have a converged reading. A state that means "tear down, then
  be present" has none: level-triggered reconciliation would re-run the
  teardown forever, or the state would need to rewrite itself — a disguised
  operation queue, which ADR 0017 removed deliberately.
- **A pure instruction, like restart.** An instruction cannot clear the
  latched `ErrorMessage` or bump the generation, and a settled failure —
  which only accepted intent re-drives (ADR 0017 §4) — is precisely the state
  repair exists for. It would also put a provider `Archive`+create sequence
  on the instruction path, which writes no lifecycle state by contract.
- **Reusing the spec fingerprint to force a replace** (bump a nonce, let the
  pool agent's drift detection rebuild). The replace path drops only the
  container; repair must also drop disposable pool-host state, which is
  `Archive`'s defined job — and a nonce in the spec would be a fake spec
  field whose only meaning is "not equal to last time".
