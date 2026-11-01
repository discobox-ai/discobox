# 26-11-01-692 — A skills source is checked out under home

- **Status**: Accepted
- **Date**: 2026-11-01
- **Supersedes in part**:
  [26-10-14-294](26-10-14-294-a-skills-repository-is-a-source-its-skills-are-linked-from.md)
  §2's placement of a `skills` source at `<working root>/skill-sources/<name>`.
- **Relates to**:
  [0145](0145-a-sandbox-declares-its-platform-and-a-non-linux-one-is-a-vm-template.md)
  §6 (a sandbox path is judged by the sandbox's platform),
  [26-10-15-417](26-10-15-417-a-sandbox-container-has-no-working-directory.md)
  (a path handed to a host tool is absolute).

## Context

A skills source is checked out at `<working root>/skill-sources/<name>` so it
sits apart from the sources being worked on (ADR 26-10-14-294 §2). That only
holds when the work is in a source of its own. A discobox with no primary
source works in the working root itself (`/workspace`), and its skills
checkout then shows up as `/workspace/skill-sources`, inside the directory the
harness works in. It sits under the work, not beside it.

The checkout should be somewhere the work never is, and it has to persist.
Persistence comes from where a source's content lives, not from where it is
mounted: a container's checkout is on the sources volume
(`/.discobox/sources/<slug>`) and is bind-mounted onto its target at every
boot, after the home volume is mounted, and a VM's disk is all persistent. Any
target that is wired after home survives, and the user's home is the natural
place.

But only the sandbox knows the home. `discobox run` on Linux or macOS sends
the host's home, a Windows client sends none, `admin box create` sends no user
at all, and an image may resolve a home of its own. The pool knows it only
when the request names it. The CLI picks the key, and today every key is an
absolute path. Neither the pool nor the sandbox would carry a `~` correctly:
the pool's `sandboxpath.Rooted` makes `~/x` into a literal `/~/x` on POSIX and
into nothing on Windows, where the source falls back to `<working root>\<slug>`.
The sandbox's intake refuses any target that is not absolute.

## Decision

### 1. A skills source's key is `~/.discobox/skill-sources/<name>`

The CLI places a `skills` source at `~/.discobox/skill-sources/<name>`. The
name is the same one a remote `-i` gets. The key and the reference's
`destination.directory` are that string, written with `/` on every platform.
`~` stands for the sandbox user's home, and nothing outside the sandbox
expands it.

The key is home-relative only on a `skills` reference. The server refuses a
`~` key, or a `~` `destination.directory`, on the primary source and on any
`work` reference. A work source's directory reaches host tools (an editor's
`ssh-remote+` folder, `{workdir}`), and those take only an absolute path (ADR
26-10-15-417). A `skills` reference's key is either absolute (the form every
existing sandbox holds) or `~/` followed by a clean, relative, slash-separated
path with no `..`. The server refuses any other key, at create and at
import. The pool refuses a `~` on anything a skills source does not name, and
places every other directory as it always has, so a sandbox created before
this, created anew from what it holds, is placed where it was.

### 2. The pool carries `~` through untouched

The pool resolves a reference's target from its key and destination as it
does today, except that a `~/` path is kept as written and not rooted. It goes
into `sandbox.json` (`runtime.sources[].target`,
`runtime.skillsSources[].target`) and into the runtime-config document as it
is. The sources volume is keyed by slug, so nothing there changes.

### 3. The sandbox agent expands `~` once, when it reads the config

`sandboxconfig` validates a target as absolute **or** `~/<clean relative
path>`, and gains one expansion that rewrites each `~/` target against a home,
joined by the sandbox's own path rules. The sandbox agent applies it once per
reading:

- where boot reads `sandbox.json`, with the home `resolveIdentity` resolves;
- where the agent builds its runtime from the bootstrap, with the home every
  exec starts in (`runUserHome`, which the meta file and wakes already use);
- where the converger takes a runtime-config document, with that same home.

The intake keeps a document as it was delivered and validates it as such: a
retried delivery has to compare equal to the document held, and an expanded
copy would not. The converger is where a delivered target is first used, so
it expands each document as it receives it and works from the expanded copy.

Everything else sees absolute targets: wiring, the direnv whitelist, skill
links, the git route and status. None of them learns about `~`. A home that
does not resolve leaves a target unexpanded and logged: convergence refuses
it, naming why, so the source is never cloned, and the rest find no checkout
there. Such a sandbox's user has no home to work in either, so nothing more
is done to keep it going.

The directories on the way to a home target are created and given to the
user: by `wireSources` in a container, as it does for any target now, and by
convergence on a VM, where nothing creates a non-primary target before it
clones today.

### 4. Existing sandboxes are untouched

A sandbox created before this keeps its absolute
`/workspace/skill-sources/<name>` keys, which stay valid. Its manifest
fingerprint does not change. Nothing is migrated.

## Alternatives rejected

- **The CLI writes the home it sends.** That is
  `<identity home>/.discobox/skill-sources/<name>`, falling back to
  `/home/<name>`. It is a CLI-only change, but it is a guess. `admin box
  create` sends no user, a Windows client sends no home, and an image can pick
  a home other than the one the CLI assumed. A wrong guess puts the checkout
  in a directory nobody owns, outside home.
- **A fixed absolute path off the working root,** such as
  `/var/lib/discobox/skill-sources`. That is one constant, and it persists for
  the same reason home does. But it is not the user's, it means nothing on a
  macOS sandbox, whose root volume is read-only, or a Windows one, and a
  person looking for the checkout has no reason to look there.
- **The pool expands `~`.** The pool knows the home only when the request
  names it, and it would still have to defer to the sandbox otherwise. Two
  places expanding one token is how they disagree.
- **Let any reference be home-relative.** A work source under `~` would be
  refused by every host tool its working directory is handed to. Nothing
  needs it yet.
- **A `%HOME%` token, as image volumes use.** `~` is what a person reads and
  types, and it is what the files API and an exec's working directory already
  accept. `%HOME%` belongs to an image's declaration and is never displayed.

## Consequences

- `cli/internal/sandboxcreate` places skills sources under
  `~/.discobox/skill-sources`. `remoteRunDestination` no longer needs to
  steer a remote primary off the name `skill-sources`.
- The server gains key validation for references, which it does not have
  today: absolute, or `~/` on a `skills` reference only.
- The pool's `sandboxSources` keeps a `~/` target as it is, and its create
  validation refuses a `~` that no skills source names.
- `sandboxconfig` gains the `~/` target form and its expansion. The sandbox
  agent's boot, runtime and converger apply the expansion. Convergence
  creates a missing target's parent directories for the user.
- A pool older than this would root a `~/` key into a literal `/~/...`
  directory. A pool upgrades itself to its server's release, so a create
  meets such a pool only while that pool is being upgraded; nothing
  negotiates around it.
- The client cannot detect a skills key that collides with a mirrored local
  source under home, because it cannot compare `~/…` with `/home/<user>/…`.
  Such a collision needs a local repository at
  `~/.discobox/skill-sources/<name>`, which nobody has.
- `--source <skills slug>` for an editor folder is refused as not absolute, as
  ADR 26-10-15-417 requires. A skills source is not something to open as the
  workspace.
