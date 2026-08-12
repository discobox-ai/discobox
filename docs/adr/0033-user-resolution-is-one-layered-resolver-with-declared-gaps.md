# 0033 — User resolution is one layered resolver, and every gap is declared

- **Status**: Accepted
- **Date**: 2026-08-12
- **Extends**: [0025](0025-the-sandbox-user-is-one-contract-resolved-inside-the-sandbox.md),
  whose §6 launch-time re-lookup this supersedes (§6 below)

## Context

ADR 0025 settled *what* the rules are: groups are all-or-nothing, names resolve
inside the sandbox, the host invents nothing, absent means absent, and an
unknown id is asked of the OS rather than defaulted. Those rules are still
right. This ADR is not about them.

It is about the fact that they keep being broken, one site at a time, by people
who had read them.

The rules live as prose that each site applies for itself. There are five sites,
and each one re-implements the same three steps — decide whether a layer named
anybody, merge the layers, complete the result — with its own code:

| Site | Where | Does |
| --- | --- | --- |
| `resolveSandboxUser` | `pool-agent/sandboxruntime/runtime.go` | forwards the request, outside |
| `resolveIdentity` | `sandbox-agent/boot/user.go` | reads `DISCOBOX_USER_*`, inside |
| `wireSources` | `sandbox-agent/boot/wire.go` | chowns sources from manifest numbers |
| `execDefaultsFromEffective` / `execDefaultUser` | `sandbox-agent/config`, `sandbox-agent/server` | manifest → default user |
| `Manager.resolveUser` | `sandbox-agent/execs/manager.go` | request over manifest |

`runuser.Resolve` sits beneath all five, but it is a **completion** function: it
fills what is missing in one already-merged identity. Every site writes its own
merge and then calls it. The merge is where the bugs are, and it is the only
part of this with no owner.

Three defects found together, all of them the same shape:

**The "did this layer name anybody?" predicate is written twice and disagrees
with itself.** `boot.resolveIdentity` asks `spec.Empty() && spec.Group == ""`.
`execs.Manager.resolveUser` asked `user.Empty()`. `Empty()` ignores `Group`, so
an exec naming only a primary group — `--group docker` with no `--user`, the
shape the CLI actually emits — fell back to the manifest user and dropped the
group silently. Boot had the extra clause. Execs did not. Same question, two
implementations, one wrong.

**`derefID(nil)` returns `0`.** ADR 0025 §4 specifies `-1`, the POSIX "leave
unchanged" sentinel, for anything the host was not given. `chownRecursive` in
the same file honours it exactly, under a comment that states the rule
correctly: *"Nothing was given, so there is nothing to assert. Leaving ownership
alone is the honest answer; the sandbox sets it once it can resolve."* Twelve
lines away, `derefID` flattens the same unset value to `0` on its way into
`sandboxconfig.Source`, whose `UID`/`GID` are plain `int64` and cannot express
absent. `boot.wireSources` then chowns the primary source tree to `root:root`.
The correct policy was already written down, in that file, and held at one call
site and not the next.

**Two group lists disagree about which is authoritative.** `boot` adds the
account to `effective.AdditionalGroups` (the image label). `execs` prefers
`effective.User.AdditionalGroups` and falls back to the label. A manifest that
declares groups gets them into every exec's credential while the OS account was
never added to them.

None of these are exotic. Each is one site applying a rule the way its author
read it, and no mechanism anywhere that could have caught the divergence.

Underneath the five merge sites is a second, quieter duplication: **four
components read `/etc/passwd` and `/etc/group`, by three different mechanisms.**

| Component | Mechanism | Reachable by the test fixture |
| --- | --- | --- |
| `runuser` | `os/user`, behind swappable vars | yes |
| `execs.userCredential` | `os/user`, called directly | **no** |
| `execs/shell.go` | direct parse of `passwdPath`; `osuser.Current()` | the parse only |
| `boot/user.go` | `getent passwd`, `getent group`, `id -u` | yes, via `booter` |

`runuser`'s design doc claims it "answers one question for everything inside a
sandbox that launches a process as somebody." That is true about the *boundary*
— nothing outside the sandbox reads these files — but not about exclusivity.
`userCredential` re-implements name→uid/gid and uid→gid lookup at launch, which
ADR 0025 §6 asked for explicitly as a last line of defense against reaching
`setuid` with an invented gid.

The cost of that second implementation is not the duplication; it is that
`FixedDatabase` cannot reach it. A test can fake the whole passwd database,
watch resolution produce the right answer, and still have the launch path
consult the developer's real `/etc/passwd` — which is why `process_unix_test.go`
asserts against `os.Getuid()`, a tautology that passes for any implementation.
Two readers means the fixture covers the one that is easy to fake and not the
one that actually calls `setuid`.

