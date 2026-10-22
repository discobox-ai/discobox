# 26-10-17-271 — The server configuration file names project gateways

- **Status**: Accepted (narrows [26-10-16-470](26-10-16-470-a-project-may-route-a-providers-api-through-a-gateway.md)
  §§1, 4 and 6: a header's value from the server's environment is the
  configuration file's alone)
- **Date**: 2026-10-17
- **Relates to**: [26-10-16-470](26-10-16-470-a-project-may-route-a-providers-api-through-a-gateway.md),
  whose deferred "projects and their gateways in the server's configuration
  file" this is;
  [0096](0096-the-server-reads-one-schema-checked-configuration-file.md),
  the file it extends;
  [26-10-07-005](26-10-07-005-discobot-asserts-who-a-person-is-and-discobox-keeps-no-users.md),
  whose asserted principals hold a project and not the server.

## Context

A gateway is setup that belongs to the operator. Its key is in the server's
environment already (ADR 26-10-16-470 §4). Its route is the same kind of
thing, but today the only way to state it is
`discobox admin gateway set`, run against each project after the project
exists. Nothing records it, and a rebuilt server needs it entered again by
hand. Operators want it written next to the rest of the server's
configuration in `server.yaml`, maintained by hand, and applied when the
server starts.

A gateway needs no new handle, since its `name` is already a DNS label that
is unique in its project. A project has two handles that are stable: its
generated ID (`proj_…`), and `default`, which the API already resolves to a
user's default project. Its `name` is not stable: anyone in the project can
rename it with `PATCH`, and it is unique per owner, not per server, so two
discobot users can both have a project called `platform`.

## Decision

**`server.yaml` holds `projects`, a map from a project handle to the
gateways the server keeps on that project. A handle is `default`, meaning
the default user's default project, or a project ID. The server keeps each
gateway the file names as the file states it, and leaves the project's
other gateways alone.**

### 1. Shape

```yaml
projects:
  default:
    gateways:
      anthropic:
        match: https://api.anthropic.com/v1
        target: https://gateway.example.com/anthropic/v1
        requestHeaders:
          remove: [x-api-key]
          set:
            - name: cf-aig-authorization
              valueFrom: {env: DISCOBOX_GATEWAY_CF_TOKEN}
            - name: x-team
              value: platform
  proj_6q3w8y5z7k9m4n6p: # platform
    gateways:
      openai:
        match: https://api.openai.com/v1
        target: https://gateway.example.com/openai/v1
        requestHeaders:
          set:
            - name: authorization
              valueFrom: {env: DISCOBOX_GATEWAY_PLATFORM_OPENAI}
```

- A gateway's body is the `PUT /projects/{id}/gateways/{name}` body
  (ADR 26-10-16-470 §1), field for field, and its key is the gateway's
  `name`. It is one vocabulary, validated by `proxy/gateway`, so the file
  can't accept a gateway the API would refuse, and the reverse holds too.
- `valueFrom.env` is the file's alone (§3). A project gets a key of its own
  by its gateway naming a `DISCOBOX_GATEWAY_*` variable no other project's
  does. The `DISCOBOX_GATEWAY_` prefix rule stays even though the operator
  wrote the file: nothing else of the server's environment is ever reachable
  through a gateway.
- `projects` has no environment variable. A map of gateways can't be
  expressed as one, so for this key ADR 0096 §4's precedence comes down to
  defaults and the file.
- The file is checked when it loads, as everything else in it is
  (ADR 0096 §3). An unknown key, an invalid gateway, two gateways under one
  handle with the same `match`, or a handle that is neither `default` nor a
  well-formed project ID all stop the server, and the error names the key.

### 2. A handle is `default` or a project ID, and nothing else

- `default` is the default user's default project: the one a server
  without discobot creates on first boot and the CLI uses. It does not mean
  a discobot user's default project.
- Any other handle is a project ID, written as the API shows it
  (`discobox admin project ls`).
- No new handle is added to projects. A server-wide name for a project
  would have to be assignable by the operator and by no project member,
  because whoever holds it receives the gateways the file lists under it.
  The API authorizes projects by membership, and under discobot the
  operator is a member of none, so that assignment needs an authorization
  path the server doesn't have. An ID is assigned by nobody and can't be
  claimed.
- When `default` resolves to a project the file also names by ID, the entry
  under its ID applies and the `default` entry is skipped with a warning.
  The ID is the more specific statement, and merging two entries would need
  a rule for two gateways with one name or one match.

### 3. The file owns the gateways it names, and nothing else

- Each gateway row records whether the file manages it
  (`managed_by = 'config'`) or the API does (`'api'`, the default).
- Only a config-managed gateway takes a header value from the server's
  environment. A `PUT` with `valueFrom` is refused with `400`, the CLI has no
  `--set-header-env` or `--add-header-env`, and the resolve route
  (ADR 26-10-16-470 §4) answers only for a config-managed gateway. Under ADR
  26-10-16-470 any member could name any `DISCOBOX_GATEWAY_*` variable, and
  so send a key the operator meant for another project to a target of their
  choosing; now only the operator names one. A gateway the API made with an
  environment header before this is deleted when the file is applied, with
  a warning naming it, and its project's discoboxes are marked, so their
  pools drop it and any value they hold for it. Leaving it would have its
  pools serve a value held from before, and refusing it at the resolve
  route alone would hold its requests as a retryable `503` forever.
- At startup the server, for each handle that resolves to a project:
  - creates or replaces every gateway the file names, and marks it
    config-managed. A gateway with that name that the API made is taken
    over;
  - deletes an API-made gateway whose `match` is the same as one the file
    names. Two gateways with the same `match` are refused
    (ADR 26-10-16-470 §2), and the file wins. The deletion is logged, naming
    the project and the gateway;
  - deletes config-managed gateways that the file no longer names for that
    project.
- A config-managed gateway on a project that no handle resolves to any more
  is deleted. That happens when its entry left the file, or when `default`
  moved to another project.
- The API leaves gateways it didn't make alone. Members keep adding,
  replacing, and deleting their project's other gateways as they do today.
- A config-managed gateway can be read through the API. `GET` shows
  `managedBy: config`, and `discobox admin gateway ls` and `get` show it.
  `PUT` and `DELETE` on one are refused with `409`, with an error that names
  `server.yaml`. A `PUT` of a new gateway whose `match` is the same as a
  config-managed one is refused the same way.
- Applying gateways marks the project's sandboxes for their policy
  reconcile, the same mark an API write sets (ADR 26-10-16-470 §5). A gateway
  is delivered and applied the same way whoever made it.

### 4. When the file is applied

The file is read once, at startup, and its gateways are held as the desired
state for each handle. A changed file takes effect when the server
restarts, like every other setting.

Section 3's apply runs for a project:

- at startup, for every handle;
- when the default user's default project is created on first boot, or
  when it changes, for the project `default` left and the one it now names,
  in the transaction that moves it: a move whose apply fails does not
  happen.

An ID that names no project is not an error. The server logs a warning at
startup naming the handle and skips it. Project IDs are never reused, so
the entry will not match until it is removed. Deleting a project deletes its
gateways, config-managed ones included.

## Consequences

- An operator can write a server's gateways down, version them, and keep
  them across restarts. A single-user server needs only `default`, so its
  file carries over to a rebuilt server unchanged.
- A project on a discobot server is configured by its ID, which the
  operator looks up once. A rebuilt server has new IDs, so its entries must
  be rewritten. A trailing comment naming the project is the only
  reminder of which project an ID is.
- Members can see the operator's gateways on their project but can't change
  them, and never see a `valueFrom.env` value. A key the operator gives one
  project is not reachable from another: no member can name a variable.
- A member can no longer route a gateway with a key from the server's
  environment; that is the operator's, in the file.
- If an operator adds a gateway to the file whose `match` collides with
  one a member made, the member's gateway is deleted at the next startup.
  This is logged but not otherwise announced.
- A project deleted while the file still names it leaves a warning at every
  startup until its entry is removed.
- `projects` is the server file's first map of structs. `configfile`
  already decodes and describes that shape; the commented reference shows it
  as one example line.

## Alternatives considered

- **Key the file by project name.** A member's rename unbinds the entry
  silently, and under discobot a name is ambiguous across owners. Rejected.
- **A project slug, assigned by the operator at a server-scoped route
  (`/project-slugs`).** It lets the file survive a rebuilt server, but it
  adds a resource that exists only to work around membership-based
  authorization. Rejected.
- **A project slug that may be set only over the server's local socket.**
  The local socket identifies the operator, since discobot never connects
  over it. But servers that run discobot usually set `authRequired`, which
  turns away the local socket entirely, so the slug would need an exception
  to `authRequired` for one setting. Rejected.
- **A project slug that members set with `PATCH`.** Whoever holds a slug
  receives the gateways the file lists under it, so a member could claim
  another team's slug and spend its keys. Rejected.
- **The file owns a listed project's whole gateway set.** A project the
  operator configured would lose every gateway its members made. Rejected
  for per-gateway ownership.
- **Seed only: create the gateways at startup and let the API edit them.**
  An edit lasts until the next restart and then disappears. That is the
  silent drift ADR 0096 exists to remove. Rejected.
- **Reserve the variables the file names, and keep `valueFrom` in the
  API.** A `PUT` naming a reserved variable would be refused, and the
  resolve route would answer a reserved one only for a config-managed
  gateway. Members could still spend every unreserved variable on any
  target, and an operator adding a variable would have to remember it was
  exposed until the file named it. Rejected for the file alone naming any.
- **Resolve the file's environment values at startup and store them as
  written values.** The resolve route would go, but the key would be stored
  in the database, pushed into every pool's policy file, and shown to every
  member by `GET`. ADR 26-10-16-470 rejected the first two already.
  Rejected.
- **Fail startup on an ID that names no project.** A member deleting their
  project would stop the server from restarting. Rejected for a warning.
- **On a `match` collision, hold the file's gateway back instead of
  deleting the member's.** The operator's configuration would then depend
  on what members did, and a gateway the operator wrote down would silently
  not apply. Rejected.

## Deferred

- **A stable, human-written project handle other than `default`.** Revisit
  when the server can authorize an operator-only action on a project the
  operator is not a member of, or when discobot can assign one.
- **Other project settings in the file** (egress, default harness config,
  retention). The `projects.<handle>` level is there for them. Revisit
  setting by setting, when someone asks for one, and decide ownership for
  each one as §3 does here.
- **Reloading the file without a restart.** Revisit when a second setting
  wants it. §3's apply is already safe to run at any time.
