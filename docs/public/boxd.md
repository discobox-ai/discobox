# Running boxes on boxd

[boxd](https://boxd.sh) runs Linux microVMs in the cloud. A `boxd` provider
lets a Discobox server run its pools and boxes there, from any host. The
server needs no public URL: it opens a channel to each pool's machine.

This page sets one up, and says what its API key can reach and who holds it.
For providers and pools in general, see
[Concepts](concepts.md#providers-and-pools).

## What runs where

- **The server** holds the provider's API key. It uses it to check the
  account, to make, repair and delete each pool's own machine, and, when a
  pool is deleted, to delete every machine and snapshot the pool made.
- **Each pool** runs on one boxd machine of its own: the pool agent, the
  pool's proxy and its builder. It makes a machine for each box, and one more
  while it builds a snapshot that later boxes start from.
- **Each box** runs on a machine of its own. The machine is isolated: its only
  route out is the pool's proxy, which applies the box's egress policy and
  swaps its secrets' sentinels.

## Prepare a boxd account

Give each Discobox project that has a boxd provider a boxd org of its own,
used for nothing else. The key and a pool machine's token reach every machine
the key's owner has in the org (see
[What the key can reach](#what-the-key-can-reach)), so anything else there is
in reach of every pool too. That includes another project's pools: their
proxies hold the real values of their boxes' secrets. Two projects sharing
one org and owner share one trust boundary.

The account carries pools only if:

- **neither the org nor the key's owner holds a host-bound secret.** boxd
  lets every machine in their scope reach the hosts such a secret is bound
  to, whatever the machine's egress fence says, so a box could reach them
  too. Keep those secrets out of the org Discobox uses, and out of the key
  owner's personal scope.
- **the org's machine limit has room.** Each pool runs one machine for itself,
  one per box, and one while it builds a snapshot. boxd counts stopped
  machines too. A new org without a payment method may have only 2: a pool
  and one box, with no room to build a snapshot, so every box provisions
  from scratch. Add a payment method, or ask boxd for a quota of at least 4.

Create an API key (`bxd_…`) with `boxd auth keys create`, as the org's member
Discobox acts as. Save it to a file only you can read. The CLI reads a secret
only from a file or from stdin, never from the command line.

## Create the provider

```sh
discobox admin provider create --help=boxd
discobox admin provider create --type boxd --name boxd --driver-region fra \
  --secret driver.apiKey=@boxd-key
discobox admin provider check boxd
```

`--help=boxd` lists the fields of the server you are connected to:

| Field | Flag | What it is |
| --- | --- | --- |
| `driver.apiKey` | `--secret driver.apiKey=@FILE` | The boxd API key. Required. |
| `driver.region` | `--driver-region` | The region the provider's machines and snapshots are in, such as `fra` or `nyc`. Required. The server asks boxd for the region's endpoint. The org must have the region enabled. |
| `driver.addr`, `driver.tokenUrl` | `--driver-addr`, `--driver-token-url` | Advanced: the boxd cluster's endpoints, where the key is sent. Leave them empty for boxd's own. `driver.addr` is the cluster's home endpoint: the region's endpoint is the one it lists for `driver.region`. |
| `base` | `--base` | Advanced: the boxd image a machine is made from; empty is boxd's default. It must have `nft`. Name only an image your account controls: it runs as root on every pool machine. |
| `controlPlaneUrl` | `--control-plane-url` | Where pool machines reach the server. Leave it empty and the server opens a channel to each pool machine instead. |

The pool policy fields (`--help=boxd` lists them) apply as on any provider.
Every `driver` field is fixed while the provider has pools: delete the pools
to change one.

Creating a provider checks only its config. `discobox admin provider check`
([Concepts](concepts.md#vm-providers-boxd-archil-and-hcs) explains its
answers) asks boxd, with the key, whether the account can carry the
provider's pools: the same check the server makes before it makes a pool's
machine (below).

An endpoint field is where the key is sent, so the server checks it. It must
be a public host over https, or a public `HOST:PORT`, and a write that sets or
changes one must carry `driver.apiKey` in the same request. So a key is only
ever sent to an endpoint chosen by someone who had that key. To allow a
private endpoint, such as your own boxd cluster, the server's operator lists
it in `providerEndpoints` (`DISCOBOX_PROVIDER_ENDPOINTS`).

## Create a pool and run a box

```sh
discobox admin pool create boxd --provider boxd
discobox admin pool get boxd
discobox run --pool boxd
```

The pool is ready when `admin pool get` shows it `active`. The first box of a
kind (its harness and setup) provisions from scratch, and takes longest.
Behind it, the pool builds a snapshot of that kind on a machine of its own;
boxes of that kind made once the snapshot is ready start from it. Make the
pool the project's default with `discobox admin pool set-default boxd`.

A box that sleeps (`discobox-power sleep`) or is paused (`discobox admin box
pause`) on a boxd pool is frozen whole: boxd suspends its machine, keeping its
memory and every process, and the box carries on where it was when it next
starts. boxd may bill a suspended machine's memory until it is resumed or
stopped.

Before it makes a pool's machine, the server checks the account through
boxd's API: that the key is accepted, that the org has the provider's region
enabled, and that no host-bound secret is in the org or the key owner's
personal scope. The pool fails with the reason if boxd refuses, and waits if
boxd does not answer. A running pool's account is checked again every ten
minutes, which reads the org's machine limit: the pool's `runtime` condition
shows it as `maxMachines`, counting every machine billed to the org, stopped
ones and other members' included. A check that fails then is logged, and the
pool keeps running.

## What the key can reach

The key is the provider's, and it never leaves the server. Here is who holds
what, and what each can do with it.

| Where | Holds | Which reaches |
| --- | --- | --- |
| The server | the API key, in its database as it stores secrets (encrypted when the server has encryption configured); never returned by the API, the CLI or the console | everything the key's owner can do in the org, including managing its API keys |
| A pool's machine | no key and no sentinel. It authenticates as itself, with the token boxd mints for the machine's owner (the key's owner) | every machine the owner has in the org, other pools' included: list, create, snapshot, run commands in, delete. The org's environment variable values (not secret values). Not API keys, and not its own machine's egress allowlist |
| A box's machine | no key, and no way to get a token: boxd gives an isolated machine none. Its secrets are sentinels | only what its egress policy allows, through the pool's proxy |

What bounds it:

- **The key** is used by the server alone. A pool, a box and a build never
  receive it.
- **A pool machine's token** lives fifteen minutes, and only the pool agent
  reaches the endpoint that mints it. A rule set on the pool machine keeps
  the proxy, the builder and every box's traffic from that endpoint, and a
  pool machine that cannot load the rule runs no pool. Deleting the pool
  machine ends its credential: its last token dies within fifteen minutes.
- **A box** runs code you have not reviewed, on a machine of its own. It
  holds sentinels, never the values, and leaves only through the pool's
  proxy.

What it does not bound:

- **Root on a pool's machine**, such as a build that escapes onto it, can
  mint a token whenever it likes, with the reach above: it could delete or
  run commands in every machine the owner has in the org, and make machines
  of its own there, which can mint the owner's token too. boxd offers
  nothing narrower for a pool machine. This is why the org should be the
  project's alone.
- **Who may write a provider**, today any member of the provider's project,
  can use the key through it, and more. They cannot read the key back, nor
  send it to an endpoint of their own without supplying it. But they can set
  `base`, which is not fixed while the provider has pools, to an image they
  choose. That image runs as root on the pool's machine, so it gets the reach
  above. Copying a project with its providers copies the key, sealed, into
  the new project, whose members then have the same use of it.

## Rotate or remove the key

To rotate the key, create a new one in boxd for the same member of the same
org, then:

```sh
discobox admin provider update boxd --secret driver.apiKey=@new-boxd-key
```

The server takes the new key on its next call to boxd, with no restart. Pools
keep running, since their machines authenticate as themselves. Then revoke
the old key in boxd. A key of another member or another org cannot see the
pools' machines: the server makes new ones, and the old ones keep running in
the old account, where Discobox can no longer delete them.

If you think the key leaked, or a pool machine was taken over:

1. Revoke the key in boxd, and set a new one as above.
2. Delete the pools whose machines may be taken over: that deletes their
   machines, which ends their credentials within fifteen minutes.
3. In boxd, delete every machine in the org that Discobox did not make, and,
   if the key leaked, every API key you did not create. Either credential
   could have made a machine that keeps minting the owner's token, or a key.

To remove everything, in order:

```sh
discobox admin box purge BOX...       # every box of the pool, with its machine
discobox admin pool unset-default     # if the pool is the project's default
discobox admin pool rm boxd           # its machine, snapshots and box machines
discobox admin pool ls                # until the pool is gone
discobox admin provider rm boxd
```

A pool with boxes, or the project's default pool, cannot be deleted. `pool
rm` returns before the pool is gone: the server then deletes the pool's
machines and snapshots with the provider's key, and a provider cannot be
deleted while it still has the pool. Delete the key in boxd only after the
provider is gone.

## Upgrading a provider without a region

A boxd provider made before the region was required has none, and it stops
working: `admin provider check` answers `refused`, `region is required`, and
its pools stay `pending`. The region cannot be added while the provider has
pools, and the server cannot reach boxd to delete them, so disable the
provider first, then delete its pools and the provider, and create them
again with `--driver-region`, as above:

```sh
discobox admin provider update boxd --disabled
discobox admin pool rm boxd
discobox admin provider rm boxd
```

A disabled provider's pools are deleted without calling boxd, so their
machines and snapshots are left there. Delete them in boxd: their names
start with `dvm-prov-` and `dvm-pool-`, followed by the provider's or the
pool's ID in lower case, with `-` for `_`.

## Upgrading a `vm` provider

On upgrade, a `vm` provider on boxd becomes a `boxd` provider with the same
ID, pools and key. If the server's environment set `BOXD_GRPC_ADDR` or
`BOXD_TOKEN_URL`, those values move into the provider's `driver` fields. It
has no region, so it is then a provider without a region (above). See
[Concepts](concepts.md#vm-providers-boxd-archil-and-hcs) for `vm` providers
on other drivers.

If the provider had `deliverProviderKey` on, its pool machines held the key
itself. They lose it at their next reconcile and authenticate as themselves
instead, but a copy taken before then still works: rotate the key, as above.

## Behind an egress proxy

A server behind an egress proxy reaches boxd through it, by name. See
[Concepts](concepts.md#vm-providers-boxd-archil-and-hcs).
