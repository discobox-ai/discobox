# 0025 — The sandbox user is one contract, resolved inside the sandbox

- **Status**: Accepted
- **Date**: 2026-08-07

## Context

`SandboxUser` is one schema used at two layers: `sandbox create` sets the
sandbox's run identity, and `exec create` sets a single exec's. It carries
`name`, `uid`, `gid`, and `homeDirectory` — identity only. Group *membership*
has no place in it at all.

Membership arrives on a separate path. A harness image declares
`additionalGroups` as a label; `harnessconfigs` snapshots it, the reconciler
puts it in the manifest, and the pool agent writes it to `sandbox.json`.
`boot.ensureAdditionalGroups` then materializes those names into the image's
`/etc/group`, and `execs` sets them on each process. Nothing between the API and
that label can express membership, so a caller cannot ask for an exec that runs
with different groups than the sandbox — the common case being an exec that
should run with *fewer*.

Three further problems are visible in the current shape.

`gid` is numeric, but the groups a sandbox cares about are named — `docker`,
`video`. A name cannot be resolved by the control plane or the pool agent
because the group lives in the image's `/etc/group`, which does not exist until
the container does. `boot.ensureGroup` works around this by deriving a name
*from* the number: it looks up whatever group holds `gid`, and failing that
creates one named after the user. The primary group is therefore whatever
happened to hold that id, which is the same "uid and gid are separate
namespaces" mistake corrected elsewhere.

The pool agent chowns one directory on the host with that numeric gid before the
sandbox exists — the git source clone at `prepareOwnedDirectory(sourcePoolPath,
user.uid, user.gid)`; every other call passes `0, 0`. That single call is why
the number has been load-bearing, and `resolveSandboxUser` invents one to feed
it: a missing gid becomes the uid, and a bare user name becomes uid *and* gid
`1000`.

Both inventions are guesses dressed as answers, and a guess is worse here than
an absence: it produces a plausible wrong group silently, where an absence is
visible. The clone path is bind-mounted into the sandbox and `boot.wireSources`
chowns the target from the manifest afterwards, so the *directory's* group is
corrected from inside regardless — but that chown is not recursive, so the
tree's contents keep whatever the host wrote.

A create request with no user at all resolves to **root**
(`sandboxUserIdentity{uid: 0, name: "root"}`). An image that ships its own
account and a `USER` directive has both ignored: the sandbox runs as root
because the caller declined to have an opinion. `ImageMetadata` declares
`additionalGroups` but no user, so nothing else fills the gap either.

Finally, the two layers disagree about what a request means. An exec request
that named a user got no groups at all, because `execUserFromAPI` builds a user
the manifest never touched. The same request that named no user got the
manifest's. Identity and membership were entangled: choosing one silently
chose the other.

## Decision

### 1. `SandboxUser` carries membership, and means the same thing at both layers

`SandboxUser` gains `groupName` and `additionalGroups`. At `sandbox create` it
*defines* the sandbox.json default user. At `exec create` it *overrides* that
user for one exec. There is one schema, one meaning, and one set of rules; a
field is never accepted at one layer and ignored at the other.

### 2. Groups are all-or-nothing, never merged

A request naming no groups inherits the manifest's. A request naming any runs
with exactly those. The two are never unioned.

Merging would make the manifest a floor no caller could get under, so an exec
could never run with fewer groups than its sandbox — which is the main reason to
ask for groups at all. The same rule applies to `sandbox create` against the
image label: a request that names groups replaces the label's, rather than
adding to them.

Membership is read off the request before identity is resolved, so a request
carrying only groups keeps them and still runs as the manifest's user. "The
usual user, plus these groups" is expressible; identity and membership are
independent choices.

### 3. The primary group may be a name, and names resolve inside the sandbox

`groupName` and `gid` are mutually exclusive; supplying both is a 400. Every
entry in `additionalGroups` is a name *or* a numeric GID, resolved by one
function, so the primary group and the supplementary ones cannot resolve by
different rules.

Resolution happens in the sandbox, where `/etc/passwd` and `/etc/group` exist.
This is the rule already applied to shells: which shell a user has is sandbox
knowledge, so an exec asks for one instead of naming it. Which gid a group name
has is the same kind of knowledge.

