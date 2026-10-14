# 26-10-14-294 — A skills repository is a source its skills are linked from

- **Status**: Accepted
- **Date**: 2026-10-14
- **Amends**: [26-10-09-395](26-10-09-395-a-sandbox-may-be-created-with-skills.md)
  §3's install order and §4's reading of `--skills`, for a `--skills` that is
  a Git URL.
- **Relates to**: [0055](0055-a-delivered-source-settles-before-its-sandbox-runs.md),
  [0056](0056-a-repository-declares-the-sources-it-is-worked-on-with.md),
  [0072](0072-a-repository-ships-skills-that-only-exist-in-a-sandbox.md),
  [26-10-10-545](26-10-10-545-git-reaches-a-pool-only-from-a-client-and-is-cached-there-per-remote.md).

## Context

`--skills DIR` sends a directory's skills as content in the create request
(ADR 26-10-09-395). The sandbox copies them into its skill directories once,
and from then on they are the harness's files. Nothing an agent changes in
them can leave the sandbox, and the content is capped at 1 MiB in 1000 files.

A team keeps its skills in a Git repository, and wants an agent that improves a
skill while using it to commit the change and push it back, like any other
change. That needs the skills to arrive as a checkout of that repository, not
as a copy of its files.

A sandbox already receives Git repositories: sources. A source reaches the pool
through the client and its per-remote cache (ADR 26-10-10-545), private ones
included. The sandbox waits for it before it runs (ADR 0055), and its `origin`
is the real remote, which the sandbox fetches from and pushes to with the
agent-credentials protocol. What is missing is a way to say that a source is
there for its skills and is not something being worked on.

## Decision

### 1. `--skills URL` is a source; `--skills DIR` is unchanged

`discobox new` (and `sandbox create`) read each `--skills` value by its form:

- **A Git URL** is a skills source, written `URL[@REF][//PATH]`. The CLI
  resolves it as it resolves a remote `-i`, and adds it to the create request
  as an extra source reference.
- **A directory** is read into content, as now, whether or not it is a Git
  checkout.

`--user-skills` stays content.

`//PATH` names the directory in the repository that holds the skills, and is
the common case: skills usually live beside other things, not at a
repository's root. Without it, the root holds them. The CLI splits the value at
the first `//` after the URL's `://`, if it has one. What precedes it is
`URL[@REF]` exactly as `.discobox/sources.json` writes it (ADR 0056 §1). `//`
is unambiguous there, because Git refuses consecutive slashes in a ref name and
a repository URL has none after its scheme. `PATH` may therefore hold `@` and
`:`. It is a relative, clean, slash-separated path inside the repository, with
no `..` segment. The CLI and the server both refuse any other path.

Two `--skills` naming one repository at one ref share one checkout, with
their directories kept in the order given. One repository at two refs is
refused: it would need two checkouts of one name, and nothing needs that.

### 2. The reference is marked with its purpose

`GitSource` gains `purpose`, an enum: `work` (the default, and what an absent
value means) or `skills`. The primary source is never `skills`.

The directories go beside the references, not on them. Source code references
are a map keyed by the directory each lands in, so they hold no order, and §4
needs the order the `--skills` values were given in. `SandboxCreateConfig`
gains `skillsSources`, a list with one entry per `PATH` of §1, in that order:

```json
"skillsSources": [
  {"source": "/workspace/skill-sources/tools",  "directory": "agents/skills"},
  {"source": "/workspace/skill-sources/shared", "directory": "."},
  {"source": "/workspace/skill-sources/tools",  "directory": "extra"}
]
```

`source` is a source code reference key, and `directory` is the `PATH`, with
`.` for the root. The server refuses an entry that names a reference whose
purpose is not `skills`, and a `skills` reference that no entry names. It
persists the list in the manifest, omitted when empty, the way it persists
skills (ADR 26-10-09-395 §2). A sandbox created before this change has neither
field, so its manifest fingerprint is unchanged. The pool resolves each entry
to the checkout its reference lands in, and writes the list, in order, into
`sandbox.json`.

A `skills` source:

- is never the primary source and never the harness's working directory;
- is placed at `<working root>/skill-sources/<name>`, named as a remote `-i` is
  named, so it sits apart from the sources being worked on. A path that collides
  with another source is refused, as two references claiming one directory are
  refused today;
- is delivered, settled and exported like any other source, with `origin` the
  real remote, so a commit made in it is pushed the way a commit in any remote
  source is pushed;
- is shown as a skills source wherever a sandbox's sources are listed;
- does not count toward `MaxSkillsBytes` or `MaxSkillFiles`, since its content
  is not in the bootstrap.

