# 0023 — A project is created by copying an existing one, and deleted only when empty

- **Status**: Proposed
- **Date**: 2026-08-06

## Context

Projects have existed as the ownership boundary since the first schema, but the
API only listed and read them. Exactly one project — the one `InitializeDefaults`
seeds at first boot — has ever existed at runtime, and `-p default` resolves it
through the `Project.Default` flag.

Making projects CRUD-able raises two questions the resource model does not
answer on its own.

**A bare project is useless.** Everything `disco run` needs is project-scoped:
provider instances, pools, harness configs, and the secrets harness configs
bind. A project created empty can list nothing and run nothing, and the user's
next twenty minutes are spent reproducing, by hand, configuration they already
have next door. The default project avoids this only because
`ensureDefaultSandboxProviderInstalled` seeds a provider and pool for it, once,
gated on a `server_state` row.

**A project is not a leaf.** It transitively owns runtime: pools are hosts,
sandboxes are containers with volumes. Deleting the row is trivial; deciding
what happens to the hosts is not.

## Decision

### 1. Create seeds the built-ins, and copies the rest on request

Every project — default or not — is seeded with the built-in harness configs
against the server's current images, unconfigured. That is the catalog, not
configuration, and a project without it cannot show the user what they could
run.

Beyond that, `POST /projects` takes `copyFromProjectId` and a `copy` selection
of `providers`, `pools`, and `harnesses`. Omitting `copy` takes all three;
an empty array takes none.

Rejected: **re-running the default-project seeding for each new project**
(install the OS-default provider and a pool). It ignores what the user actually
configured — a remote provider, a sized pool — and reintroduces the one-time
`server_state` gate for something that now happens many times.

Rejected: **create nothing, make the user wire it up**. Correct and useless;
see above.

### 2. Copying a harness config carries its credentials, not its image

A copied provider instance keeps its type, name, and config. A copied pool is
recreated against the copied provider — a genuinely new host, not a reference —
and the source's default-pool choice is carried across.

A copied harness config keeps only what the configure flow produced: its
`Configured` flag, its configured files, the secrets it minted, and the
harness-config-scoped grants over them. Its **image is not copied**: the
destination's built-ins were just seeded from the server's current images, and
copying the source's pinned image would silently pin a new project to whatever
the source happened to be seeded from — possibly months stale. A user-created
(non-built-in) harness config is copied whole, image included, because there
the image is its identity.

Secrets are copied rather than shared. Sealing binds ciphertext to
`projectID/secretID`, so the value is opened and re-sealed under the copy's
identity. Sharing a secret row across projects would put one project's
credential inside another project's authorization boundary, which is the one
thing the project boundary exists to prevent.

Rejected: **copy the harness config but not its secrets**, leaving bindings
dangling. The copy would look configured and fail at the first resolve.

### 3. Delete refuses a non-empty project

`DELETE /projects/{id}` refuses (409) a project that still has sandboxes or
pools, and refuses the default project outright. Once empty, it removes the
project's own configuration rows — harness configs and their bindings,
provider instances, secrets, grants, requests, memberships.

Rejected: **cascade**. A project delete would have to submit delete intent for
every sandbox and pool and then wait for their reconcilers, meaning a
long-running, partially-failed delete with no obvious resume point, expressed
through an endpoint that returns 204. Pools already refuse to delete while they
hold sandboxes for the same reason; a project inherits that rule rather than
inventing an exception to it. `--force` was rejected with it: it is the same
cascade behind a flag.

Rejected: **allowing the default project to be deleted, promoting another
automatically**. `default` is resolved by a flag the user sets; silently moving
it during a delete makes every subsequent unqualified command act on a project
the user never chose.

### 4. The default is set, never unset

`PUT /projects/{id}/default` moves the flag; there is no `DELETE`. Pools model
an unset default (a project may legitimately have no default pool, forcing
`--pool`), but a user with no default project has no answer for `-p default`,
which is the flag's default value — so every unqualified command would fail.
The flag moves atomically: clearing the old and setting the new in one
transaction, because a crash between them would leave the user with two
defaults or none.

### 5. A project has an ID and a name; the slug is removed

`Project.Slug` is deleted. It was write-only: `InitializeDefaults` set it to
`"default"`, tests set it to `"project"`, and no code ever read it. Nothing
addressed a project by slug — `/projects/default` resolves through the
`Default` flag, and every other route takes an ID.

Worse, its one live value actively misled. The seeded project's slug being
`"default"` reads as the mechanism behind `-p default` when it is a
coincidence. The two would come apart the first time anyone moved the default
flag: `-p default` would follow the flag to the new project while the old one
kept the slug, so the same word would name two different projects depending on
who was reading it.

A project is therefore addressed by ID. Its name is a display label that
clients may also accept as a selector, and it is unique per owner so that
selection is unambiguous — the same rule pools already apply to their names
within a project.

Rejected: **keeping the slug and reserving `"default"`** as a value. That
preserves a second identifier with no reader, and buys a rule everyone has to
be told about. Rejected with it: making the slug a real addressing key the
server resolves in the path. That is a genuine feature, but it is a URL design
decision for a multi-tenant HTTP API that does not exist yet, and inventing it
now to justify a column nothing reads is backwards.

## Consequences

- Project creation is not atomic end to end. The provider and harness copies
  are database-only and roll the project back on failure; pools are created
  last, because a pool schedules a real host. A failure part-way through pool
  creation leaves the project and its already-created pools in place, for the
  user to inspect and delete, rather than orphaning hosts nothing points at.
- Copying is a point-in-time snapshot. Nothing links the copy to its source:
  later changes on either side do not propagate.
- Sandbox-scoped grants are not copied. The new project has no sandboxes.
- Project names are now unique per owner, so a rename can conflict. Two
  projects that only ever differed by slug are no longer expressible.