There is a further tension ADR 0025 did not name. §4 says the pool agent must
not resolve, which is correct — the account may not exist until boot's `useradd`
creates it. But the pool agent cannot fully abstain either: it publishes the
manifest, expands `%HOME%` in harness env, and sets container `HOME`. It is
required to commit to values it is structurally unable to determine. Handed that
contradiction with no way to express it, `resolveSandboxUser` did the natural
thing and guessed — `path.Join("/home", out.name)` — directly beneath a comment
reading "Nothing is invented."

## Decision

### 1. One resolver owns the merge, not just the completion

`runuser` gains the layered entry point. The three inputs are named, ordered
general to specific, and a nil layer named nobody:

```go
type Layers struct {
    Image    *User // who this process already is; inside the sandbox only
    Manifest *User // sandbox.json
    Request  *User // the per-call override
}
```

One precedence rule, stated once and implemented once:

> Identity is chosen **whole, by layer** — the most specific layer that names
> anybody supplies all of it. Each group list is chosen whole and independently,
> by the same rule. Completion against `/etc/passwd` and `/etc/group` happens
> once, at the end, inside the sandbox.

"Named anybody" is one exported predicate with one definition. Adding a field to
`User` means teaching that predicate about it, in one place, instead of
discovering months later which of five sites forgot.

Sites 2–5 lose their local merge. `boot` keeps only what is genuinely its own:
creating the account, the group, and the sudoers drop-in.

### 2. Resolution is total, and a caller declares the fields it cannot have

A caller states which fields it requires. Every required field is either
resolved or an error naming it and saying why. There is no third outcome, and
in particular no zero value standing in for an answer.

```go
func Resolve(l Layers, need Fields) (User, error)
```

A caller that knows a field is undeterminable in its context must leave it out
of `need`, which is an explicit, greppable, reviewable statement of "I cannot
know this." Fields outside `need` come back nil — never zero, never `""`, never
a plausible default.

This is the mechanism the previous three defects all lacked. `derefID` could not
have returned `0`, because `0` for an unset uid would have had to be written as
`need: UID` against a layer set that cannot supply it, and failed. The invented
`/home/<name>` could not have been written, because the pool agent has no
standing to ask for `Home` at all.

Defaults are how this class of bug enters: `0` is root, `uid == gid` is a
`useradd` coincidence, `/home/<name>` is a distro convention. Each reads as an
answer at every call site downstream. An error at the one site that lacked the
information is louder and lands on whoever can actually fix it.

### 3. Absent is nil, end to end, including on the wire

The `-1` sentinel and the bare-`int64` manifest fields both go. `Absent` has one
representation — a nil pointer — from the API through `sandbox.json` to the
launch path. `sandboxconfig.Source.UID`/`GID` become `*int64`.

Sentinels are only ever as good as every conversion between them, and
`derefID` is the proof: two encodings of absent, converted at a boundary, one
of which collides with a real and maximally privileged value.

`-1` remains what is passed to `chown(2)`, where it is that syscall's own
vocabulary for "leave unchanged". It stops being how the codebase represents
absence to itself.

### 4. The host cannot complete, by type rather than by convention

The pool agent gets merge-only:

```go
func Merge(l Layers) User // precedence only; no lookups, so no guesses
```

It has no `Image` layer to pass, performs no lookups, and therefore cannot
invent. ADR 0025 §4 becomes a property of the API the host is given rather than
a rule the host is asked to remember.

### 5. What the host cannot determine, it defers rather than approximates

Whatever the host cannot resolve is left for the sandbox, as an unexpanded token
or an absent field:

- Source ownership is published only when the request actually gave ids.
  Otherwise `Source.UID`/`GID` are nil and `boot.wireSources` chowns with the
  identity it resolved — which it already has in hand at that moment and
  currently ignores in favour of the manifest's number.
- `%HOME%` in harness env is expanded inside the sandbox, not by the pool agent
  against a guessed home.

This is an established pattern here, not a new one: `sandboxconfig`'s
`%LOCAL_SUBNETS%` is deliberately left unexpanded for exactly this reason — the
pool agent cannot know the sandbox's own networks, so it forwards a token rather
than a plausible list. `%HOME%` is the same question about a different file.

### 6. `runuser` is the only component that resolves against `/etc/passwd` and `/etc/group`

One reader, one seam. Everything else receives values already resolved.

**`execs.userCredential` stops looking anything up.** Under §2 it no longer has
to: `Resolve` guarantees that a requested field is filled or the call failed, so
the launch path requires `UID` and `GID` to be non-nil and refuses if they are
not. A missing id becomes an assertion about a broken invariant instead of a
second lookup with its own copy of the policy.

This supersedes ADR 0025 §6's launch-time re-lookup, and it is a stronger
guarantee than the one it replaces, not a weaker one. The re-lookup could only
ever catch a *missing* gid; it could not catch a *wrong* one, since it had no
way to tell an id resolved by policy from an id someone invented upstream. An
invariant that must hold by construction, asserted at the boundary, covers both
— and unlike the re-lookup it holds under the test fixture rather than against
whatever `/etc/passwd` the test host happens to have.

