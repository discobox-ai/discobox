# 26-10-26-225 — Dagger owns CI and release with native worker boundaries

Status: Accepted (supersedes ADR 0066 §§1–4, 6–7 for CI/release orchestration and tooling; ADR 0105 §3's workflow-only promotion and its Taskfile/tap-dispatch consequences)

Date: 2026-10-26

## Context

The user explicitly chose a full Dagger reimplementation of CI and release,
including the Taskfile and Actions entry points, with native macOS and Windows
execution stubbed so the complete graph can be inspected locally. Merely
running Task inside Dagger would preserve two orchestration layers and would
not move image builds, artifact flow, or publication policy out of Actions.

## Decision

Dagger owns the graph and container toolchains. Task exposes shortcuts and
retains native development workflows. Actions supplies events, source refs,
and credentials and invokes the same Dagger operations available locally.
There are no workflow dispatches into the Homebrew tap: its formulae are
recomputed and pushed directly with an explicitly supplied contents token.

Use Go for the pipeline's validation and artifact assembly, with generated SDK
bindings checked for drift. Keep product Dockerfiles and domain generators;
Dagger composes them rather than reimplementing product contents.

Native macOS and Windows execution is deferred until a worker integration can
return same-source results and real macOS signing evidence. The boundary is
explicit reports, not fake success or fake binaries. Preview can complete with
these reports; actual release assembly and publication reject incomplete native
artifacts. macOS's worker contract includes the full Go test suite.

## Consequences

A local machine with Dagger and a container runtime can reproduce the Linux
CI and build graph, including independent VM artifact checks. Native execution
remains visibly unavailable, and real product releases are blocked until it is
implemented. External publication still requires explicit destination credentials.

ADR 0066 §5's final-byte signing rule, ADR 0105's release channels, independent
VM release lines, the Nix host development shell, and product artifact formats
are preserved. The former tap dispatch token must be replaced by a token that
can write formula contents. ADR 0105's release-state/channel rules remain, while
its Actions-only promotion and cross-repository dispatch are replaced by the
same locally callable operations. Existing branch protection job names need to follow
the new single `pipeline` CI job.
