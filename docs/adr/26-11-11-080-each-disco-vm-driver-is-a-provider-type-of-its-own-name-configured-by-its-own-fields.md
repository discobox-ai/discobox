# 26-11-11-080 — Each disco-vm driver is a provider type of its own name, configured by its own fields

- **Status**: Proposed. On acceptance it supersedes, and each superseded ADR's
  status line says so:
  - [26-10-15-629](26-10-15-629-the-vm-provider-and-its-image-kind-are-spelled-vm.md)
    §1's provider type and catalog name `vm`. The rest of §1 stands for
    discobox's own strings (§2). Its image kind `vm/<driver>` (§2) and its
    state root under `discobox/vm/<driver>` stand.
  - [26-10-09-106](26-10-09-106-disco-vm-is-a-pool-runtime-and-the-server-runs-its-machines.md)
    §1's "one provider with drivers — `vz`, `hcs` and `boxd`".
  - [26-10-23-551](26-10-23-551-a-vm-pool-bootstraps-its-own-agent-from-a-bundle-and-provisions-its-machines.md)
    §4's provider's own key placed on a pool machine when the driver cannot
    issue one (`deliverProviderKey`), with what bounds a delivered key that
    is the provider's own: "the proxy never forwards it". A pool machine
    holds a sentinel instead, and its proxy forwards the key to the one
    place the driver sends it (§9). §4's issued key, and its bounds, stand,
    as does its "A key wider than the pool is acceptable where the driver
    offers nothing narrower", which now also covers §9's tokens and the key
    in the proxy's memory. Its "the pool records whether its key is issued or the
    provider's" reads "issued or a sentinel for the provider's".
- **Narrows**:
  - 26-11-11-963, "One vm provider type: every pool runs a Linux pool agent
    in a machine its driver makes" (Proposed in #1003), if it is accepted:
    - its "`vm` provider" and "`vm` pool" read as every provider type of §1
      below, and its one pool shape is theirs;
    - no build links a held driver that carries a pool: vz and libkrun are
      not linked, and hcs is linked with its pools refused (963 §13). So
      963's held-driver work waits on this ADR's Deferred entry: §1's pool
      OS column and platform check, the held halves of §§3–7, and §§9–12.
      What 963 decides for a driver reached at its URLs (boxd) is built.
  - [26-10-09-106](26-10-09-106-disco-vm-is-a-pool-runtime-and-the-server-runs-its-machines.md)'s
    Deferred "disco-vm's `docker` driver as a `discovm` driver", and
    [26-10-23-551](26-10-23-551-a-vm-pool-bootstraps-its-own-agent-from-a-bundle-and-provisions-its-machines.md)
    §5's "disco-vm's `docker` driver stays deferred": `docker` is a built-in
    provider type, so the driver is not linked, and is revisited with this
    ADR's Deferred entry.
  - 551 §3: the bootstrap of a pool on a driver reached at its URLs names the
    driver's configuration beside the driver (§6).
  - [26-10-31-694](26-10-31-694-a-providers-credentials-are-sealed-in-its-row-and-never-returned.md)
    §2's "a field the body leaves out keeps its sealed value", for a write
    that changes a `Privileged` field (§4).