**The shell-field parse moves into `runuser`.** `os/user` does not expose the
login shell, so something genuinely must parse `/etc/passwd` directly; that is a
real constraint, not an accident. But it is the same file, and two packages
should not each carry knowledge of its format. `runuser` gains the shell lookup
and `execs/shell.go` becomes a consumer, as `execs` already is for identity.
`osuser.Current()` goes with it — "who is this process" is the `Image` layer of
§1, which `runuser` now owns.

**`boot` keeps `getent`.** It is the one component that *mutates* the database —
`useradd`, `groupadd`, `groupmod`, `usermod` — and it must observe the result
through the same NSS view those tools write, including in the window before the
account exists at all. "Does this name already exist, so should I create or
align it?" is a different question from "who does this identity resolve to," and
a subprocess against the system's own tooling is the right way to ask it. Boot
uses `runuser` for resolution, as it does today, and `getent` only for the
existence checks that drive its mutations.

### 7. One test owns the matrix

The layer matrix — image × manifest × request, across identity, primary group,
and supplementary groups — is one table-driven test in `runuser`, not a corner
each site tests for itself. `FixedDatabase` gains an overridable effective
uid/gid so the `Image` layer is fakeable; today `os.Getuid()` is called directly
and asserted against itself, which passes for any implementation and is
tautological on the usual `uid == gid` developer account.

§6 is what makes this test meaningful rather than merely present. With one
reader behind one seam, faking the database fakes it for every consumer,
including the path that calls `setuid`. While two readers exist, a green matrix
test says only that the fakeable half is correct.

## Alternatives rejected

**Keep the per-site merges and rely on the rules being read.** This is the
status quo, and 0025's rules are unusually well written — yet the `Group`
predicate and `derefID` both entered the tree after it, past review, from
authors who had read it. Prose cannot make five implementations agree; the
disagreement is invisible until a user reports it. Rules that must be
re-implemented per site will diverge per site.

**Have the pool agent inspect the image to resolve names.** It can read a
`USER` directive and the `/etc/passwd` shipped in the layers, which covers the
common case honestly rather than by guessing. Rejected because it is wrong
exactly where it matters: the account frequently does not exist until boot's
`useradd` creates it, so the interesting case is precisely the one layer
inspection cannot see — and a resolver that is right for pre-existing accounts
and silently wrong for created ones is worse than one that abstains, because
nothing downstream can tell the two apart.

**Resolve at the control plane and store the answer.** Same blindness, plus the
answer is now persisted and outlives the image it described. An upgrade to an
image whose account differs leaves a stored identity that is confidently stale.

**Make every field required, so nothing is ever unresolved.** It removes the
opt-out and with it the ambiguity, but it does not remove the missing
information — it relocates the guess to the caller, who has strictly less
context than the resolver. The pool agent would be required to supply a home
directory it cannot know, which is how `/home/<name>` was written in the first
place.

**Keep `userCredential`'s independent lookup as defense in depth.** Two
implementations disagreeing is how a bug gets *caught*, and this one guards the
`setuid` call itself — the most consequential line in the system. Rejected
because it does not actually guard it. The re-lookup fires only when an id is
absent, so it catches a missing gid and is blind to a wrong one; an id invented
upstream arrives fully populated and passes straight through. Meanwhile it is
the reason the fixture cannot reach the launch path, so the defense costs the
test coverage that would catch the wrong-id case it cannot. Defense in depth is
worth real duplication when the layers fail independently; here the second layer
is blind to the failure mode the first one has, and blinds the tests as well.

**Move `boot` onto `runuser` too, for one reader with no exceptions.** Tempting
for symmetry, and it would make the rule absolute rather than "one resolver plus
boot's existence checks." Rejected because boot is asking a genuinely different
question. It reads to decide whether to `useradd` or `usermod`, in the window
where the account may not exist yet and where its own mutations must be visible
to the next read. Routing that through a resolver whose contract is "complete
this identity" would either need a parallel existence-check API on `runuser` —
the same duplication with a longer import path — or would tempt boot into
treating "does not resolve" as "does not exist," which are not the same thing
and whose conflation is what ADR 0025 §6 already had to correct once.

**Return a wrapper type whose unresolved fields are unreadable.** Genuinely
safer than nil discipline, and it makes §2 enforceable by the compiler rather
than by review. Rejected as disproportionate: it puts an accessor call on every
read of a type used across five packages, to guard a nil that — once §3 removes
the sentinels — no longer converts to anything dangerous. Revisit if a field
outside `need` is ever read as an answer again.

## Deferred

ADR 0025 §4's deferred item stands unchanged: a source tree's *contents* keep
the pool agent's group, since only the mount point is corrected from inside.
§5 here narrows the gap — `wireSources` will chown with a resolved identity
rather than a manifest number — but does not make that chown recursive. The
revisit condition is unchanged: when something depends on group permissions
inside a source tree.