Whoever supplied the list is the authority on *membership*; the group file is
consulted only to resolve an entry to an id. A numeric entry resolves as an id
first, so a group literally named `997` cannot shadow gid 997, and a bare GID
resolves even with no group-file line — the id is the authority and the file
only names it. An entry that resolves to nothing is skipped rather than fatal,
mirroring `boot.ensureAdditionalGroups`: the two must not disagree about the
same image, and a harness Dockerfile that forgot to install a package must not
break every exec in the sandbox.

### 4. The host invents nothing; unknown ids are left unchanged

`resolveSandboxUser` stops deriving ids it was not given. No `gid = uid`, no
`uid = 1000` for a bare name. What the request did not say stays unset.

One thing genuinely needs numbers on the host: `prepareOwnedDirectory` chowns
the git source clone recursively, and the marker file with it, because the pool
agent writes that tree as root before the sandbox exists and the sandbox user
must be able to write it afterwards. Everything else that looked like a consumer
is not one — `doc.Runtime.User` and `DISCOBOX_USER_*` merely *forward* the
identity, and git runs deliberately as the caller (`{uid: -1, gid: -1}`), not as
the sandbox user. Volume `%UID%`/`%GID%` tokens resolve in `boot/wire.go` against
boot's own ids, never the pool agent's.

So the host chowns what the request actually gave it, and passes `-1` — the
POSIX "leave this field unchanged" sentinel, already used here for identity in
`execidentity.SysProcAttr` and `materializeGitSource` — for anything it was not
given. A numeric `gid` is applied. A `groupName` is not, because the host cannot
resolve it.

This is the same rule as §3 one layer out. `uid == gid` is a `useradd`
coincidence, not a fact about a sandbox, and `1000` is a convention of one
distro family. Guessing either runs the sandbox under whatever identity happened
to hold that number.

**Deferred:** a sandbox created with `groupName` gets a source tree whose
*contents* keep the pool agent's group, since only the mount point is corrected
from inside. The files are uid-owned and the sandbox user reaches them through
the user bits, so nothing is broken today. Revisit when something depends on
group permissions inside a source tree, or when `boot.wireSources` has reason to
chown recursively — at which point it can carry the resolved gid and the gap
closes without changing this decision.

### 5. `SandboxUser` is optional, and omitting it means the image's own user

A create request that supplies no user leaves the manifest's user unset, and
boot provisions no account. The sandbox runs as whatever the image already is —
its `USER` directive and its `/etc/passwd` — and `ExecDefaults` derive from that
account, resolved in the sandbox like everything else in §3.

Defaulting to root instead, as today, overrides an image that had a considered
answer with one it did not ask for, and does so precisely when the caller
expressed no preference. Absent means absent: a field nobody set must not
silently become the most privileged value available.

Each field is independently optional. A request giving only `additionalGroups`
keeps the image's identity (§2), and one giving only `name` leaves the ids to
the image's account rather than inventing them (§4).

### 6. An unknown id is asked of the OS, never defaulted

Wherever an id is missing and `/etc/passwd` can answer, it is looked up. A name
with no ids takes both from its passwd entry. A uid with no gid takes the gid of
*that uid's* entry — its default group. A uid with no entry at all is an error,
not a guess.

Nothing defaults to `0`, and nothing defaults `gid` to `uid`. Both are guesses
that read as answers: `0` is root, and `uid == gid` holds only for accounts a
`useradd` default happened to create. The user's real default group is a fact
the OS already knows, and asking is neither expensive nor ambiguous.

This applies everywhere inside the sandbox, which is everywhere the question can
be answered:

- `boot.resolveIdentity` currently defaults `DISCOBOX_USER_UID` to `0` and
  `DISCOBOX_USER_GID` to the uid. Both stop; an unset id is resolved from the
  account, and an unset uid means the image's own user (§5) rather than root.
- `execs.Manager.resolveUser` resolves the gid at create, so the exec record
  reports the group the process will actually run in instead of leaving it null
  for the launch path to work out privately.
- `execs.userCredential` keeps the same lookup at launch as the last line of
  defense, so no path can reach `setuid` with an invented gid.

