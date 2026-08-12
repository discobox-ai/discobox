# sandboxuser

The identity a sandbox process runs as: the type, the precedence between the
layers that describe it, and the vocabulary for saying which parts of it a
caller needs.

Decision records: [ADR 0025](../docs/adr/0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md)
(the rules), [ADR 0032](../docs/adr/0032-user-resolution-is-one-layered-resolver-with-declared-gaps.md)
(where they live).

## Why it is in the root module

Completing an identity means reading the image's own `/etc/passwd` and
`/etc/group`. Only code inside the sandbox can do that, so completion lives in
[`sandbox-agent/runuser`](../sandbox-agent/runuser/DESIGN.md) and this package is
the half that is safe everywhere else.

The split is load-bearing, not tidy. `pool-agent` imports this module and
*cannot* import `sandbox-agent`, so "the host must not resolve" is enforced by
the build graph rather than by a rule someone has to remember.

```mermaid
graph TD
    SU["sandboxuser<br/>(root module)<br/>type · precedence · Fields"]
    RU["sandbox-agent/runuser<br/>completion vs /etc"]
    PA["pool-agent<br/>Merge only"]
    EX["sandbox-agent/execs · boot · terminal"]
    SU --> RU
    SU --> PA
    RU --> EX
    PA -. "cannot import" .-> RU
```

## The three facets

An identity is three independent choices, each taken whole from the most
specific layer that names it:

| Facet | Fields | Crosses an identity change? |
| --- | --- | --- |
| Who to run as | `Name`, `UID`, `HomeDirectory` | — |
| Primary group | `GID` or `GroupName` | **No** |
| Supplementary groups | `AdditionalGroups` | Yes |

Choosing each facet whole is what makes a partial request expressible: "the
usual user, but in group `docker`" and "the usual user, plus these groups" each
say something about one facet and nothing about the others.

The primary group does not outlive the identity above it — inheriting a gid
across a change of user would run user A's process in user B's default group.
Supplementary groups do, deliberately: they describe what the *sandbox* may
reach rather than who it is, so naming a user must not silently strip them.

## Rules

- **One predicate.** `Named` (and the per-facet `NamesIdentity` /
  `NamesPrimaryGroup` / `NamesGroups`) is the only test for "did this layer say
  anything". Adding a field to `User` means teaching that function, not five
  call sites.
- **Absent is nil.** Never `0` (which is root), never `-1`, never `""`. `-1`
  appears only as an argument to `chown(2)`, whose own vocabulary it is.
- **An empty group list is not a choice.** Groups are all-or-nothing, so "none
  named" inherits and only a non-empty list replaces.
- **Merge cannot guess**, because it cannot look anything up. That is the point
  of it being a separate function from `Resolve`.
