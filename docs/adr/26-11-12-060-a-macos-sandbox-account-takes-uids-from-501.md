# 26-11-12-060 — A macOS sandbox's account takes uids from 501

- **Status**: Accepted. Supersedes, for a darwin sandbox only,
  [0141](0141-a-sandbox-account-is-created-with-an-id-the-guest-gives-accounts.md)
  §1's single `[1000, 60000]` range and §2's normalization of a macOS
  client's ids. It settles
  [26-10-09-106](26-10-09-106-disco-vm-is-a-pool-runtime-and-the-server-runs-its-machines.md)
  §5's "ADR 0141's range becomes per guest OS".
- **Date**: 2026-11-12

  Accepted by the owner by merging #1083, 2026-11-14.
- **Decided**: by the owner, 2026-10-24, on
  [#121](https://github.com/discobox-ai/discobox/issues/121): a macOS
  sandbox's account takes uids from 501 up; Linux sandboxes are unchanged
  ([26-10-16-423](26-10-16-423-a-harness-is-a-provisioning-bundle-and-the-agent-provisions-any-box.md)
  §14).
- **Issue**: [#121](https://github.com/discobox-ai/discobox/issues/121), part
  of [#42](https://github.com/discobox-ai/discobox/issues/42) (step 4.1 of
  [its task](../tasks/42-one-vm-pool-shape.md)).
- **Relates to**:
  - [0145](0145-a-sandbox-declares-its-platform-and-a-non-linux-one-is-a-vm-template.md)
    §5, a non-Linux sandbox has one account, narrowed for macOS by 106 §5:
    that account has POSIX ids.
  - [26-11-11-963](26-11-11-963-one-vm-provider-type-every-pool-runs-a-linux-pool-agent-in-a-machine-its-driver-makes.md)
    §9, the darwin base VM the server installs adds the single account
    "(uids from 501)".

## Context

Checked against `main` at `be413820`.

ADR 0141 gives every sandbox account one range: root, or `[1000, 60000]`,
Debian's `UID_MIN`/`UID_MAX`. Below 1000 a Linux guest keeps its system
accounts, so a macOS client's own 501 in group 20 (`dialout` on Linux) is
refused by the API and replaced with 1000 by the client.

macOS is the other way round. Its first ordinary account is 501, every
account it creates has primary group 20 (`staff`), and ids from 1000 up have
no special meaning. 106 §5 gave a macOS sandbox's single account POSIX ids
and made 0141's range per guest OS, without saying what macOS's is.

Today `sandboxuser.HasPOSIXIDs` is true for Linux alone, so
`ValidateAccount("darwin")` takes the single-account path of 0145 §5 and
refuses any uid. The server validates every create as `"linux"`
(`server/internal/resources/sandboxes/service.go`), because every sandbox it
records is Linux.

A Linux machine sandbox's ids are not a range question: 423 §14 deletes
whatever account holds the client's uid.

## Decision

### 1. The account range is the guest OS's

`sandboxuser` judges a sandbox account's ids by its platform's OS:

| Guest | uid | gid |
| --- | --- | --- |
| `linux` (container and machine) | 0 or `[1000, 60000]`, as 0141 | 0 or `[1000, 60000]`, as 0141 |
| `darwin` | 0 or `[501, 60000]` | 20 (`staff`) or `[501, 60000]` |
| `windows` | none: 0145 §5's one account, by name | none |

- **Linux is unchanged**, for container and machine sandboxes alike. A
  machine guest's account in the way is deleted (423 §14).
- **darwin starts at 501**, the first id macOS gives an ordinary account. The
  upper bound stays 60000, the one `sandboxuser` already has; macOS assigns
  upward from 501 and nothing a client sends is above it.
- **A darwin gid of 20 is accepted**, since it is the primary group of every
  account macOS makes. 0141 §2's "gid becomes the uid" would give a macOS
  account a group macOS never made.
- **darwin keeps 0145 §5's single account** as 106 §5 narrowed it: one
  account, named in the manifest, now with ids. An exec still may not name
  another user or group set.
- **The rule is the same everywhere it is applied**: an out-of-range id is a
  400, never clamped (0141 §1).

### 2. The client normalizes against the sandbox's platform

0141 §2's normalization is the client's choice of an id the guest can use,
so it is made for the guest the sandbox will have:

- **A macOS client creating a darwin sandbox sends its own account**, 501 in
  group 20, unchanged. The macOS sandbox's account matches the developer's.
- **Creating a Linux sandbox, nothing changes**: a uid below 1000 becomes
  1000, a gid outside the range becomes the uid.
- Windows clients still send `discobox` at 1000 to a Linux sandbox.

### 3. Existing sandboxes are untouched

No darwin sandbox exists yet, so nothing is migrated. Linux sandboxes,
including ones created before 0141 with 501/20, keep their ids, as 0141
says.

## Alternatives rejected

- **One range for every guest, `[501, 60000]`.** It reopens the Linux system
  range 0141 closed: a macOS client's 501/20 lands below `UID_MIN` in
  `dialout` again.
- **Keep `[1000, 60000]` on macOS too.** It works, but the macOS sandbox's
  account would never match the developer's own, and 1000 is not where macOS
  puts its first account; a base VM installed with its account at 501
  (963 §9) would not meet it.
- **No ids on macOS** (0145 §5 as first written). 106 §5 already rejected
  it: macOS has POSIX ids, the pool cache partitions by uid (0094), and
  source trees are chowned to it.

## Deferred

- **How the sandbox's account meets the darwin base VM's** (963 §9), when
  the client's name or uid differs from the one the server installed:
  whether 423 §14's deletion applies on macOS, or the base's account is
  renamed or adopted. Revisit with the macOS sandbox image (#126).

## What it needs

- `sandboxuser`: `HasPOSIXIDs("darwin")` is true for creation; the range is a
  function of goos; a darwin gid of 20 is in range; single-account exec
  checks stay for darwin.
- Server: sandbox create validates against the sandbox's platform, not a
  fixed `"linux"`.
- CLI (`cli/internal/sandboxcreate`): normalize the host account against the
  target platform.
- Done when (#121): `sandboxuser` accepts 501 for a darwin sandbox and
  refuses it for a Linux one.

## Consequences

- A macOS developer's macOS sandbox runs as their own uid 501 and group
  `staff`; their Linux sandboxes still run as 1000.
- The pool cache (0094) and source-scoped memories partition a darwin
  sandbox by its uid, 501 for a typical Mac.
- `sandboxuser/DESIGN.md` describes the per-OS range once implemented.
- How a darwin sandbox's account meets the one the server installed in the
  darwin base VM is still open (Deferred); #126 settles it.
