# 26-10-31-694 — A provider's credentials are sealed in its row and never returned

Status: Accepted
Date: 2026-10-31

## Context

A provider instance's credentials are stored and served in the clear. Four
config fields hold one — `digitalocean`'s `token` and `sshPrivateKey`,
`execvm`'s `sshPrivateKey`, and `vm`'s `apiKey` — and the instance's config is
one JSON column written as given (`store/providers.go`), with none of the
sealing a secret's value gets. The API returns the config with the key in it;
only the CLI hides it (`isSensitiveConfigKey`). Whoever reads the database, a
backup of it, or the provider over the API reads the key.
`providers/DESIGN.md` calls the config "non-secret", which these fields are
not. The row already has an `encrypted_config` column, which nothing writes.

Two more fields name an environment variable instead (`tokenEnv`,
`sshPrivateKeyEnv`): the key is whatever the server's process holds there.

The project's secrets store is the obvious place for a credential, and the
wrong one for these. Its secrets are what a person grants an agent: bound to
a discobox, a harness config or the project, requested, approved, delegated,
handed over raw ([ADR 26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)).
A provider's key is the setup of the pools agents run on. If it were a
project secret, a project member could bind it to a discobox, and an agent
would hold the key to every machine of the provider's account.
[ADR 26-10-16-470](26-10-16-470-a-project-may-route-a-providers-api-through-a-gateway.md)
kept a gateway's key out of the store for the same reason.

The vm provider's `deliverProviderKey` places its key on a pool's machine
([ADR 26-10-23-551](26-10-23-551-a-vm-pool-bootstraps-its-own-agent-from-a-bundle-and-provisions-its-machines.md)
§4). How it is delivered is unchanged here; where the key it delivers is
kept is not.

## Decision

### 1. A field the definition types `secret` is sealed in the provider's row

`sandbox.ProviderConfigField` gains the type `secret`. The four fields above
are typed `secret`, replacing `password`. A secret field's value is never in
the `config` column: the provider service moves the secret fields into
`encrypted_config`, a JSON object sealed with the store's sealer (the one a
secret's value is sealed with), and `config` keeps the rest.

- **The server joins them before the provider sees its config.** Resolving
  an instance opens `encrypted_config` and merges its fields into the
  config, which the provider decodes into the `Config` it decodes today.
  Validation runs on the merged config. A provider imports nothing of the
  store, and its code is unchanged except for the fields §6 removes.
- **No project secret is ever named.** A provider's credential is a field
  of that provider, not a reference to anything. It has no binding,
  appears in no secret listing, and no request, approval, delegation or raw
  binding reaches it. Nothing grants it to an agent, because it is not
  somewhere a grant can name.
- **Who sets one** is who may write the provider. Today that is a member of
  the provider's project, because there are no project roles. The provider
  routes are not in the sandbox role, so no discobox reads or writes one.
  When project roles exist, writing a provider (and so its credentials)
  belongs to the role that administers the project's pools.
- **With no sealer configured**, the store keeps a secret's value as
  plaintext, and `encrypted_config` is the same. A provider's credential
  gets what a secret gets at rest, and no more.

### 2. A secret field is written and never read back

- **Create** takes a secret field's value in `config`, as today.
- **Update** replaces `config` as today, except for secret fields. A secret
  field the body leaves out keeps its sealed value. A value replaces it.
  `null` clears it. So `admin provider update --config @file` without the key
  changes everything else and leaves the key alone.
- **Every read** returns `config` without secret fields, and
  `secretFields`, the names of the secret fields that have a value, so a
  client can tell which are set:

  ```json
  {"config": {"driver": "boxd", "deliverProviderKey": true}, "secretFields": ["apiKey"]}
  ```

- **The catalog** marks each field's type, so the CLI and console know which
  ones to prompt for without echoing.
- **The CLI** shows a set field as `apiKey  (set)` in `admin provider get`.
  It reads a value from a file or stdin (`--secret apiKey=@file`,
  `--secret apiKey=-`), never from the command line, where `ps` shows it.
  `isSensitiveConfigKey`, which hid values by guessing from key names, goes,
  since no value comes back to hide. The `credentialProvider` and
  `credentialAuthType` hints go with `password`, the type they were hints
  for.
- **Immutable fields** (`refuseImmutableChange`) compare `config` as today.
  No secret field is immutable: a key changes while pools stand, which is
  what rotation is.

### 3. Rotating a key is updating the provider

Setting a secret field writes the row, which moves `UpdatedAt`. The provider
manager's cache is already keyed on `UpdatedAt`, so the next resolve builds
the provider again with the new key. No second cache key and no new trigger
is needed. A key `deliverProviderKey` already placed on a pool's machine is
not replaced by a rotation: that stays with ADR 26-10-23-551 §4, which tells
the operator to rotate the key when a machine is lost.