### 3. Its skills are linked, not copied

On the primary terminal's first launch, after the image's, the repository's and
the request's content skills are copied (ADR 26-10-09-395 §3), the sandbox
links each skills source's skills in. Every immediate subdirectory of each of
the source's skills directories that holds a `SKILL.md` is one skill. In each
skill directory (`~/.claude/skills`, `~/.agents/skills`), `<name>` becomes a
symbolic link to `<checkout>/<PATH>/<name>`. A Windows sandbox uses a directory
junction.

The whole repository is checked out, not only `PATH`: a commit is made and
pushed from the checkout, and a skill's scripts may reach outside its own
directory. A `PATH` the pinned commit does not hold, or that is not a
directory, fails the install, naming the source and the path. The client
cannot check it, because when the pool already holds the commit, the client
never fetches the repository's tree.

A link is the point of the change: a harness that edits the skill edits the
checkout, where `git commit` and `git push` reach the skills repository.

The once-only rule stands. A skill added to the repository after the first
launch, or by a later `git pull`, is not linked.

### 4. A linked skill replaces a copied one whole

A link cannot overlay a directory the way one copy overlays another. Where a
skill of the same name was copied in from an earlier layer, the linked skill
replaces that directory. Between two skills directories, of one source or of
two, the later `--skills` wins.

A linked skill also wins over a content skill of the same name, whatever order
the flags came in, which is a deliberate break from ADR 26-10-09-395 §4's
"later wins". When the pool already holds the pinned commit (ADR 26-10-10-545
§3), the client never fetches the repository, so it cannot tell which names the
repository holds. The precedence has to be decided in the sandbox, which sees
only the content skills and the checkout, not the order they were named in.
The sandbox logs each name a link replaced.

## Alternatives rejected

- **Have the CLI clone the repository and send its contents.** This needs no
  API change. But it is still a copy, so an edit cannot be pushed back, and it
  keeps the 1 MiB cap.
- **Copy the skills out of the checkout instead of linking them.** That keeps
  the copy-and-overlay rule, but the copy the harness edits is not the checkout,
  and moving the edits back would be manual.
- **Treat a local directory that is a Git checkout as a skills source.** It is
  the natural next step, but today `--skills ./dir` means content. Changing that
  for every directory that happens to sit in a repository would change what
  working commands do. Deferred, below.
- **`URL@REF:PATH`, or `URL#PATH`, for the directory.** `:` is already the
  separator in scp-style URLs (`git@github.com:acme/tools`), so a URL with no
  ref would be ambiguous. `#` is legal in a ref name, and Git drops a URL's
  fragment.
- **A separate `--skills-path` flag.** With `--skills` repeated, nothing says
  which repository a path belongs to.
- **Check out only `PATH` (a sparse checkout).** It is smaller, but a skill's
  scripts may reach elsewhere in the repository, and a sparse checkout is
  easy to commit from wrongly.
- **Carry the directories on each reference, with an order number.** It keeps
  a skills source's directories beside it, but an order number is a list
  spelled out by hand, and one number per reference cannot order one
  repository's directories among another's.
- **Make source code references a list.** It would carry the order without a
  second field, but it changes a public request and response shape and a
  stored column of every existing sandbox, for an order only skills need.
- **Infer the purpose from the destination path.** Anything that reads a
  sandbox's sources would have to know the convention. A field says it once,
  where the server and every client can read it.
- **Have the client resolve precedence by reading the repository's tree.** That
  keeps "later wins" across both kinds of skills, but it forces a fetch to the
  client on every create, including the ones the pool's cache could answer by
  pinning alone.

## Consequences

- `GitSource` gains `purpose` in the server API. `SandboxCreateConfig` and
  the sandbox's read config gain `skillsSources`, which the store keeps in its
  own manifest column. The pool create request gains the same list, and
  `sandboxconfig`'s runtime layer gains it resolved to checkout paths. The
  sandbox agent reads it.
- `sandbox-agent/terminal/skills.go` gains a fourth, linking layer, and its
  `DESIGN.md` describes the order.
- A sandbox with skills sources parks in `awaiting_source` until they land, as
  it does for any source.
- `discobox new --skills URL` with no primary source creates a sandbox whose
  only source is a skills source.

## Deferred

- **A local checkout as a skills source.** Revisit when someone wants to push
  edits back to a skills repository that is checked out on their machine and
  not reachable as a URL, or when `--skills DIR` meaning content is more often
  a surprise than what was wanted.
- **Re-linking after a pull**, so a skill added upstream appears without
  re-creating the sandbox. Revisit if teams update skills repositories mid-task.
