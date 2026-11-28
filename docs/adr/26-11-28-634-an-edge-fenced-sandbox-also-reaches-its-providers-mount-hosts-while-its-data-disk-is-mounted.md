# 26-11-28-634 — An edge-fenced sandbox also reaches its provider's mount hosts while its data disk is mounted

- **Status**: Accepted
- **Date**: 2026-11-28
- Accepted by the owner by merging #1845, 2026-11-28
- **Issue**: #1828 (found while working on #1754)
- **Supersedes in part**
  [26-11-16-619](26-11-16-619-a-pool-on-a-driver-with-no-networks-fences-each-sandbox-at-the-providers-edge-and-starts-systemd-in-the-guest.md)
  (its implementation has shipped, so this supersedes rather than amends):
  - §1, guarantee 2's "The allow-list names the host the pool machine is
    reached at and nothing else". On archil it also names Archil's mount
    hosts while the sandbox's data disk is mounted (§1 below). The rest of
    guarantee 2 (DNS closed, Archil's resolvers denied) and guarantees 1, 3
    and 4 stand.
  - Context's "A guest with a data disk holds that disk's mount token,
    readable by root", and the Open item on replaying that token. No mount
    token was found in the guest (§2).
- **Relates to**:
  - [26-11-24-077](26-11-24-077-disco-vm-holds-a-pool-discobox-asks-for-its-host-and-members-and-the-driver-fences-them.md)
    (Proposed), whose archil row restates guarantee 2 as "the host's public
    name alone". It would carry §1's exception if both are accepted.
  - [26-11-02-023](26-11-02-023-every-machine-sandbox-mounts-its-volumes-from-its-own-data-and-cache-roots.md)
    (a machine sandbox's data volume) and
    [26-11-23-458](26-11-23-458-a-machine-sandbox-is-provisioned-in-place-and-only-docker-and-containerd-restart-around-its-mounts.md)
    (a machine created started, with its disk mounted at create).
  - discobox-ai/vm#236 (the driver reads a fence back without the mount
    hosts), discobox-ai/vm#216 (no mount token in the guest) and
    discobox-ai/vm#238 (the live check of what the hosts reach).

## Context

619 §1 holds an edge-fenced sandbox (archil) to four guarantees. Guarantee 2
says its allow-list "names the host the pool machine is reached at and
nothing else". On archil that has not held since its sandboxes got data
disks:

- **Archil adds two hosts while a sandbox has a mount.** A deny-by-default
  sandbox that mounts a disk, at its create or at a start, has
  `mount.*.archil.com` and `data.*.archil.com` appended to its `allow`. They
  stay through any policy set while it is mounted and through a stop, and
  go at a start with no mounts. A `PUT /network` without them reads back
  with them. Seen on Archil 2026-11-26 (discobox-ai/vm#236, #238).
- **discobox cannot take them away**, and the archil client needs them to
  mount the disk: it dials `mount.<cell>.<region>.<cloud>.prod.archil.com`
  on 8100, which hands back a data endpoint by IP.
- **Today's code accepts them.** At the pinned disco-vm (`4ec3d57`), the
  archil driver's `fenceOf` reads a fence back without them, `checkAllow`
  refuses a fence that names them, and a policy set keeps them (disco-vm
  `pkg/machine/archil/edge.go`, `mountHosts`). So a machine created started
  with its data disk (#1754) reads back as fenced as asked, and the pool
  agent's `fenced` check (`pool-agent/sandboxruntime/machine.go`) accepts a
  found machine that reaches them. Before #1754 a machine was created
  stopped and fenced, and its first start mounted the disk and added the
  hosts. No start checks the fence, so this already happened then.
  `pool-agent/DESIGN.md` (One machine per sandbox) and
  `server/providers/discovm/DESIGN.md` already say so and point here.

**What the hosts reach** (discobox-ai/vm#238, Archil aws-us-east-1,
2026-11-26, disco-vm `4ec3d574`):
- The rest of the fence held: Archil's API (`control.…`) and `s3.…` hosts
  and any other host were refused. An Archil API key in the guest reaches
  nothing.
- With a disk's own token, minted through Archil's API outside the guest and
  copied in by hand, the fenced guest mounted that disk read-write. The
  mount host is regional and names no account. Other regions' mount servers
  were reachable too, up to their authorization refusal.
- A `deny` of a host takes precedence over Archil's allows, so another
  region's mount host can be denied. Within a region, nothing narrower is
  possible.
- Not tried: one disk's token against another disk, and a token for a disk
  of another Archil account. Whether a token is scoped to its disk is not
  known. 619's spike saw a token taken from one sandbox fail authorization
  in another, which suggests a binding but tests something else.

**The mount token.** 619's Context says a guest with a data disk holds that
disk's mount token, readable by root. Checked on 2026-11-23 (discobox-ai/vm#216;
Archil client version 1006, `debian:sid`, two VMs, as root) and not found:
- no token in any file under `/run`, `/etc`, `/var/lib` or `/root`, nor in
  the environment, and `archil status` and the CLI print none;
- `archil mount` from inside the guest fails for want of a token, for its own
  disk as for another VM's;
- Archil's client mounts the disk from outside the namespaces guest
  operations run in (systemd's, 619 §4), from a handshake its log says was
  baked in.

What was not checked: whether the guest's root can reach the client's
process or namespace and read a token from it, and whether a later client
version or another base differs. disco-vm's archil driver documents the
finding under `Bare` (`docs/drivers/archil.md`).

## Decision

### 1. Guarantee 2 is narrowed: the pool's host, and the provider's mount hosts while a data disk is mounted

An edge-fenced sandbox's egress allow-list names the host the pool machine is
reached at and, while the sandbox's data disk is mounted, the hosts its
provider adds for that mount. On archil those are `mount.*.archil.com` and
`data.*.archil.com`. Nothing else is allowed, and DNS stays closed, as 619
§1 says.

- **The driver owns which hosts these are.** disco-vm's driver names them,
  reads a fence back without them, and refuses a fence that names them. A
  fence discobox asks for never lists them, and discobox matches no
  provider host by name.
- **Any other difference still refuses the machine.** A fence that reads back
  with a host other than these is not fenced (`fenced`, the create's
  read-back), as today.
- **No region deny.** The driver does not deny other regions' mount hosts.

**Why it is accepted:**
- The hosts are the provider's, needed to mount the disk, and cannot be
  removed.
- No credential they accept was found in the guest (§2). A guest reaches a disk
  through them only with a token someone copied into it, and a credential
  copied into a box is a risk on any pool and any driver, which no fence
  prevents (owner, 2026-11-28: "if a token of any type is manually copied to
  the host then it will be bad. So the user shouldn't do that, we can't
  prevent that"). So whether a token of another Archil account opens its
  disk from a fenced sandbox, which discobox-ai/vm#238 did not try, decides
  nothing here.
- A region deny would close nothing a copied token needs: a disk can be made
  in the sandbox's own region. It would also need keeping current as Archil
  adds regions.

### 2. The guest holds no mount token, as far as it has been checked

619's Context is corrected: no mount token was found in the guest, and the
guest cannot mount its own disk itself (Context). Guarantee 4 stands on
that.

This ADR rests §1 on it, so what is unchecked matters:
- **A token for the guest's own disk alone** would change nothing. It is the
  guest's own (619 §1.4).
- **A token that opens other disks of the account**, if the guest's root can
  read one from Archil's client, would breach guarantee 4. Through the mount
  hosts it would reach other sandboxes' data disks, around the pool's proxy.

Both are open (below). The replay question in 619's Open is replaced by
them.

## Alternatives rejected

- **Refuse a machine whose fence reads back with the hosts.** Every archil
  sandbox with a data disk would fail, at create (since #1754) or at its
  first start's read-back. Archil pools would not run.
- **Give an edge-fenced sandbox no data disk**, so Archil never adds the
  hosts. Its data would live on the machine's root and be lost at a new
  root, an upgrade or a snapshot's, against 023's data volume, to close a
  path that needs a credential copied in.
- **Deny every other region's mount host.** It narrows the hosts to the
  sandbox's region, but a disk can be made there (§1), and the list needs
  upkeep.
- **Treat a disk token as something a fence must stop.** No fence stops a
  credential copied in from being used where its fence allows (§1). What a
  fence keeps out is the account's own, and none of it was found in the guest (§2).

## Deferred

- **A mount bound to the sandbox's own disk.** If Archil binds a sandbox's
  mount traffic to the disks it mounts, or to its organization, the driver
  uses it. Revisit when Archil offers that.

## Open, to check on Archil

Each is a live run in discobox-ai/vm, and neither changes code unless it
finds a token:
- **Is a mount token scoped to its disk?** Mount disk B from a fenced guest
  with disk A's token, both of one organization.
- **Can the guest's root read the token Archil's client mounts with?** As
  root in the guest, through the namespaces a guest operation runs in and
  around them (a fresh `/proc`, `nsenter` to PID 1, the client's memory).

If the guest's root can read a token that opens other disks, §1's acceptance
no longer holds and this is revisited. The code and design docs otherwise
match §1 already (Context).

## Consequences

- **disco-vm:** unchanged. Its archil driver already treats the hosts as
  Archil's (discobox-ai/vm#236).
- **pool-agent, server:** unchanged. `pool-agent/DESIGN.md` and
  `server/providers/discovm/DESIGN.md` cite this ADR where they now cite
  #1828, and drop the hosts' reach being unverified, once it is accepted.
- **619** is marked superseded in part by this ADR, in its Status line,
  once this is accepted.