### 4. Existing rows are sealed at startup, and an unsealed value is never used

A startup migration runs in the store, where the sealer is available, before
`EnsureExistingSandboxProviderInstances`. It runs at every start, as
`migrateSecretTypes` does, and leaves a row it already moved alone. For each
instance, in one transaction:

- **The fields are listed in the migration** by type and key —
  `digitalocean.token`, `digitalocean.sshPrivateKey`,
  `execvm.sshPrivateKey`, `vm.apiKey` — rather than read from the provider
  definitions. The migration moves what was stored before this decision, and
  a later definition must not change what it moves.
- **A non-empty string moves** from `config` into the sealed
  `encrypted_config`, merged with anything already sealed there.
- **An environment reference (§6) is resolved once, here.** If the migrating
  process has the variable set, its value is sealed as a plaintext value
  would be, and the `...Env` field is removed. If it is unset, the field is
  removed, and the provider fails to resolve with a message saying which
  field to set. The migration changes nothing else about that provider.
- **No provider is deleted or recreated.** Its ID and pools stay. The moved
  config rebuilds its provider once, with the same key.

A server that finds a plaintext value in a secret field of `config` at
resolve fails that instance's resolve, with a message that a restart seals
it. That can only be a row written by an older server sharing the database
after this one migrated. The value is never handed to the provider. An older
server reading a migrated row finds no key and fails the same way. Running
mixed versions on one database is not supported across this upgrade.

### 5. A project copy copies the sealed fields

Copying providers copies `encrypted_config` with the rest of the row. The
copy is sealed on the same server with the same sealer, and today's copy
already carries the key, in the clear. The person copying is a member of
both projects.

### 6. Environment references are removed and refused

`tokenEnv` and `sshPrivateKeyEnv` are removed from the definitions, migrated
by §4, and afterwards refused by name at validation (400): a decoded config
ignores fields it does not know, so they would otherwise be ignored silently.
No secret field takes an environment variable's name either.

- **A row would be tied to one process.** A second server on the same
  database, the server restarted with another environment, or a project copy
  carries the variable's name and not the key, and works on one server only,
  or on none.
- **A config writer could read the server's environment.** Anyone who may
  write a provider in any project could point `tokenEnv` at a variable the
  server holds for itself, and have the provider send it to a host the
  config names.

## Alternatives considered

- **Make the field a reference to a project secret.** That reuses the store's
  sealing, revisions and binding trail. But a project secret is grantable to
  an agent: any member could bind the provider's key to a discobox. Refusing
  that would require a class of project secret that no binding, request,
  delegation or raw delivery may touch, which is a second store inside the
  first. The exchange secret `scripts/boxd-secret.sh` makes is exactly that
  kind of agent credential: it is how a discobox gets a boxd token, and it
  stays one. Rejected.
- **A separate store of operator credentials that providers name.** This
  would keep pool credentials apart from project secrets, and one key could
  be shared by several providers and rotated in one place. But no provider
  shares a key today, and it would add a resource, an API and a CLI to get
  that. Deferred (below).
- **Seal the whole config.** Then the non-secret fields would have to be
  unsealed to show, compare (`Immutable`) and copy them, and a reader could
  not tell a driver from a key. Rejected: only what is a credential is
  sealed.
- **A separate `...Secret` field beside each value field.** Every field
  would become two, which one config could set at once, and each provider
  would decide which wins. Rejected for the field type.
- **Keep environment references beside sealed values.** Rejected for §6's
  reasons.

## Consequences

- No provider credential is in the `config` column, in an API answer, or in
  the CLI's output. Each is sealed in its provider's row, and only the server
  opens it.
- A provider's key can never be granted to an agent: it is not a secret any
  binding names.
- A key used by several providers is set on each, and rotated on each.
- Who wrote or rotated a provider's key is recorded nowhere. No trail of
  provider writes exists today, and none is added here (below).
- `scripts/boxd-pool.sh` keeps one step. It gives the key as the provider's
  `apiKey` from a file, as it does now, and the server seals it. Its comment
  that the key is in the database in the clear goes.

## Deferred

- **A store of operator credentials shared across providers.** Revisit when
  two providers, or a provider and a gateway, need one key rotated in one
  place.
- **A gateway's literal header values**, which are stored in the clear,
  returned by `GET`, and written into every pool's policy file. Sealing them
  the same way, in the gateway's row, means revisiting ADR 26-10-16-470 §5
  and ADR 26-10-17-271, so it is a decision of its own. Revisit with that
  issue.
- **A trail of who changed a provider.** Revisit when project roles separate
  who administers pools from who uses them, since that is when "who rotated
  this key" stops being any member.
