# runuser

Answers one question for everything inside a sandbox that launches a process as
somebody: **given what a caller asked for, who does the process actually run
as?**

Decision record: [ADR 0025](../../docs/adr/0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md).

## The API

```go
u, err := runuser.Resolve(runuser.User{Name: "dev"})
// u.UID=1000  u.GID=2000  u.HomeDirectory="/home/dev"  u.Group=""
```

`Resolve` is the entry point. It returns a `User` with nothing left to work out
— no name to look up, no nil gid, no unresolved group name — so callers read the
fields directly instead of each re-deriving them.

| Call | Use for |
| --- | --- |
| `Resolve(User) (User, error)` | Any identity you are about to launch a process as. |
| `Groups([]string) []uint32` | Supplementary GIDs for a credential, unknown entries dropped. |
| `LookupGroupID(string) (uint32, bool)` | One entry — a name or a numeric GID — to a GID. |
| `NameAndHome(*User)` | Login name and home only, when you do not need ids. |

`execs.User` is an alias of `runuser.User`. One type: the exec record, the boot
flow, and anything new describe identity the same way.

## Rules it enforces

- **Ask, never default.** A missing id is read from the passwd entry. Never `0`,
  never `gid = uid` — that coincidence is a `useradd` default, not a fact.
- **Only what is missing is looked up.** A complete identity needs no account to
  exist yet, which is what lets the boot flow resolve an account it is about to
  create.
- **Names resolve to ids, one way.** `Group` becomes `GID` and is cleared;
  numeric entries resolve as ids before names, so a group named `997` cannot
  shadow gid 997.
- **Membership is the caller's, resolution is the OS's.** Whoever supplied
  `AdditionalGroups` decides who is in what; the group file only turns an entry
  into a number, dropping what the image never created.
- **An error is final.** Callers must not fall back to a default when `Resolve`
  fails; that fallback is the guess this package exists to remove.

## Boundary

Resolution reads the image's own `/etc/passwd` and `/etc/group`, so this package
is usable **only from inside the sandbox**. The control plane and the pool agent
cannot resolve a sandbox's names — the account may not exist until boot creates
it — and must not try (ADR 0025 §4).

## Testing

`FixedDatabase() (restore func())` swaps the lookups for a fixed table; use it
with `t.Cleanup` from any package. Its ids deliberately break `uid == gid`, since
a fixture reproducing that coincidence would hide the bug rather than catch it.

It takes no `*testing.T` on purpose: importing `testing` from a non-test file
registers test flags on every binary that links the package.
