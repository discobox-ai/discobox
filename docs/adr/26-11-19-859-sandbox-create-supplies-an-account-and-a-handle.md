# 26-11-19-859 — Sandbox create supplies an account and a handle

- **Status**: Accepted
- **Date**: 2026-11-19
  Accepted by the owner by merging #1490, 2026-11-24.
- **Amends**: [ADR 0025](0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md)
  §5 for Linux sandbox create, and [ADR 26-11-16-207](26-11-16-207-a-box-is-named-at-creation-by-seeding-its-meta-name.md)
  §1 for a create with no handle.
- **Issue**: #1486

## Context

The schemas for `CreateSandboxBody` and `SandboxCreateConfig` in
`api/openapi/server.yaml` require `config` and its handle. The create service
(`server/internal/resources/sandboxes/service.go`) checks the handle and
validates the account against the pool's guest OS. With no user it records
unset account fields. The built-in Docker sandbox image runs as root by
default, and Claude Code refuses its configured permissions mode when run
as root. The CLI avoids both problems by supplying a generated handle and
its own account.

[ADR 0141](0141-a-sandbox-account-is-created-with-an-id-the-guest-gives-accounts.md)
explains why the Linux account's numeric IDs must be known before boot.
Source checkout needs them, and a rebuild must use the same IDs for durable
files. Linux boot already provisions a requested account, including
replacing an image account that occupies its name or UID
(`sandbox-agent/boot/user.go`, [ADR 26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md) §14).

## Decision

### Default the account in the create service

Resolve the omitted account after choosing the pool and before validating and
recording the manifest. An absent `config.user` and an empty user object
both request the default.

| Guest OS | Default |
| --- | --- |
| Linux, container or machine | `discobox`, UID 1000, GID 1000, home `/home/discobox` |
| macOS | The template's single ordinary account, with the name and IDs the template provisions |
| Windows | The template's single ordinary account, identified by name |

Persist the Linux default as an ordinary account request. Boot applies the
same collision rules to it as to a client-supplied account, including on a
custom image with its own non-root user. A client that needs a particular
image account can supply its identity. If the image cannot provision the
requested account, provisioning reports the failure without falling back to
the image user.

On macOS and Windows, omission retains the template-account path from
[ADR 0145](0145-a-sandbox-declares-its-platform-and-a-non-linux-one-is-a-vm-template.md)
§5 and [ADR 26-11-12-060](26-11-12-060-a-macos-sandbox-account-takes-uids-from-501.md).
A template eligible for this default must provide an ordinary account. The
server supplies no additional login, numeric IDs or home override on this
path. If the pool has yet to declare its guest OS, a create needing the
default account returns a retryable 503 until the OS is known. This avoids
persisting a Linux account on a box that placement could later assign the
pool's non-Linux platform.

A user object carrying explicit fields keeps their existing validation and
resolution rules, including explicit UID 0 where supported and the image
identity inherited by a Linux groups-only request. The default applies only
to fresh creates. Existing rows, imports, rebuilds and per-exec overrides
retain their account semantics. Git identity stays optional.

### Generate a project-unique handle at create

Accept an absent `config` as an empty configuration, and make
`SandboxCreateConfig.name` optional. The server draws the omitted handle
with `pkg/randomname`. An explicitly blank handle is invalid. Explicit
valid handles are stored verbatim and a duplicate in the same project gets
409. The handle remains distinct from `config.metaName`.

Use the `idx_sandbox_project_name` constraint as the authority for uniqueness
at insertion, including when two creates race. Retry only a generated
handle's collision on that constraint, with a fresh handle and a bounded
number of attempts. Exhaustion returns 503. Retry only after the failed
create transaction has rolled back. The intent, bindings, inline secrets and
reconciliation work become visible together with the successful insert
(`server/internal/resources/sandboxes/intents.go`). The retry bound and
database-specific error classification are implementation details.

With a configured default pool and harness, `{}` can create a sandbox.
`{"harnessName":"claude-code"}` selects that harness on the default pool.
Existing validation still applies to the resolved pool and harness.

## Alternatives considered

- **Use the image's account or declare a non-root requirement per harness.**
  The image can default to root even when the harness works as an ordinary
  user. A harness-specific requirement leaves API clients discovering which
  combinations need an explicit identity and still leaves source ownership
  unresolved before boot.
- **Require the caller to send a user and handle.** This catches omissions
  early but makes every client implement the same defaults and collision
  handling.
- **Let Linux boot choose a free UID.** A UID selected at boot arrives too
  late for source checkout and can change across images. Persisting a
  deterministic account before provisioning gives the checkout and the
  sandbox the same ownership contract.
- **Generate a handle based only on a preflight lookup.** A concurrent
  insert can take the candidate after the lookup. The database already
  arbitrates handles for the whole project.

## Consequences

Minimal Linux creates use the same ordinary account for source ownership
and the primary terminal. On custom Linux images, omitting the account now
opts into account provisioning, which can replace a colliding image
account under the existing boot rules. Clients that supply an account or
handle continue to control those values. No data migration is needed.
