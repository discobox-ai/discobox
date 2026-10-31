# 26-10-31-694 — A provider's credentials are secrets its config names

Status: Proposed
Date: 2026-10-31

## Context

A provider instance's credentials are stored and served in the clear. Four
config fields hold one — `digitalocean`'s `token` and `sshPrivateKey`,
`execvm`'s `sshPrivateKey`, and `vm`'s `apiKey` — and the instance's config is
one JSON column written as given (`store/providers.go`), with none of the
sealing a secret's value gets (`sealSecretForWrite`). The API returns the
config with the key in it; only the CLI hides it (`isSensitiveConfigKey`).
Whoever reads the database, a backup of it, or the provider over the API
reads the key; nothing records who pointed a provider at which key; and
rotating one means rewriting the provider. `providers/DESIGN.md` calls the
config "non-secret", which these fields are not.

Two more fields name an environment variable instead (`tokenEnv`,
`sshPrivateKeyEnv`): the key is whatever the server's process holds there.

The secrets store already does what these keys lack: a value sealed with the
store's sealer and never returned, a `Revision` that moves with every write,
bindings that are the whole of a permission and recorded in
`secret_binding_events` ([ADR 26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
§1), and a copy that carries secrets with what binds them.

The vm provider's `deliverProviderKey` places its key on a pool's machine
([ADR 26-10-23-551](26-10-23-551-a-vm-pool-bootstraps-its-own-agent-from-a-bundle-and-provisions-its-machines.md)
§4). How it is delivered is unchanged here; which key it delivers, and who
may let it leave, is not.

## Decision

### 1. A field the definition types `secret` holds a reference, and the server resolves it

`sandbox.ProviderConfigField` gains the type `secret`. The four fields above
are typed `secret`, replacing `password`. Such a field's stored value is a
reference, never a value:

```json
{"driver": "boxd", "apiKey": {"secret": "sec_01J..."}, "deliverProviderKey": true}
```

- **Only a `secret` field takes a reference, and it takes nothing else.**
  Validation refuses a string in one (400, naming the field and saying to
  store it as a secret first), and refuses a reference in any other field.
  The secret is named by ID, which a rename does not move; the CLI takes a
  name and looks the ID up.
- **The server resolves before the provider sees its config.** Resolving an
  instance reads each referenced secret, unseals it, and puts its value in
  the field as the string the provider's `Config` already decodes. A provider
  imports nothing of the secrets store, and its `Config` and `Validate` are
  unchanged but for the fields §7 removes.
- **A reference that does not resolve fails the instance**, as a backend
  that does not come up does: a create answers 502 and leaves nothing
  behind, and at startup the instance is logged and its pools are retried
  (`providers/DESIGN.md`). It is never resolved to an empty value.

Rejected:

- **A separate `...Secret` field beside each value field.** Every field
  becomes two, which one config may set at once, and each provider decides
  which wins. A plaintext value stays one field away.
- **A reference allowed in any field, with no field type.** The server could
  not refuse plaintext in a credential field it cannot tell apart, and any
  field — `execvm`'s `command` — could pull any secret in the project into
  where the provider puts it, a command line among them.
- **Accepting a value and sealing it into an anonymous secret.** The secret
  would sit outside the listing a person rotates and audits secrets from,
  and every config update would mint another. There is one way in: store the
  secret, then name it.

### 2. It names a `token` secret, never an `exchange` or `oauth` one

A `secret` field takes a secret of type `token`: one opaque string, which an
SSH private key's PEM is as much as an API key is. Any other type is a 400.

- **`exchange` is refused**, the kind `scripts/boxd-secret.sh` makes. Its
  value is the token the exchange returns, which boxd expires within the
  hour; the vm driver needs the key itself, to issue a pool its own key and,
  with `deliverProviderKey`, to place it on a machine. Its stored fields are
  not read out either: an exchange's key is sealed to go only to its
  recipe's URL, which `checkRecipe` holds within the secret's routes, and
  handing it to a provider would send it wherever the provider sends it.
  The same boxd key may be stored twice, as an exchange secret for
  discoboxes and a token secret for the provider; they rotate apart.
- **`oauth` is refused.** Its access token renews on resolve and expires;
  none of the providers takes a token that changes under it.

A secret's routes do not apply to the provider's use. They say where a pool
proxy may swap a sentinel; a provider's request is the server's own, to a
host the provider decides — DigitalOcean's API, boxd, an SSH host the exec
command names — which no route list names ahead. What bounds it is the
binding of §3.

### 3. Naming a secret is a provider binding, made by a person, and recorded

The reference is the provider's config, and the permission is a
`SecretBinding` with the new target kind `provider`, its target the provider
instance. The provider service writes it in the transaction that writes the
config, one per referenced secret, and deletes it when the reference goes.

- **Who may make one** is who may write the provider: a person who is a
  member of the provider's project. The provider routes are not in the
  sandbox role, so no discobox names a secret here, and the secret must be
  in the provider's project (404 otherwise).
- **It is shaped for the server's use** (`checkBindingShape`): `use`, no
  variable, no routes, no delegation, no parent. It is none of the four
  levels `ResolveBindings` reads, so it reaches no discobox and marks no
  holder.
- **It never lapses, so the secret must allow that.** A secret's binding
  limit is a ceiling on how long anything holds it, and a provider holds it
  for as long as the provider exists: a secret whose `MaxBindingTTL` is not
  0 is refused (400, saying so). No exemption is made for the server.
- **Placing the value on a machine is handing it over**, so a `vm` provider
  with `deliverProviderKey` requires `Secret.AllowRaw` on its `apiKey`
  secret ([ADR 26-10-27-840](26-10-27-840-a-binding-may-hand-over-the-raw-value-held-in-memory-and-watched-for-at-the-proxy.md)
  §1), refused at create and update otherwise. Clearing `AllowRaw` on a
  secret such a provider names is a 409, not the silent delete a
  discobox's raw bindings get: it would block the provider's pools. Without this, a
  member could point a delivering provider at any secret in the project and
  read it off a machine.
- **The binding API reads it and does not write it.** A person lists it
  among the secret's bindings, which is how they see what uses a key before
  rotating or deleting it. Creating, updating or deleting a `provider`
  binding through the binding API is refused (409): the config is where it
  is made.
- **A secret a provider names is not deleted** (409, naming the providers),
  as a provider with pools is not: deleting the binding with it would leave
  a config naming nothing.
- **It is recorded.** Each create, change and delete is a
  `secret_binding_events` row with the person who wrote the provider. Each
  first resolve of a secret at a new revision for that provider records the
  binding's `delivered` event, as a raw binding's first answer does, so the
  trail says when the server read which revision of a key for which
  provider.

Rejected: **a reference table of its own, outside bindings.** It would hold
the same row as a binding and need its own events, cascade and listing; and
it would make a binding no longer the whole of a permission
(ADR 26-10-27-880 §1).

### 4. The API returns the reference, and the CLI says which secret

The instance API returns the config as stored: the reference, never a value.
The catalog says which fields are `secret`, so a client knows what to ask
for. `admin provider get` shows such a field as the secret's name and ID
(`apiKey  secret boxd (sec_01J...)`), and `admin provider create` and
`update` take `--secret <field>=<name or id>`. `isSensitiveConfigKey`, which
masked values by their key's name, goes: there is no value to mask. The
`credentialProvider` and `credentialAuthType` hints on a field go with
`password`, the type they were hints for.

### 5. A new revision of the secret rebuilds the provider

The provider manager's cache is keyed by the instance's `UpdatedAt` and the
`Revision` of every secret its config names (not the secret's `UpdatedAt`,
which a coarse clock can repeat). A value write — a person's update, or the
upgrade of §6 — marks the pools of every provider naming the secret dirty
(`EnqueueProviderPools`), so the next reconcile resolves the provider again
and builds it with the new value without waiting for another write. Rotating
a key is updating its secret; the provider's config does not change.

A key `deliverProviderKey` already placed on a pool's machine is not
replaced by a rotation; that stays ADR 26-10-23-551 §4's, which tells the
operator to rotate it when a machine is lost.

Rejected: **reading the secret on every call instead of rebuilding.** The vm
driver's `machine.KeySource` could, but DigitalOcean's and the exec
provider's clients take their key at construction. One rule for every
provider, which the cache already has.

### 6. Existing rows are moved into secrets at startup, and a server never uses one it has not moved

A startup migration, run in the store where the sealer is in reach and
before `EnsureExistingSandboxProviderInstances`, moves every plaintext
credential into a secret. It runs at every start, like `migrateSecretTypes`,
and does nothing to a row already moved. For each instance, in one
transaction per instance:

- **The fields are listed in the migration**, as type and key —
  `digitalocean.token`, `digitalocean.sshPrivateKey`,
  `execvm.sshPrivateKey`, `vm.apiKey` — rather than read from the provider
  definitions. The migration moves what was stored before this decision, and
  a later definition must not change what it moves.
- **A non-empty string becomes a `token` secret** in the instance's project,
  named `<provider name>-<field key>` (with a `-2`, `-3`… on a collision),
  sealed by the store, with `MaxBindingTTL` 0, no routes, and `AllowRaw` set
  for a `vm` `apiKey` whose provider has `deliverProviderKey`: what it took
  to keep the provider working, and no more. The field becomes the
  reference, and the `provider` binding is written with cause `upgrade`.
- **An environment reference (§7) is resolved once, here**: the migrating
  process's variable, if set, is stored exactly as a plaintext value would
  be, and the `...Env` field is removed. If it is unset, the field is removed
  and the provider fails to resolve, saying which field to set; nothing else
  about it changes.
- **No provider is deleted or recreated**: its ID and pools are kept, and
  the changed config rebuilds its provider once, with the same key.

A plaintext string a server finds in a `secret` field at resolve — a row
written by an older server sharing the database after this one migrated —
fails that instance's resolve with a message that a restart migrates it. It
is never handed to the provider. An older server reading a migrated row
fails to decode the reference, and its instance does not resolve: running
mixed versions on one database is not made to work.

With no sealer configured, the store keeps a secret's value as plaintext,
and a provider's credential gets exactly that; this decision gives a
provider credential what a secret gets, not more.

### 7. Environment references are removed and refused

`tokenEnv` and `sshPrivateKeyEnv` are removed from the definitions, moved
by §6, and refused by name at validation (400) afterwards, since a decoded
config ignores fields it does not know. No `secret` field takes an
environment variable's name either.

- **A row would be tied to one process.** The same database behind a second
  server, the server restarted with another environment, or a project copy
  (§8) carries the variable's name and not the key, and works on one server
  only, or none.
- **A config writer could read the server's environment.** Anyone who may
  write a provider in any project could point `tokenEnv` at a variable the
  server holds for itself and have the provider send it to a host the config
  names.

### 8. A project copy copies the secrets its providers name

Copying providers copies each secret a copied provider names, with its value
(as `copySecret` already does for a harness config's), points the copy's
reference at the copy, and writes its `provider` binding. A secret two
copied providers name is copied once.

## Consequences

- No provider credential is in a provider row, in the API, or in the CLI's
  output; each is a sealed secret that a person lists, rotates and deletes
  where other secrets are, and its use by a provider is in the binding trail.
- Making a provider is two steps where it was one: store the secret, then
  name it. `scripts/boxd-pool.sh` stores the key as a `token` secret with
  `--allow-raw` and no binding limit, and names it.
- A secret meant for a provider must have no binding limit, which also lets
  a person bind it to a discobox for good. That is the person's to choose
  when they store it; a separate secret for a provider is the way not to.
- A credential a discobox's binding and a provider both use cannot be
  deleted while the provider names it.
- A key `deliverProviderKey` places on a machine still outlives a rotation
  there (§5); this decision makes the rotation one secret update, not the
  machine's copy go away.
