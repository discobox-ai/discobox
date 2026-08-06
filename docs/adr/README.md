# Architecture Decision Records

An ADR records a decision, the alternatives rejected, and why — at the time it
was made.

## When to write one

Only when either is true:

- A plausible alternative was rejected for a non-obvious reason.
- Something was deferred with a specific condition for revisiting it.

Otherwise skip the ADR and update the relevant `DESIGN.md`. Most changes do not
need one.

## Rules

- **Immutable.** Never edit an accepted ADR. Superseded by a new one that links
  back; mark the old one `Superseded by NNNN`.
- **Outside the drill-down hierarchy.** ADRs live here, never next to code.
  `DESIGN.md`/`REVIEW.md` are read root-down by agents on every task; ADRs are
  not, and must not be, because they are history rather than current state.
- **Not a substitute for `DESIGN.md`.** When the work lands, update the live
  design docs to describe what now exists. The ADR keeps the "why we didn't";
  `DESIGN.md` keeps the "what is".

## Status lifecycle

`Proposed` → `Accepted` → (`Superseded by NNNN`)

Use `Rejected` for decisions considered and declined; keep the file.

## Workflow

Nygard-style ADRs (see adr.github.io) combined with current-state design docs:

1. **Decide first.** Draft the ADR as `Proposed` and land it on its own,
   before implementation. Flipping it to `Accepted` is the decision gate —
   with review, the ADR's PR is where the review happens, and merge means
   accepted. Keep the status in the ADR header and the index table in sync.
2. **The accepted ADR is the spec** while implementation is in flight.
   `DESIGN.md` never describes in-progress or planned work — only what exists.
3. **DESIGN.md rides the code.** Each change that alters the architecture
   updates the affected `DESIGN.md` files in the same change, not as a
   follow-up pass. When the ADR's work fully lands, the live design docs
   describe the new state and the ADR keeps only the why.
4. **Plans are not documents.** Sequencing, checklists, and rollout order live
   in the task or branch driving the work; they are meant to go stale.
5. **Wrong decisions:** an `Accepted` ADR may still be amended while nothing
   has shipped against it. Once implementation has landed, supersede instead.

## Index

| ADR | Title | Status |
| --- | --- | --- |
| [0001](0001-sandbox-origin-and-remote-source-push.md) | Sandbox origin and remote source push | Accepted |
| [0002](0002-harness-config-is-the-only-harness-concept.md) | Harness config is the only harness concept | Accepted |
| [0003](0003-promote-pool-to-a-first-class-primitive.md) | Promote pool to a first-class primitive | Accepted |
| [0004](0004-user-namespaces-are-the-default-isolation.md) | User namespaces are the default isolation | Proposed |
| [0005](0005-kubernetes-backend-is-a-worker-driver.md) | Kubernetes backend is a worker driver | Proposed |
| [0006](0006-pool-is-the-runtime-host.md) | Pool is the runtime host; the worker resource is removed | Accepted |
| [0007](0007-declarative-sandbox-volumes-wired-by-the-sandbox-agent.md) | Declarative sandbox volumes wired by the sandbox-agent | Proposed |
| [0008](0008-attach-stream-packages.md) | Attach stream is one protocol with two roles | Accepted |
| [0009](0009-previous-configure-secrets-are-prefixed-sentinels.md) | Previous configure secrets are offered as prefixed sentinels | Proposed |
| [0010](0010-deletes-are-hard-deletes.md) | Deletes are hard deletes | Proposed |
| [0011](0011-oauth-secrets-refresh-server-side-on-resolve.md) | OAuth secrets refresh server-side, on resolve | Proposed |
| [0012](0012-sandbox-config-is-three-attribute-owned-layers.md) | Sandbox config is three attribute-owned layers, merged by a shared library | Accepted |
| [0013](0013-local-linux-pools-use-libkrun-microvms.md) | Local Linux pools use libkrun microVMs with VSOCK and passt | Accepted |
| [0014](0014-disco-apply-pulls-sandbox-commits-via-cherry-pick.md) | `disco apply` pulls sandbox commits to the host via cherry-pick | Accepted |
| [0015](0015-nested-docker-builds-trust-the-mitm-proxy-via-nri.md) | Nested Docker builds and containers trust the MITM proxy via an NRI plugin | Superseded by [0020](0020-nested-docker-trust-is-injected-by-a-runc-wrapper.md) |
| [0016](0016-sandbox-image-upgrades-are-explicit-and-in-place.md) | Sandbox image upgrades are explicit, in-place, and digest-driven | Accepted |
| [0017](0017-resource-state-is-desired-and-observed-with-no-operations.md) | Orchestration is generation convergence; a resource has state and desired state | Accepted |
| [0018](0018-disco-diff-resolves-its-base-inside-the-sandbox.md) | `disco diff` resolves its base inside the sandbox | Accepted |
| [0019](0019-one-server-per-data-directory-enforced-by-a-file-lock.md) | One server per data directory, enforced by an advisory file lock | Proposed |
| [0020](0020-nested-docker-trust-is-injected-by-a-runc-wrapper.md) | Nested Docker trust is injected by a runc wrapper, not an NRI plugin | Accepted |
