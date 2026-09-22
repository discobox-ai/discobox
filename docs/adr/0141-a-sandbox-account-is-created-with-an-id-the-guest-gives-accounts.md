# 0141 — A sandbox account is created with an id the guest gives accounts

- **Status**: Accepted
- **Date**: 2026-09-22
- **Supersedes**: [ADR 0025](0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md)
  §5's "one giving only `name` leaves the ids to the image's account", for
  sandbox create only. Exec users are unchanged.

## Context

The prompt path sends the client machine's own account as the sandbox user. On
macOS that is uid 501 in group 20. A Linux guest reserves ids below `UID_MIN`
(1000 in the Debian the images build from) for system accounts, so the sandbox
account landed in the system range with `dialout` as its primary group, and
`useradd` warned about it.

The sandbox create API accepted any id, and any user by name alone. A name
alone only works for an account the image already has. For one it lacks, boot
has no passwd entry to read ids from and fails.

PR #36 proposed dropping the out-of-range ids and letting boot create the
account by name, with `useradd` choosing the ids. That moves the numeric ids
out of the manifest, and the pool agent needs them before the sandbox exists
([ADR 0025](0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md)
§4):

- It clones and checks out git sources, and chowns the tree's contents, to the
  manifest uid. With none, it runs git as root and chowns nothing. Boot chowns
  only each source's top directory, so the user gets a root-owned checkout.
- `sandboxUserFromEnv` defaults an absent uid to 0, so the pool agent's
  `git http-backend` for the sandbox runs as root, over a worktree and
  `.git/config` the sandbox can write.
- The export container (ADR 0129) has a read-only rootfs with no capabilities,
  so it cannot run `useradd` to learn the uid.
- Nothing records the allocated uid. A container rebuilt on another image can
  allocate a different one, and the volumes stay owned by the old uid.

## Decision

### 1. Sandbox create refuses an account it cannot create with a usable id

`sandboxuser.(*User).ValidateAccount` is the rule, and the server applies it to
every sandbox create with a 400:

- A user that gives `name` or `homeDirectory` must give `uid`.
- `uid` and `gid` must each be 0 (root) or within `[1000, 60000]`
  (`AccountIDMin`/`AccountIDMax`, `UID_MIN`/`UID_MAX` in `login.defs`).
- A user that names only groups keeps the image's account and needs no uid.

An out-of-range id is an error, never clamped at runtime. Which id to use
instead is the caller's choice, and a server that silently turned 501 into 1000
would create an account nobody asked for.

The pool agent and boot do not enforce the rule. Sandboxes created before it
keep the ids they were created with, 501/20 included, and still boot as they
did.

### 2. The client chooses a usable id when the host has none

The prompt path normalizes the host account before sending it. It is the one
place such a choice is made:

- A uid outside the range (or not numeric) becomes 1000.
- A gid outside the range becomes the uid, as `useradd`'s private group would.
- A name no Linux account can have becomes `discobox`, as on Windows.

Every such host gets uid 1000, which Windows clients already send.

## Alternatives rejected

**Send the name alone and let boot allocate the ids (PR #36).** The allocated
uid would exist only inside the sandbox, and everything listed in Context needs
it outside, before boot or without running boot. Making that work means storing
the uid after the first boot, feeding it back to the pool agent, and giving
export a way to read it. That is a larger change than choosing the id on the
client.

**Clamp an out-of-range id at runtime.** This hides a wrong request behind a
different account. The API should say the request was wrong.

**Validate in the OpenAPI schema.** `SandboxUser` is shared with exec create,
where uid 33 or a bare name for an account the image has are valid. The schema
also describes persisted sandboxes: an older sandbox still carries 501, and
clients validate the responses they decode.

## Consequences

- A create from macOS runs as uid 1000 with the host's name and home, and its
  source tree is owned by that uid.
- A Linux host in a system primary group (such as `users`, gid 100) gets a
  private group with gid equal to its uid.
- `discobox admin box create --user-name` now also needs `--user-uid`.
- Existing sandboxes are not migrated.
