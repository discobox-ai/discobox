# Projects Design

`internal/resources/projects` owns the Project resource: the ownership and
membership boundary every other resource is scoped to, and the default-project
flag the `default` alias resolves.

- Project listing uses the authenticated user principal from `internal/auth` to
  scope results.
- Create, update, delete, and set-default all require a user principal. The
  creating user becomes the project's owner and only member.
- A project is addressed by ID. Its name is the only human-facing handle, and
  is unique per owner (`idx_project_owner_name`) so clients can resolve it
  unambiguously; create and rename check it first to report a conflict rather
  than an index violation. There is no slug (ADR 0023 §5).
- `SetDefaultProject` moves the flag; there is no unset, since `-p default` is
  the CLI's default and must always resolve. The move is one transaction in
  `store.SetDefaultProjectForUser`.
- `DeleteProject` refuses the default project and any project still holding
  sandboxes or pools; those own runtime that has to drain through its own
  reconcilers. Once empty, `store.DeleteProject` removes the project's own
  configuration rows.
- `Project.Welcomed` records that the launcher has shown its introduction. It is
  a project row rather than client-side state so the welcome does not repeat on
  a second machine, and is settable both ways through `UpdateProject` — clearing
  it is how someone asks to be shown it again.
- The default project itself is still created by `internal/service`'s
  `InitializeDefaults`, which is startup policy: it also owns the one-time
  provider/pool installation gated on `server_state`.

## Copying (`copy.go`)

Creation seeds the built-in harnesses into every project, then optionally
copies providers, pools, and configured harnesses from a source project the
caller is a member of. See
[ADR 0023](../../../../docs/adr/0023-projects-are-created-by-copy-and-deleted-only-when-empty.md)
for what is copied and why.

Writes go through the owning services (`ProviderInstances`, `Pools`,
`HarnessConfigs` — consumer-side interfaces satisfied by those resource
packages) so create-time behavior still runs: provider config validation and
instance resolution, pool reconcile scheduling, built-in seeding. Reads come
straight from the store.

Ordering is load-bearing: providers and harnesses are database-only and roll
the project back on failure, so pools — which schedule real hosts — run last.
