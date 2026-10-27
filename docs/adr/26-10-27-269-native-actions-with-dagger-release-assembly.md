# 26-10-27-269 — Native Actions with Dagger release assembly

Status: Accepted (supersedes ADR 26-10-26-225's native-execution deferral and narrows its Actions-as-event-adapter-only decision for native jobs)

Date: 2026-10-27

## Context

The user explicitly chose conventional GitHub Actions jobs for the work that
requires macOS or Windows, with the same commands runnable locally. Linux
containers cannot execute Windows tests or the Apple SDK/codesign toolchain.
A remote-native-worker dispatcher inside Dagger would add infrastructure without
making that work portable.

## Decision

Dagger retains Linux checks, portable helper and executable builds, OCI builds,
release assembly and publication. Native Actions invokes repository-owned Go
commands: macOS runs Darwin lint/vet, workspace tests, and native build/sign/smoke;
Windows runs workspace tests and smokes Linux's exact cross-built executables.
Portable helpers are transferred from Linux so macOS does not rebuild them.

Release jobs check out the same explicit tag. macOS signs the final server
before generating its digest and linking the CLI, then signs the CLI and emits
a version/revision-bound receipt with signing and smoke evidence. Linux Dagger
accepts those files as an explicit directory argument and validates receipts,
required checks and final hashes before assembly or publication. Missing native
artifacts still fail closed. No native result is inferred from a Linux pass.

## Consequences

The complete release is locally runnable using a Mac-produced artifact directory;
Linux alone cannot manufacture that directory. Actions transports files without
owning packaging or publication policy. Native jobs remain ordinary required
checks on main/PR. Existing ad hoc signing is preserved; Developer ID signing,
notarization, and automatic winget submission are not added.
