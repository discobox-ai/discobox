# Projects Design

`internal/resources/projects` owns the Project resource: the ownership and
membership boundary every other resource is scoped to, and the default-project
flag the `default` alias resolves.

- `/projects` admits any authenticated principal. Listing is scoped to the
  caller's memberships when the `internal/auth` principal is a user; a non-user
  principal lists every project.
- Create and set-default require a user principal, checked in the service. The
  creating user becomes the project's owner and only member. Every
  `/projects/{id}` route (get, update, delete, set-default) reaches this
  package already gated by `auth.ProjectAuthorizer`, which requires a user
  member and resolves `default` to that user's default project.
- A project is addressed by ID. Its name is the only human-facing handle, and
  is unique per owner (`idx_project_owner_name`) so clients can resolve it
  unambiguously; create and rename check it first to report a conflict rather
  than an index violation. There is no slug (ADR 0023 §5).
- `UpdateProject` edits only the project's own settings: name,
  `ArchiveRetentionSeconds`, `SandboxUpgradePolicy` (`automatic` or `manual`),
  and `Welcomed`. Zero retention and an empty policy are values, not absence:
  they restore the server default, which the project then tracks (ADR 0022 §4,
  ADR 0082 §3). Membership and ownership are not editable here.
- `SetDefaultProject` moves the flag; there is no unset, since `default` is the
  CLI's `--project` default and must always resolve. The move is one
  transaction in `store.SetDefaultProjectForUser`.
- `DeleteProject` refuses the default project and any project still holding
  sandboxes or pools; those own runtime that has to drain through its own
  reconcilers. Once empty, `store.DeleteProject` removes the project's own
  configuration rows.
- `Project.Welcomed` records that the launcher has shown its introduction. It is
  a project row rather than client-side state so the welcome does not repeat on
  a second machine, and is settable both ways through `UpdateProject` — clearing
  it is how someone asks to be shown it again.
- The default project itself is created by `internal/service`'s
  `InitializeDefaults`, which is startup policy: it also owns the one-time
  provider/pool installation gated on `server_state`.

## Copying (`copy.go`)

Creation seeds the built-in harnesses into every project, then optionally
copies providers, pools, and configured harnesses from a source project the
caller is a member of. See
[ADR 0023](../../../../docs/adr/0023-projects-are-created-by-copy-and-deleted-only-when-empty.md)
for what is copied and why.

- The copy plan is resolved and authorized before the project row is written.
  `copyFromProjectId` accepts `default`, resolved here because `POST /projects`
  has no project path for the authorizer to resolve. Selecting pools implies
  providers: a pool binds to a provider instance in its own project.
- Provider instances and pools are created through the owning services
  (`ProviderInstances`, `Pools` — consumer-side interfaces satisfied by those
  resource packages) so create-time behavior still runs: provider config
  validation and instance resolution, pool reconcile scheduling. Built-in
  seeding goes through `HarnessConfigs.SeedBuiltIns`.
- Harness config copies, their secrets, bindings, and harness-config-scoped
  grants are written through the store. A seeded built-in takes only the
  source's configured state, never its image; a user-created config is copied
  whole. Each secret is opened and re-sealed under a new ID, once per copy.
  Reads come straight from the store.

Ordering is load-bearing: providers and harnesses are database-only and roll
the project back on failure, so pools — which schedule real hosts — run last. A
pool failure leaves the project and the pools already created in place.