- **Date**: 2026-11-11
- **Issue**: #1006
- **Relates to**:
  - [0003](0003-promote-pool-to-a-first-class-primitive.md), one provider
    instance backing several pools;
  - disco-vm ADR 26-11-11-215, a driver declares and validates its own
    configuration (vm#73, merged at `8b07714`);
  - disco-vm PR 69, boxd's `region` (vm#68; open). It subsumes #997.
  - ADR 26-11-06-442, WebSocket credentials require explicit message places
    (Proposed in #748, for #741), whose place model §9's HTTP body places
    (#1012) follow.

## Context

discobox has one disco-vm provider, type `vm` (`server/providers/discovm`):

- Its `Config` (`provider.go`) names a disco-vm `driver`, `Immutable` while
  the provider has pools, beside settings of discobox's own: the pool policy,
  `apiKey` (a sealed `secret`), `base`, `controlPlaneUrl` and
  `deliverProviderKey`.
- `Validate` constructs the driver to learn its kind, local or remote.
  `validateFor` then requires a remote driver's key and refuses a local
  driver's key, URL and key delivery.
- The server and the pool agent each link drivers one by one, not through
  disco-vm's `pkg/machine/drivers`. The server links boxd everywhere
  (`drivers.go`), vz on darwin and hcs on Windows; the pool agent links boxd
  (`pool-agent/drivers.go`). A pool on vz or hcs has never run: `hostAgent`
  answers `errHostAgentNotStaged` (`hostagent.go`).
- `server/go.mod` and `pool-agent/go.mod` pin disco-vm at `5682ac6`, where a
  driver reads its own settings from the process environment (boxd's
  `BOXD_GRPC_ADDR`, `BOXD_TOKEN_URL`).
- Provider routes are authorized by `ProjectAuthorizer`
  (`server/internal/auth/authorizers.go`): any member of the project, or a
  person discobot vouches for in it, may create or update a provider. There
  is no administrator role; `admin` is only the CLI's command group.
- An update keeps every sealed field its body leaves out (694 §2,
  `updateConfig`). `refuseImmutableChange` allows any change once the
  provider has no pools.

disco-vm ADR 26-11-11-215 (merged at `8b07714`) gives each driver a
configuration of its own:

- `machine.Config.DriverConfig`, a JSON object, decoded strictly;
- a `machine.Definition` per driver, read with `machine.DefinitionOf`. Its
  `ConfigField`s have the shape of discobox's `ProviderConfigField`, plus
  `Default`, `Env`, `Privileged` and `Retired`;
- `Definition.Base`, describing `machine.Config.Base`, which stays outside
  the driver's configuration;
- no environment read by a driver, and no field holding a secret: a key is
  `machine.Config.Keys`;
- a local shim given its configuration in a file, and refusing a driver
  whose configuration is not that file's (215 §6).

boxd's fields are `addr` and `tokenUrl` (both `Privileged`) and `region`;
archil's are `region` and `baseUrl` (`Privileged`); hcs has none. boxd
refuses every region until disco-vm PR 69 lands. archil reports no
`Isolation`, no `Ports` and `Keys: none`, and refuses `Networks` and
`Isolated` on create, though it is called with a key (it registers a
`KeyEnv`).

The owner (2026-11-11):

> for discovm we effectively flatten the driver list from discovm so that
> each driver in discovm becomes a provider in discobox, with no prefix. So
> discovm driver x becomes provider x in discobox. The config should line up
> nicely now so that the dynamic config can be supported.

and, on drivers whose names clash with a built-in provider: "just don't
import the drivers that will cause a problem".

## Decision

### 1. A provider type per linked driver, named as disco-vm names it

- **Each disco-vm driver discobox links is a provider type of the driver's
  own name**, with no prefix: `boxd` and `archil` on every build, `hcs` on
  Windows. The `vm` type goes.
- **One implementation serves them all.** The `discovm` package registers
  one type per linked driver, from disco-vm's registry, with its
  description from the driver's `Definition`. Nothing in it is written per
  driver beyond the import that links one. With 963 accepted, every type
  has 963's one pool shape.
- **Not linked:**
  - `docker`, `libkrun` and `vz`, whose names are built-in provider types
    already;
  - `fake`, which only tests link, as now.
- **A clash fails the server's start.** Registering a driver's type whose
  name is already a provider type panics, as disco-vm's own duplicate
  registration does. A test registers every provider of every build, so a
  disco-vm driver added under a taken name fails CI, not a user's server.
- **A type exists for every linked driver, and whether it carries a pool is
  disco-vm's report.** A pool on a driver that meets no fence of 963 §5
  (today's `Isolation` check, without 963) is refused with that reason:
  - **archil**: every pool is refused today. It has no `Isolation`, no
    `Ports`, and refuses `Networks`, so it meets neither fence. Its provider,
    fields and validation exist, so a pool needs only disco-vm to give it a
    fence.
  - **hcs**: every pool is refused until 963 §13's Windows work, as a `vm`
    pool on hcs is today.

### 2. The configuration: discobox's fields, and the driver's under `driver`

A provider's configuration is one JSON object in two parts, which share no
key: discobox's pool-hosting fields, and the driver's fields under `driver`.
The driver's secret fields are among its own, and travel apart from the rest
(below).

**discobox's pool-hosting fields, at the top level.** None is a driver's
setting: each says how discobox hosts a pool on the driver. Each applies by
the driver's kind (963 §1: held, or reached at its URLs), as `validateFor`
does now:

- the pool policy: every type.
- `controlPlaneUrl`: a driver reached at its URLs. It is refused on a held
  driver (hcs).
- `deliverProviderKey` goes (§9).
- `base`, which is `machine.Config.Base`: a driver that takes one. Its
  label and description are the driver's `Definition.Base`. Whether a
  driver takes a base is not readable from disco-vm either: hcs says "none"
  only in its description, and ignores `Config.Base`. Until disco-vm exposes
  it (Consequences), a driver reached at its URLs takes one, and a held
  driver (hcs) has `base` refused.

**The driver's configuration, under `driver`**: an object passed to disco-vm
as `machine.Config.DriverConfig`, as it is.

- It is validated by disco-vm alone: `machine.New`, and the same
  normalization `Validate` runs. An unknown key, a missing required field,
  or a value the driver refuses is a provider create's 400.
- Its fields are listed from `machine.DefinitionOf` when the server starts.
  A field disco-vm adds shows up in the provider catalog, `admin provider
  create`, its help and its validation, with no change to discobox, except a
  `Privileged` one (§3).
- In the catalog a driver field's key is `driver.<key>`, and its CLI flag
  `--driver-<key>` in the CLI's kebab case (`--driver-region`,
  `--driver-token-url`). A `Retired` field is listed nowhere, and still
  accepted, as disco-vm decodes it. A field's `Default` is shown.
- The server reads no driver's environment variable, except once in §8's
  migration. A variable a linked driver's field names (`BOXD_GRPC_ADDR`)
  that is set in the server's environment is named in a warning at start,
  never its value.

**The driver's secret fields.** A driver defines its secrets as fields of
its definition, typed secret, as it defines any other: boxd's and archil's
API key are theirs, and hcs has none. discobox's `apiKey` goes. A secret
field is listed under `driver.<key>` with the rest, and:

- **is stored apart.** discobox seals it in the provider's row, outside
  `config`, as it seals any secret field (694): never returned, given to the
  CLI only from a file or stdin (`--secret driver.apiKey=@path`), and kept
  by an update that leaves it out, but for §4's write.
- **is transferred apart.** At resolve, discobox passes the driver's secret
  fields to disco-vm on their own, never inside `DriverConfig`: asked on
  every call, as `Config.Keys` is today, so a rotated key needs no restart.
  Nothing that carries `DriverConfig` carries a secret: not the pool's
  bootstrap (§6), its placement fingerprint, a shim's configuration file
  (215 §6), nor anything disco-vm prints.
- **is never on a pool machine's disk, in its environment or in its
  provisioning key file.** That file holds an issued key (551 §4), or else a
  sentinel for the driver's key secret, which its pool's proxy swaps (§9).
  The key is on the pool machine only in that proxy's memory, while cached. An issued key is a value
  for the driver's key secret, so the pool agent's engine calls with it as
  the provider's engine calls with the provider's.
- **tells discobox which drivers take a key.** A driver with a secret field
  takes one, which replaces the guess from `Capabilities.Keys` (which says
  whether a driver issues keys, not whether it takes one).

This reverses disco-vm ADR 215 §7's "No field holds a secret … a
definition has no type for one". disco-vm owes that change (Consequences).
How disco-vm folds `Config.Keys` and its issued keys into secret fields is
its own ADR's to decide.

**Wording.** A driver's descriptions and errors are shown as disco-vm writes
them, so a person may read disco-vm's terms there. discobox's own strings
keep 629 §1's rule: they say VM, never disco-vm.

### 3. Who may set a `Privileged` field, and where it may point

A `Privileged` field of a linked driver is an endpoint the key is sent to:
boxd's `addr` and `tokenUrl`, archil's `baseUrl`. Any project member may
write a provider (Context), and the server dials these from its own network
with the key, as does the pool agent (§6). So:

- **The endpoint is public, over TLS.** A `Privileged` endpoint is `https`
  (`addr` is TLS whatever it says), and its host is not a loopback,
  link-local or private address, nor a name that resolves to one. disco-vm's
  `http` to loopback, which it allows for a test, is refused.
- **The server's operator may allow more**, by a server setting listing the
  endpoints, private ones included, that providers may name. A private boxd
  cluster is one of these.
- **Checked when set, and when dialed.** A name is resolved and checked by a
  write that sets or changes the field, as §4 scopes its rule. A write that
  leaves it as it is (a key rotated, a policy changed) does not check it.
  The engine's connections are checked again at dial,
  through a dialer disco-vm takes from its embedder (Consequences), so a
  name that later resolves elsewhere reaches nothing.
- **`Advanced`** in the catalog, whatever disco-vm marks them.
- **A test holds each linked driver's `Privileged` keys.** A driver that
  gains one fails it, and is linked again only once that field is checked:
  - a field naming a host resource (a socket, a file, a directory) is never
    a project's to set;
  - a driver whose base is `Privileged` (vz, libkrun: a host directory) is
    not linked.

  This is the one place a driver's new field needs a discobox change.

### 4. Changing a `Privileged` field carries the key again

A write that sets or changes a `Privileged` field, on create or update,
carries every secret field of the provider in the same body. Otherwise it is
refused with a 400 naming them. So a sealed key is only ever sent to an
endpoint written by someone who supplied that key. This narrows 694 §2 for
such a write; every other update keeps a sealed field it leaves out.

### 5. A driver field cannot change while the provider has pools

Every field under `driver` is `Immutable` while the provider has pools, as
`driver` is today. Fields like boxd's `region` and `addr`, and archil's
`baseUrl`, decide where a pool's machines are, so a new value would leave the
machines the old one made with nothing to find them. The type itself cannot
change. Which fields could safely change is deferred (Deferred).

### 6. The pool agent gets the same driver configuration

On a driver reached at its URLs, the pool agent opens disco-vm's engine
itself (551 §3). Its bootstrap names the provider's driver configuration
beside the driver, so the agent's engine calls the same endpoint and region
as the provider's. It holds no secret: the driver's secret fields travel
apart (§2).

- It is in the placement's fingerprint. A pool whose provider has an empty
  driver configuration, which is every migrated row but those §8 gives
  values, keeps its fingerprint and is not placed again. One with values is
  placed again once.
- On a held driver, 963 §3's bootstrap names the served engine, not a
  driver configuration.

### 7. A region is a field of the driver

boxd's `region` is an ordinary field of boxd's configuration, read from
disco-vm's registry like any other. There is no pool-level region setting,
and #997 is closed into #1006. Until disco-vm PR 69 is in discobox's pin,
boxd refuses every region, and so does a `boxd` provider's create, with
disco-vm's message. Once it is in, the pin moves and nothing else changes.
archil's `region` is accepted now, on a provider whose pools are refused
(§1).

### 8. Upgrading a `vm` provider

A data migration rewrites each provider row of type `vm`, once. It is
idempotent: it finds only rows still typed `vm`. It runs where
`SealProviderCredentials` runs, with the server's environment lookup.

- **A driver discobox links** (`boxd`, `archil`, `hcs`, by a fixed list
  written into the migration):
  - the type becomes the driver's name;
  - `driver` is dropped from the configuration, and every other field kept
    as it is;
  - **the sealed `apiKey` becomes the driver's key secret**, sealed to
    sealed, by a fixed mapping in the migration from `apiKey` to the secret
    field each linked driver names for its key. It is never written to
    `config` or decrypted into anything stored;
  - **the endpoints the old pin read from the environment are carried
    into the row.** For a boxd row, each of `BOXD_GRPC_ADDR` and
    `BOXD_TOKEN_URL` that is set becomes `driver.addr` or `driver.tokenUrl`.
    It is what that row's pools were running against, and §5 would refuse
    setting it afterwards. Carried values are the operator's: the row
    records them as carried, and §3's check, when written and at dial,
    allows a carried value as it allows one on the operator's list. A later
    write of the field drops the mark and is checked as any other. With
    neither set, no `driver` object is written, and boxd takes the defaults
    it took before.
    - The mark is server-held state in the provider's row, outside
      `config`. No request body can set it, and only this migration does.
    - A project copy does not carry it: a copied provider's endpoint is
      checked as a fresh write.
  - The row's ID, and so its disco-vm scope (`prov-x`), its pools, their
    machines, keys and runtime records, do not move. A boxd pool runs on
    with the machine it has.
- **Any other driver** (`vz`, `docker`, `libkrun`, `fake`, empty, or unknown):
  - the row keeps type `vm` and its configuration, and is disabled;
  - its pools never ran (`errHostAgentNotStaged`, or a driver the build
    never linked, which `Validate` refused);
  - nothing is deleted. The row and its pools are the user's to delete, and
    deleting them removes rows only, since no machine was made. How a pool
    of an unregistered type is deleted is implementation's.
- **`deliverProviderKey`** is dropped from every row. A pool machine that
  holds the provider's key has it taken off at its next reconcile, as
  withdrawing the opt-in takes it off today, and is given the sentinel (§9).
- **Sealing.** `SealProviderCredentials` is keyed by the stored type
  (`storedProviderCredentials`, `"vm": apiKey`). A `vm` row's clear `apiKey`
  is sealed first, under `vm`, and then moved as above, so it is never left
  clear.

### 9. A pool machine holds a sentinel for its driver's key

`deliverProviderKey` goes. A pool on a driver reached at its URLs gets its
engine's key in one of two ways:

- **An issued key**, when the driver issues one (551 §4, unchanged):
  narrower, expiring, renewed at half its life, revoked by name.
- **Otherwise, a sentinel for the driver's key secret, swapped by the pool's
  own proxy.** boxd issues none to an API key (`ErrCannotIssue`,
  `pkg/machine/boxd/keys.go`), so this is every boxd pool on an API key.
  - The pool machine's provisioning key file holds a sentinel shaped as the
    driver's key. For boxd that is `bxd_…`, which its driver exchanges for
    a token rather than sending it as a bearer.
  - The pool agent's engine reaches the driver's endpoints through the
    pool's proxy, as a client of its own: the proxy tells clients apart by
    their mutual-TLS certificate, and no sandbox or build step holds the
    pool agent's. The pool agent trusts the proxy's CA for those hosts
    alone.
  - The proxy swaps the sentinel only at the place the driver sends its
    key. For boxd that is the JSON body field `/api_key` of a `POST` to its
    `tokenUrl`, the one request that carries the key. That is an **HTTP body
    place** (#1012), following #741's WebSocket places (ADR 26-11-06-442):
    the value at a JSON pointer must be exactly a sentinel, on a named host,
    method and path, and nothing else in that request or any other is
    swapped.
  - What comes back is the driver's own short-lived credential (boxd's
    one-hour JWT), which the engine uses as it is. boxd's gRPC calls carry
    that token, never the key, so no gRPC route is needed.
  - Where a driver sends its key is the driver's to declare with its secret
    field (Consequences), and discobox writes the route from that. Its host
    is the driver's `Privileged` endpoint field, which §3 already checks.

**The server hands a provider's key to a pool's proxy only for that pool.**
Today the only way a pool learns a secret's value is resolving a sandbox's
sentinel, authorized by that sandbox's grants (`ResolveSandboxSecret`). This
is a new resolution, and it is answered only:

- to a request authorized by the asking pool's own credential;
- with the key secret of the provider that pool is bound to, never another
  provider's, and never for a pool of another type;
- for the pool agent's own client, an identity no sandbox ID can take;
- for the host, method, path and place the driver declares;
- while the pool exists and is bound to that provider. It is refused once
  the pool is deleted or rebound, and a cached value goes with the grant.

**The audit never holds the key or a token.** The proxy records ordinary
exchanges and serves them through the pool's audit API, which any project
member may read. For the pool agent's exchanges on a driver route:

- the request body is recorded as the client sent it, with the sentinel,
  never with the key;
- the response body of the exchange that returns a credential is never
  recorded or served, and a response header that carries one is redacted as
  a swapped header is.

**What it bounds, exactly.** The provider's key is never on the pool
machine's disk, in its environment, in its provisioning key file, or in
anything recorded. A build step there (767) reads a sentinel. The key is on
the pool machine only in its proxy's memory, while cached, as every swapped
secret is. Withdrawing the grant stops every new exchange, and the last
token dies by its own expiry.

**The cost, stated.** Each token the pool obtains has the key's full scope
for its life (boxd: an hour). A build that escapes onto the pool machine
could mint tokens through the proxy, or read the key from its memory.

## Alternatives rejected

- **Keep `vm`, with the driver's fields shown according to `driver`.** The
  catalog and the CLI have no fields conditional on another field's value,
  and a person picking "vm" then "boxd" is choosing boxd. The owner's
  direction is the flat list.
- **A prefix (`vm-boxd`, `vm.boxd`).** The owner's direction is no prefix.
  It also buys nothing once a clash fails the build (§1).
- **The driver's fields flat at the top level, beside discobox's.** A
  driver adding `base` or `controlPlaneUrl` would shadow a discobox field,
  or fail to register. disco-vm's strict decoder would also need discobox's
  keys stripped before every call. One `driver` object passes to disco-vm as
  it is.
- **discobox restating each driver's fields in its own `Definition`.** It
  drifts from the driver's decoder, which is what 215 exists to stop.
- **`apiKey` as discobox's field, passed as `Config.Keys`**, as 215 §7 has
  it. A key's meaning is the driver's: which drivers take one, what it is
  called, how many a driver needs. discobox would guess each from signals
  disco-vm does not give, and one string cannot hold a driver's second
  secret.
- **Keeping `deliverProviderKey`**, the provider's own key on the pool
  machine by consent. It is the account's full, never-expiring key, in a
  file on a machine where builds run (767), and discobox can neither rotate
  nor revoke it there. A sentinel keeps it off the disk and out of reach of
  anything that reads files (§9). Placing it with no opt-in at all is worse still.
- **The server minting the pool's short-lived token**: boxd's `IssueKey`
  answering an API key with an exchanged one-hour JWT, placed and renewed
  as an issued key is. It needs no proxy work, but it is a second,
  per-driver way of handing a box a credential beside the one the proxy
  already resolves, authorizes, audits and revokes for every other secret.
  It would also leave the token's refresh to the server's uptime alone.
- **A driver's secrets inside `DriverConfig`, as plain fields.** Everything
  that carries `DriverConfig` would carry them: the pool's bootstrap, its
  placement fingerprint, a shim's configuration file, disco-vm's `info`. One
  blob also cannot be sealed field by field. Secrets travel apart (§2).
- **`Privileged` fields as any member's to set, unchecked**, on the reasoning
  that whoever supplies the key may point it anywhere. An update does not
  supply the key (694 §2), and the server's dial from its own network is
  reach a member does not otherwise have (§§3–4).
- **`Privileged` fields as the server operator's alone.** Members may supply
  their own key to a boxd account, and an endpoint of the provider's own
  (a public boxd cluster) is theirs to name. The operator's list (§3) covers
  what is not public.
- **Linking vz and libkrun under other names (`vm-vz`).** Two providers of
  one hypervisor, named apart by a prefix, on one Mac. The owner's direction
  is not to link them (Deferred).
- **Retyping a `vm`/vz row to the built-in `vz`.** That is a different
  provider, with a different configuration and pool shape, and the row's
  pools never ran. Disabling it keeps what the user made, for them to delete.
- **Deleting a `vm` row whose driver is not linked.** Persisted state is
  durable.
- **Migrating a row to the driver's defaults, with a warning.** A server
  whose environment pointed boxd at a private cluster would send that
  cluster's key to boxd's default endpoint at the next start, and §5 would
  refuse the fix while the pools stand.

## Deferred

- **disco-vm's vz, libkrun and docker drivers as providers.** Revisited when
  the built-in `vz` and `libkrun` providers move onto disco-vm (106's
  Deferred "The `dockerworker` pool VMs on disco-vm", #608), which frees
  their names, or when the owner picks names for them. Until then no build
  links a held driver that carries a pool, and 963's held-driver work waits
  (Status).
- **Driver fields that may change under a provider's pools.** Revisited
  when disco-vm's definition marks which fields leave a driver's machines
  where they are (`tokenUrl`, say). Until then every one is `Immutable`
  (§5).

## Consequences

- **`server/providers/discovm`.**
  - `Config.Driver` becomes the driver's configuration object, keyed
    `driver`, in place of the driver's name.
  - `Definition` and `Validate` are per type, built from
    `machine.DefinitionOf`, with §2's per-kind rules in place of
    `validateFor`'s, and §§3–4's checks.
  - `providers.go` registers one type per linked driver, panicking on a
    clash.
  - `drivers.go` gains archil; `drivers_darwin.go` goes.
  - The bootstrap carries the driver configuration (§6).
  - `runShim` (`shim.go`) makes its driver with the configuration its file
    holds (215 §6), for hcs.
- **pool-agent.** `pool-agent/drivers.go` links archil too, and its engine
  is made with the bootstrap's driver configuration. Its engine's calls to
  the driver go through the pool's proxy as a client of its own, with the
  route §9 writes, when it holds a sentinel.
- **The proxy owes**, before §8's drop of `deliverProviderKey` and §9 land:
  sentinels swapped at a named place in an HTTP request's JSON body (#1012),
  the request-body counterpart of #741, with §9's audit rules. Until then
  no `deliverProviderKey` is dropped, so no pool that runs today stops.
- **`server/internal/resources/secrets`.** §9's resolution: a pool asking
  for its provider's key secret, authorized by the pool's own credential,
  beside `ResolveSandboxSecret`. The provider's key secret is that pool
  agent's grant, on the route §9 writes.
- **`server/go.mod` and `pool-agent/go.mod`** pin disco-vm at `8b07714` or
  later, and at PR 69's merge for a working region.
- **`server/internal/resources/providers`.** §4's rule beside
  `refuseImmutableChange`, and §3's endpoint check with the server setting
  that allows more.
- **`server/providers/discovm`'s provisioning key** (`poolkey.go`). The
  provider's own key is never placed. A driver that cannot issue gets the
  sentinel and its grant (§9).
- **`server/internal/sandbox` and the API.** `ProviderConfigField` gains
  what the catalog needs to show a driver field (`Default`). Its `Immutable`
  covers a nested key.
- **CLI.** `admin provider create` and `update` build `driver.<key>` fields
  into the `driver` object, and `--help=boxd` lists them.
- **`server/internal/database` and `server/internal/store`.** §8's
  migration, and a `REVIEW.md` rule that a migration names provider types by
  a fixed list.
- **Docs.** The providers and their fields are listed. `discovm/DESIGN.md`
  and `server/providers/DESIGN.md` describe the per-driver types, with the
  code.
- **disco-vm owes discobox**, before the implementation lands:
  - **secret fields in a driver's definition**, superseding 215 §7's "no
    field holds a secret", passed to the engine apart from `DriverConfig`,
    asked on every call, and kept out of a shim's configuration file and
    everything disco-vm prints (§2). How `Config.Keys` and issued keys fold
    into them is disco-vm's to decide;
  - **where a driver sends each secret**, declared with its secret field
    (host field, method, path, and the header or JSON body place), so
    discobox writes §9's route without knowing the driver;
  - whether a driver takes a base, readable from its definition, to replace
    §2's stand-in;
  - a dialer its engine's service connections use, given by the embedder,
    for §3's check at dial.
- **Issues.** #997 closes into #1006.
