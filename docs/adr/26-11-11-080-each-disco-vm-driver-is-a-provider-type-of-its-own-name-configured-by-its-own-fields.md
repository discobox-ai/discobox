# 26-11-11-080 — Each disco-vm driver is a provider type of its own name, configured by its own fields

- **Status**: Accepted. It supersedes, and each superseded ADR's status line
  says so:
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
    authenticates in-machine where its driver gives it a credential (boxd),
    and otherwise holds a sentinel, which its proxy swaps for the key at the
    one place the driver sends it (§9). §4's issued key, and its bounds,
    stand, but for a driver that authenticates in-machine, which is issued
    none (§9). So does its "A key wider than the pool is acceptable where the
    driver offers nothing narrower", which now also covers §9's in-machine
    token, the tokens a sentinel obtains, and the key in the proxy's memory.
    Its "the pool records whether its key is issued or the provider's" reads
    "issued, in-machine, or a sentinel for the provider's".
- **Narrows**:
  - 26-11-11-963, "One vm provider type: every pool runs a Linux pool agent
    in a machine its driver makes" (accepted in #1003):
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
- **Accepted**: by the owner merging #1007, 2026-11-12. disco-vm's debts
  (Consequences) are met by its ADR 26-11-12-461 (vm#88).
- **Amended**, both before anything shipped against it:
  - 2026-11-13, §9: a boxd pool authenticates in-machine, by the owner
    merging PR 1093 (#1092);
  - 2026-11-13, §3's check at dial through a proxy, by the owner on #1006.
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
  - #1090, the spike that found a boxd pool machine already holds an
    in-machine credential, and on which the owner chose it for boxd's pools
    (2026-11-13); disco-vm's in-machine credential source for boxd (vm#109).

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
  provisioning key file.** A pool that authenticates in-machine has no
  such file (§9). Otherwise it holds an issued key (551 §4), or else a
  sentinel for the driver's key secret, which its pool's proxy swaps (§9),
  and the key is on the pool machine only in that proxy's memory, while
  cached. An issued key is a value for the driver's key secret, so the pool
  agent's engine calls with it as the provider's engine calls with the
  provider's.
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
- **Through the server's egress proxy**, the dial sends the name as named
  and the proxy resolves it; only a literal non-public address is refused
  there. A proxy in the server's environment is the operator's, outside
  discobox's control, and what it lets the server reach is its policy: an
  operator whose proxy reaches an internal network refuses private
  addresses there (owner, 2026-11-13, #1006). The dial without a proxy is as
  above.
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
  withdrawing the opt-in takes it off today, and is given what §9 gives its
  driver: nothing on boxd, which authenticates in-machine; a sentinel on a
  driver that does not.
- **Sealing.** `SealProviderCredentials` is keyed by the stored type
  (`storedProviderCredentials`, `"vm": apiKey`). A `vm` row's clear `apiKey`
  is sealed first, under `vm`, and then moved as above, so it is never left
  clear.

### 9. A pool machine authenticates in-machine, or holds a sentinel for its driver's key

`deliverProviderKey` goes, and the provider's key is never placed on a pool
machine. A pool on a driver reached at its URLs authenticates its engine in
the first of these ways its driver offers:

- **In-machine**, when the driver gives a pool machine a credential of its
  own. This is every boxd pool.
- **An issued key**, when the driver issues one (551 §4, unchanged):
  narrower, expiring, renewed at half its life, revoked by name.
- **Otherwise, a sentinel for the driver's key secret, swapped by the pool's
  own proxy.** No linked driver takes this way today: archil's pools are
  refused (§1). It is archil's once disco-vm gives it a fence, and any
  driver's that has neither of the others.

**In-machine, on boxd.** boxd gives a private machine that is not isolated
a metadata endpoint on its gateway, which mints a token for the machine's
owner on every read. An isolated machine gets none (#1090).

- **The pool machine is private and not isolated**, as disco-vm makes it
  now, on a network of the pool's own. Not isolated is also what lets its
  isolated sandboxes reach it. It is never shared: a shared machine's token
  cannot snapshot a machine or set its egress allowlist.
- **Nothing is placed.** The pool machine gets no key, no sentinel and no
  provisioning key file, and is issued no key. The provider's key is used
  by the server alone, to create, repair and delete the pool machine.
- **The pool agent's engine uses disco-vm's in-machine credential source**
  (vm#109): it reads the endpoint, caches the token until near its expiry,
  and calls the driver's endpoints with it directly. It does not go through
  the pool's proxy, and the pool has no grant, route or body place for it.
  Its bootstrap names no key secret.
- **Nothing to revoke per pool.** The credential goes with the machine:
  deleting the pool machine ends it, and its last token dies within fifteen
  minutes.

**What the in-machine token reaches, stated.** It is the machine owner's
`member` credential, fenced to one org and to the machine's life, as #1090
observed it:

- it lists, creates, snapshots, runs commands in and **deletes every machine
  the owner has in the org**, other pools' included, and reads the org's env
  var values (not secret values);
- it cannot manage API keys, nor change its own machine's egress allowlist;
- it lives fifteen minutes, and cannot be revoked apart from deleting the
  machine;
- root on the pool machine, such as a build that escapes onto it (767), can
  mint one whenever it likes.

That is the provider's key's reach, less key management, with shorter-lived
tokens. boxd has nothing narrower for a pool machine (Deferred), and the
machine holds this credential whatever discobox places on it, so no sentinel
would narrow it (Alternatives rejected).

**The sentinel, on a driver without in-machine auth.**

- The pool machine's provisioning key file holds a sentinel shaped as the
  driver's key.
- The pool agent's engine reaches the driver's endpoints through the
  pool's proxy, as a client of its own: the proxy tells clients apart by
  their mutual-TLS certificate, and no sandbox or build step holds the
  pool agent's. The pool agent trusts the proxy's CA for those hosts
  alone.
- The proxy swaps the sentinel only at the place the driver sends its
  key: a header, or an **HTTP body place** (#1012), following #741's
  WebSocket places (ADR 26-11-06-442), where the value at a JSON pointer
  must be exactly a sentinel, on a named host, method and path. Nothing
  else in that request or any other is swapped.
- Where the driver exchanges its key for a short-lived credential, what
  comes back is used by the engine as it is, and calls that carry it need
  no route.
- Where a driver sends its key is the driver's to declare with its secret
  field (Consequences), and discobox writes the route from that. Its host
  is the driver's `Privileged` endpoint field, which §3 already checks.

**The server hands a provider's key to a pool's proxy only for that pool.**
Today the only way a pool learns a secret's value is resolving a sandbox's
sentinel, authorized by that sandbox's grants (`ResolveSandboxSecret`). This
is a new resolution, and it is answered only:

- to a request authorized by the asking pool's own credential;
- with the key secret of the provider that pool is bound to, never another
  provider's, and never for a pool of another type, nor for a pool that
  authenticates in-machine;
- for the pool agent's own client, an identity no sandbox ID can take;
- for the host, method, path and place the driver declares;
- while the pool exists and is bound to that provider. It is refused once
  the pool is deleted or rebound, and a cached value goes with the grant.

**The audit never holds the key or a token.** The proxy records ordinary
exchanges and serves them through the pool's audit API, which any project
member may read. For the pool agent's exchanges on a driver route:

- the request is recorded as the client sent it, with the sentinel, never
  with the key;
- the response body of an exchange that returns a credential is never
  recorded or served, and a response header that carries one is redacted as
  a swapped header is.

**What a sentinel bounds, exactly.** The provider's key is never on the pool
machine's disk, in its environment, in its provisioning key file, or in
anything recorded. A build step there (767) reads a sentinel. The key is on
the pool machine only in its proxy's memory, while cached, as every swapped
secret is. Withdrawing the grant stops every new exchange, and the last
token dies by its own expiry.

**The sentinel's cost, stated.** Each request the engine sends through the
proxy, and each token it obtains, has the key's full scope. A build that
escapes onto the pool machine could use the proxy as the engine does, or
read the key from its memory.

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
  nor revoke it there. §9 keeps it off the machine: in-machine auth places
  nothing, and a sentinel keeps it off the disk and out of reach of anything
  that reads files. Placing it with no opt-in at all is worse still.
- **A sentinel for boxd's key, swapped in its token exchange's body**, as
  this ADR first had it. A boxd pool machine is private and not isolated,
  and boxd gives every such machine a token of the owner's member scope
  (§9), whether or not it holds a sentinel. So the sentinel narrows nothing
  root on the pool machine can do. It still costs a grant of the provider's
  key to the pool's proxy, a body place (#1012), and the proxy's CA trusted
  for boxd's hosts by the pool agent's engine, which disco-vm cannot do
  today. In-machine auth takes the key and the sentinel off the machine
  entirely, with shorter-lived tokens (fifteen minutes, not an hour) that
  cannot manage keys (#1090).
- **An issued key for a boxd pool**, where boxd would issue one (551 §4):
  boxd's issued keys are `member` keys too (disco-vm ADR 26-10-24-562), so
  one reaches no less than the in-machine token, and sits on the pool
  machine's disk for days rather than minutes.
- **A shared boxd pool machine**, whose in-machine token is narrowed to the
  machines it created. That token cannot read its owner (`Whoami`), snapshot
  a machine or set one's egress allowlist, so disco-vm cannot run a pool on
  it (#1090).
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
- **Through a proxy, resolving the name on the server and connecting to the
  checked address**, so a rebinding name could not answer inward at the
  proxy. A TLS-intercepting proxy mints its certificate for the address it
  is asked for, and a hostname-only proxy refuses an address, so every
  driver call failed behind one (a discobox box's own egress among them);
  and the proxy is the operator's to police (§3).
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

- **A boxd in-machine token narrowed to the machines its pool machine
  created**, as boxd narrows a shared machine's. A private machine's token
  is not narrowed today. Revisited when boxd offers it for a private
  machine, which would make in-machine auth narrower than any key; it is to
  be asked of boxd.

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
  is made with the bootstrap's driver configuration. On a driver that
  authenticates in-machine (boxd), its engine uses disco-vm's in-machine
  credential source and calls the driver's endpoints directly. When it
  holds a sentinel, its engine's calls to the driver go through the pool's
  proxy as a client of its own, with the route §9 writes.
- **§8's drop of `deliverProviderKey` waits** on what its pool is given
  instead: on boxd, disco-vm's in-machine credential source (vm#109) in
  discobox's pin; on a driver that takes a sentinel, the proxy's sentinels
  swapped at a named place in an HTTP request's JSON body (#1012), the
  request-body counterpart of #741, with §9's audit rules. Until then no
  `deliverProviderKey` is dropped, so no pool that runs today stops. boxd's
  pools do not wait on #1012.
- **`server/internal/resources/secrets`.** §9's resolution, for a pool that
  holds a sentinel: a pool asking for its provider's key secret, authorized
  by the pool's own credential, beside `ResolveSandboxSecret`. The
  provider's key secret is that pool agent's grant, on the route §9 writes.
  No boxd pool has one.
- **`server/go.mod` and `pool-agent/go.mod`** pin disco-vm at `8b07714` or
  later, and at PR 69's merge for a working region.
- **`server/internal/resources/providers`.** §4's rule beside
  `refuseImmutableChange`, and §3's endpoint check with the server setting
  that allows more.
- **`server/providers/discovm`'s provisioning key** (`poolkey.go`). The
  provider's own key is never placed. A driver that authenticates in-machine
  gets no key file and is issued no key; one that cannot issue gets the
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
  code. `discovm/DESIGN.md` states the reach of a boxd pool machine's
  in-machine token (§9) where it shows the pool's `MachineToken`.
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
    for §3's check at dial;
  - **an in-machine credential source** (vm#109): boxd's driver reads its
    machine's metadata endpoint and caches the token until near its expiry,
    on a machine where no key is given. Whether a driver authenticates
    in-machine, and what its pool machine must be for that (boxd: private,
    not isolated), is readable from its definition, so discobox picks §9's
    way without knowing the driver. That boxd's endpoint and token lifetime
    hold is the driver's to keep: neither is in boxd's documentation.
- **Issues.** #997 closes into #1006.
