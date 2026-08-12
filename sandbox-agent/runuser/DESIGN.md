# runuser

Answers one question for everything inside a sandbox that launches a process as
somebody: **given what each layer asked for, who does the process actually run
as?**

Decision records: [ADR 0025](../../docs/adr/0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md),
[ADR 0032](../../docs/adr/0032-user-resolution-is-one-layered-resolver-with-declared-gaps.md).

The type, the layer precedence, and the `Fields` vocabulary belong to
[`sandboxuser`](../../sandboxuser/DESIGN.md) in the root module. This package
adds the half that needs the image: completion against its `/etc/passwd` and
`/etc/group`.

## The API

```go
u, err := runuser.Resolve(runuser.Layers{
    Image:    runuser.Current(),        // who this process already is
    Manifest: cfg.User,                 // sandbox.json
    Request:  req.User,                 // this call's override
}, sandboxuser.Complete)
// u.UID=1000  u.GID=2000  u.HomeDirectory="/home/dev"  u.GroupName=""
```

| Call | Use for |
| --- | --- |
| `Resolve(Layers, Fields)` | Any identity you are about to launch a process as. |
| `Current()` | The image layer: this process's own ids. |
| `Groups([]string) []uint32` | Supplementary GIDs for a credential, unknown entries dropped. |
| `LookupGroupID(string) (uint32, bool)` | One entry — a name or a numeric GID — to a GID. |
| `LoginShell(name)` | The passwd shell field, which `os/user` does not expose. |

## Declare what you cannot have

`Fields` is the second half of the contract. A caller passes the set it
genuinely needs; anything required but undeterminable is an `*UnresolvedError`
naming the field, and anything not required comes back absent rather than
defaulted.

Leaving a field out is an explicit, greppable claim of "I cannot know this
here". `boot` is the worked example: it asks for `FieldUID|FieldGID` for a
configured user, because the account may not exist until `ensureUser` creates
it, and asks for name and home separately where absence is an acceptable answer.

## Rules it enforces

- **Ask, never default.** A missing id is read from the passwd entry. Never `0`,
  never `gid = uid` — that coincidence is a `useradd` default, not a fact.
- **Only what is missing is looked up.** A complete identity needs no account to
  exist yet, which is what lets the boot flow resolve an account it is about to
  create.
- **Names resolve to ids, one way.** `GroupName` becomes `GID` and is cleared;
  numeric entries resolve as ids before names, so a group named `997` cannot
  shadow gid 997.
- **Membership is the caller's, resolution is the OS's.** Whoever supplied
  `AdditionalGroups` decides who is in what; the group file only turns an entry
  into a number, dropping what the image never created.
- **An error is final.** Callers must not fall back to a default when `Resolve`
  fails; that fallback is the guess this package exists to remove.

## Boundary

This is the **only** package that resolves against the image's account database
(ADR 0032 §6). `execs.userCredential` requires the ids rather than re-deriving
them, and the login-shell parse lives here rather than in `execs`, so faking the
database fakes it for every consumer — including the path that calls `setuid`.

`boot` still shells out to `getent`, deliberately: it *mutates* the database and
must observe its own writes through the same NSS view those tools use, including
before the account exists. That is a different question from resolution.

## Testing

`FixedDatabase() (restore func())` swaps the lookups, the passwd file, and the
effective ids for a fixed table; use it with `t.Cleanup` from any package.
`FixedEffectiveIDs(uid, gid)` overrides just the image layer, for an image
running as a uid with no passwd entry.

Its ids deliberately break `uid == gid` — including the effective ids, because
while those were read from the real process a test could only assert them
against another `os.Getuid` call, which passes for any implementation.

It takes no `*testing.T` on purpose: importing `testing` from a non-test file
registers test flags on every binary that links the package.