§4 is the one exception, and it is the same rule: the pool agent cannot ask,
because the account lives in the image. So it writes nothing (`-1`, leave
unchanged) rather than writing a guess. "Ask the OS where it knows, leave it
alone where it doesn't" — never fabricate in either case.

## Alternatives rejected

**Make `gid` an array, index 0 primary.** It mirrors the CLI flag exactly, but
`SandboxUser` is shared, so `sandbox create`'s gid changes type too, and the
wire stops saying which element is the primary group — a reader has to know the
convention. Two named fields cost one line of validation and are self-describing.

**Resolve group names in the control plane or pool agent.** Neither can. The
group lives in the image's `/etc/group`, and for a group boot itself creates
there is nothing to resolve against until the container is running.

**Keep a host-side numeric fallback (`gid = uid`) so the chown always has a
value.** The chown does need a value, and unlike the directory itself the tree's
contents are not corrected from inside (§4). But the fallback does not produce
the *right* group — it produces whichever group happens to hold the uid, stated
as confidently as a real answer. Leaving the group unchanged is wrong in the
same cases and honest about it: the file's group is visibly the pool agent's
rather than plausibly the sandbox's.

**Make `boot.wireSources` chown recursively so the host never needs a gid.**
This closes the §4 gap completely, and is where the fix belongs if it is ever
needed. Deferred rather than adopted: it puts a recursive chown over a
freshly-cloned tree on the boot path of every sandbox, to correct a group that
nothing currently reads.

**Default the sandbox user to root when a request omits it.** Today's behavior,
rejected in §5. It discards an image's own `USER` exactly when the caller
expressed no preference, and picks the most privileged identity available to do
it.

**Leave a resolved-at-launch gid null in the exec record.** An intermediate
position: never guess, but let the record stay empty and have the launch path
look the gid up privately. Rejected in §6 — the record is what an operator reads
to answer "what did this run as," and a null there is indistinguishable from
"root" or "unknown" at a glance. The lookup that launch would do is available at
create, in the same sandbox, against the same passwd file. Doing it once, early,
and recording the answer costs nothing and makes the record true.

**Let the request add to the manifest's groups.** Rejected in §2: a union has no
way to express fewer.

**Keep membership out of the API and infer it from the image label alone.** This
is today's behavior. It cannot express a per-exec group set, and it left
`execUserFromAPI` producing a user with no groups whenever a caller named one —
a silent, surprising interaction between two fields that should be independent.

**Distinguish "no groups" from "inherit" by nil-versus-empty.** A three-state
field that JSON `omitempty` and Go's `append([]string(nil), ...)` both collapse.
Length is the test instead: zero means inherit. A request cannot ask for "the
sandbox user with no supplementary groups at all"; no caller has wanted it, and
it can be added later as an explicit flag without changing this rule.

## Consequences

`sandbox.json`'s user gains `groupName` and `additionalGroups`, and the image
label's groups become a default the create request can replace rather than the
only source. `boot.ensureGroup` inverts: it resolves a name to an id instead of
deriving a name from an id.

`disco box exec create` and `disco box sandbox create` take groups as a slice
whose first element is the primary group and whose rest are supplementary,
each a name or a numeric GID.

A sandbox created with no user no longer runs as root. It runs as the image's
own account — for most harness images a non-root user, which is the behavior
those images were built expecting. Any caller depending on the old root default
must now ask for root explicitly.

`sandboxUserIdentity` stops being a fully-populated struct. Its ids are unset
when the request did not give them, and the host-side chown passes `-1` for
those, so pool-agent code may no longer assume it holds a usable uid/gid pair.
`runChown` shells out as `chown -R uid:gid`, which cannot express `-1`; it emits
just the uid when the group is unknown.

A source tree in a sandbox created with `groupName` is group-owned by the pool
agent rather than by the sandbox user (§4, deferred). Files remain uid-owned and
reachable through the user bits.

An exec record's `user.gid` is now always populated, where it was omitted for a
request that named only a uid. Anything reading that field sees the real default
group instead of nothing.

A uid with no `/etc/passwd` entry now fails the exec rather than running it
under a fabricated group. That is a new error path for images that reference an
account they never created.
