# Pool Agent Review Notes

## Ownership of host-backed roots

`prepareSandboxVolumes` provisions the roots that become container binds. The
two helpers are not interchangeable, and the question that picks between them is
*who writes the contents*, not how big they are:

- **`prepareOwnedTree` is for roots this agent materializes end to end** — the
  per-sandbox config and secrets trees (end to end at create only: once the
  container exists, their contents are the sandbox agent's, written through the
  runtime-config intake), and each pool-side origin repository. It wrote every
  byte and one sandbox bounds the size, so asserting ownership over the tree —
  root for config and secrets, the sandbox user for origins — is both cheap and
  meaningful.
- **`prepareOwnedMountpoint` is for everything else** — the sandbox's data root,
  the sources root, the pool cache, and each source's data-key directory. A
  bind-mount source needs only its own ownership to be right; what lives inside
  belongs to whoever wrote it.
- **A source's checkout is neither.** The sandbox clones it, as the user boot
  gives the directory to (ADR 0126 §4); the pool creates the empty directory
  once and never chowns, reads, or runs git in what is under it. Anything the
  pool needs to know about a checkout — whether it is materialized, its project
  layer — it asks the sandbox.

Getting this wrong on the pool cache is a latency bug: it grows without limit
(tens of GB and ~10^6 inodes in normal use), so a recursive chown there costs
~37s of every sandbox create on a cold page cache, and it takes back ownership
sandbox-agent gives the cache paths it wires for the sandbox user (ADR 0094).

Getting it wrong on the **data root is a correctness bug**: that tree is the
sandbox's `$HOME`. Unarchiving is a create against a tree that is already full
(ADR 0022 §6), so a `chown -R 0:0` there hands root every file the sandbox has
ever written. Do not answer this with "the sandbox chowns it back on boot" —
boot does not walk home at all: it chowns only what it creates
(`sandbox-agent/REVIEW.md`, "Boot cost"), so nothing on the sandbox's side
repairs what a walk here gets wrong.

Rule of thumb: on the create path, walk only what this agent wrote.

## A sandbox container has no `WorkingDir`

- **Never set `container.Config.WorkingDir` for a sandbox.** Docker creates
  a working directory the image lacks, root-owned, in the container's own
  layer, before boot runs. For a checkout kept at its host path that is a path
  under the user's home, which boot then leaves root's (it chowns only what it
  creates) and which turns home's volume into an overlay. It is not how the
  harness's start directory arrives: PID 1 is systemd, which runs from `/`,
  and the sandbox agent starts every exec where `sandbox.json` says
  (`sourceStartDirectory`, ADR 26-10-15-664 §3) (#234, ADR 26-10-15-417).

## Nothing is written into a sandbox after create

The pool's writes into a sandbox's config volume are the bootstrap, before
its container exists, and the keypair its agent serves TLS with, before each
start (ADR 26-10-29-481 §3). Both go through an `os.Root`
(`writeRootFile`): the volume is the sandbox's to write, and is reused when its
container is replaced, so a link it planted at `sandbox.json`, `agent-tls` or
a `.tmp` beside either is replaced or refused, never followed onto the host
the pool agent writes as root. Both writes followed links once; anything else
the pool writes there goes through the same root. Everything after that — secrets, proxy material, the idle
timeout, readiness — is a change to the runtime-config record
(`decideRuntimeConfig`) and a delivery, never a file. And nothing the pool
decides is read back from the sandbox's volumes, which the sandbox can write:
keep a record of your own beside the tree (`runtime-config.json`,
`project-layer.json`, `file-secrets.json`) instead (ADR 26-10-08-127).
`sandbox.json` above all: the pool writes it once, and from then on it is the
box's to replace — with a symlink to `/dev/zero` or a FIFO, which the pool
agent, as root on the host, would follow and read without bound. It has been
read back twice (`recordedSandboxUser`, and the credential environment before
`file-secrets.json`); record what you need from the request that wrote it.

A machine is the exception, and only for what it boots on: a machine can be
copied into only while it runs, so `MachineSandboxRuntime.place` copies
sandbox.json, the agent's TLS keypair where it serves TLS, the agent layer,
its units and its pins into a running machine at every start, and restarts it
when what it boots on moved (ADR 26-10-24-953 §2, ADR 26-10-27-345, ADR
26-10-29-481 §3). The keypair is no part of what moved: the agent reads it
afresh for every handshake. Nothing else is written into a machine either: no
record of the pool's, and nothing into the sandbox's own files.

The one other write is the agent's placement in a delivered sandbox's
container, before each start and never into a running one: the agent layer,
its units and `pins.json` (`placeAgent`, ADR 26-10-16-423 §7 as amended).
It is pre-boot placement as the bootstrap is — what this boot runs on, which
the agent reads before any delivery could reach it — and what was placed is
the pool's record (`agent-placed.json`), never read back from the box. Do not
widen it to anything a running box must take: that is the runtime-config
document's.

## Sandbox user ids

The pool host cannot resolve a sandbox's account (ADR 0025 §4). An id the
request did not give stays `unsetID` (-1, the chown(2) sentinel) rather than
being guessed — see `chownID`/`chownSpec`.

## Subprocesses

`exec.Cmd.Run`, `Output`, `CombinedOutput` and `Start` are not for this module:
the agent is PID 1 and reaps orphans, so a child it does not know it owns is one
the reaper may collect out from under `Wait`. Start subprocesses through
`childproc` (ADR 0087). What a stolen status looks like at the call site is
`waitid: no child processes` over complete output and a command that ran.

Do not turn a command's failure into a fact about the world — "get-url failed,
so there is no remote", "the check exited nonzero, so it is not there". Ask for
the value and read the answer, and prefer one write that both creates and
corrects over deciding between add and update.

## Serving a cache to a sandbox

A sandbox's namespace shares an object store with every other sandbox of its
remote, so `githttp`'s `FetchOnly` restrictions are the whole of what keeps it
from the rest, and they hold only together. Each has a bypass of its own: the
dumb protocol reads objects as files, a v2 upload-pack serves any object asked
for by id (the `allow*SHA1InWant` switches do not stop it), and a switch left
to the cache's own configuration can be loosened there. Do not forward
`Git-Protocol` on a sandbox's fetch, do not let it answer anything but the two
upload-pack requests, and keep the switches on the command line.
`githttp/namespace_test.go` fetches by id under every protocol for these
reasons.

- **`X-Discobox-Git-Cache` and `X-Discobox-Git-Cache-Caller` are the control
  plane's word.** They are read only on routes behind the control plane's
  token. A route a sandbox's own token reaches takes the cache from
  `source-caches.json`, never from a header.
- **Hold the cache for the whole request.** Everything that reads or writes a
  cache takes it through `gitcache.Store.Use` and releases it when the
  response is finished. A read outside it can meet the sweep's prune.
- **A sandbox's push needs both halves.** `receive.hideRefs` limits what it is
  told; the pre-receive hook, with `receive.unpackLimit=1`, limits what it can
  name. Unpacked, an object the store already holds is not written again, and
  the hook could not tell a push that sent it from one that only named it.
- **Mark a namespace before writing it** (`Receiving`, `markNamespace`). One
  with refs and no record is one the sweep cannot attribute, and so never
  removes.
- **A cache is never shallow.** Pull full history and refuse shallow pushes.
- **A pull pins only what its remote supplied in that request** (ADR 26-10-23-948).
  An unadvertised commit must be fetched into an empty repository first:
  fetching its ID into a cache that already holds it can succeed without
  asking the remote for it, leaking another sandbox's private cached object.
- An imported origin (`origins/<slug>.git` out of an archive) is untrusted
  down to its `config`. Never run git in it or fetch from its path: read its
  refs as files and lend its objects as an alternate
  (`gitcache.readImportedRepository`), and keep refusing links and an
  alternates file in it, which would lend the cache another repository's
  objects.
- Never bind state the pool owns or vouches for into a sandbox, not even
  read-only: the build area, a blob or bundle store, an agent layer or bundle
  tree. Sandbox containers are privileged, so the box's root can remount a
  read-only bind writable, and a shared tree it rewrote runs as root in every
  other box of the pool (ADR 0050). Copy what one box needs into that box
  instead. The pool cache and per-source data are the deliberate exception:
  shared on purpose and writable by every box (ADRs 0050, 0094), so nothing
  may trust what is in them.

## Error responses

A status this API reuses for several conditions needs a `type` to tell them apart.
409 is "already exists", "archived", and "no container", and the control plane
acts on each differently, so `mapRuntimeError` stamps the last two with
`model.ErrorTypeSandboxArchived` and `model.ErrorTypeSandboxNoContainer` and
`NewError` copies the type onto the response. A handler returns runtime errors
through `mapRuntimeError`, never bare: a bare sentinel reaches `NewError` with no
status and goes out as a 500. Do not let a caller's only signal be the detail
string: it is prose, it is written for people, and nothing stops it being
reworded.

The sandbox-directed routes have no problem document to carry a `type`: they
are proxied onward and refuse in plain text, which git and the CLI relay to a
person as it stands. Their condition travels in a header instead.
`requireRunning`'s 409 sets `model.SandboxNotRunningHeader` to the state it
found, and the control plane answers the reads it holds an inventory for from
that inventory when it sees it (ADR 26-10-11-657 §4). Do not let it match the
text there either.

## Running is not ready

Docker reports a container `running` the moment it starts; the sandbox agent in
it answers well after. A sandbox is usable only once `waitForSandboxAgent` has
seen `/healthz` (ADR 0017 §12, ADR 0039 tier 2), and every path that starts a
container marks the sandbox booting until then (`beginBoot`, before
`ContainerStart`). A status check that decides whether to proxy must not treat
`StatusRunning` as reachable while `SandboxBooting` says otherwise: a request
arriving mid-start reaches an agent that is not listening, and the caller gets
a bare 502. `EnsureSandboxRunning` waits the boot out; `requireRunning` refuses
with 409, or for the lists (`waitOutBoot`) waits it out through
`AwaitSandboxBoot`, which starts nothing, and then looks again. A new path that starts a container brackets it with `beginBoot` and
`finishBoot` too. The boot's wait for the agent runs detached from the request
that began it, so a caller going away cannot end the mark early; do not put it
back on the caller's context.

`EnsureSandboxRunning`'s already-running shortcut regressed this way once.
Do not answer it with the power lock: that lock is also held by create,
archive, delete, import and stop, so waiting on it (or failing a `TryLock`)
stalls healthy traffic behind unrelated work, and re-reading the container
after it adds a Docker round trip to every request.

Never take a machine as started on its power state alone. A machine is
placed into only while it runs, so one can run with nothing placed in it: its
start's placement failed, its caller went away, or the pool agent died
mid-start. Every start checks the pool's own record of what it placed in that
instance (`placementCurrent`) and places what is missing, and `startLocked`
stops a box whose start fails after it came up, detached from the caller.
Why: a machine answered as started on its state alone has no agent, and every
request to it is a 502. The machine runtime's `EnsureSandboxRunning` answers
without the power lock or a call to the driver, as the paragraph above asks,
only for a sandbox it last saw running, on no boot under way, whose
placement record is current (`answersRunning`): being seen running is a power
state, which any poll sets, and never enough alone.

Run a thaw's follow-up calls detached from the caller and under the power
lock. What `resumeLocked` does after `ContainerUnpause` — the runtime-config
delivery and the sandbox agent's `autostop/resumed` — is the thaw's equivalent
of a boot's wait, and the same rule holds: do not put it on the caller's
context. A caller that goes away mid-thaw must not leave the due wake in place:
the next sleep's confirmation would return it, and the server would thaw the
sandbox again at once (ADR 26-10-16-420 §4).

## What starts a sandbox is not decided here

A sandbox-directed route is wrapped in `onUse` and nothing else. Whether it
starts a stopped sandbox is the `use` claim on the request's token, which the
control plane sets where it classifies the route (ADR 26-10-12-711 §2). Do not
give a route its own start-or-refuse choice here: that is a second table, and
two tables drift. Do not read a token with no mark as leave to start one
either. And do
not wait here for a container that is not there: only the control plane knows
whether a rebuild is coming, and it is the tier that waits.

## A missing container is not a missing sandbox

Never destroy a sandbox's durable tree because no container carries its ID. A
failed rebuild, a settled failure waiting for its repair, and an archive all
look like that, and the control plane holds every one of them. Which sandboxes
exist is the control plane's to say: the volume reaper collects only trees
outside the set it answers (`WatchSandboxVolumes`, ADR 26-10-01-876), and an
unanswered ask is never an empty set. A new path that reclaims per-sandbox
durable state asks the same question.

## A sandbox's proxy record

- **Remove a sandbox's proxy record only under its power lock, after a look
  taken under the lock finds no container** (`reapSandboxMaterial`). The
  record — sentinels, routes, egress and gateways in the secrets file — is
  removed whole, and its loss fails open: a sandbox with no egress entry is
  unrestricted. A replacement has no container between the old one's removal
  and the new one's create, after the create registered the new record, so
  "no container" alone is not "orphan"; the material record's age does not
  help, because a re-stage never moves it (#314).
- **Every push that writes the record goes through
  `updateSandboxProxyPolicy`**: under the lock, and nothing for a sandbox with
  no container. An entry written after a delete has no material record naming
  it, and the reaper finds sandboxes only through those.

## A listener the proxy forwards to

- **A listener that takes `proxy.PoolClientHeader` as its only word on who is
  asking is on a socket, never a loopback port.** The proxy strips and sets
  the header only on the pool-host path; it sends a sandbox's ordinary egress
  from the pool's network namespace, so only its refusal of its own loopback
  (ADR 26-10-29-160 §3) keeps a box from reaching a loopback port with a
  header of its own and being answered as another sandbox. That is one check;
  a socket no egress can name is the boundary. The asset cache
  (`layout.ProxyAssetsSocket`, #265's review) and the registry front
  (`layout.ProxyRegistrySocket`) are on sockets for this reason. The origins
  listener may stay on loopback only because it also requires the pool's
  sandbox token for the sandbox it names.
- **Nothing keyed on a loopback source trusts it, or caps it as no caller.**
  For the same reason, 127.0.0.1 is any sandbox's own egress: a limit or a
  trust keyed by source address must treat it as one untrusted caller. The
  tunnel listener splices from an address of its own (`TunnelSource`), the one
  source `dnsforward` exempts from its per-source cap, and bounds the tunnels
  it holds itself. Exempting all of loopback was proposed in ADR 26-10-23-719's
  first draft and would have let one box exhaust the pool's DNS.
- **The pool cache and the server's asset store read a bundle the same way.**
  Manifest parsing, a pin's reach and the damaged-blob rule live in
  `imagecache`; a change to one store's reading belongs there, not in a copy.

## The pool registry

- **Every registry reference a solve names is bound to the caller's
  namespace** (`bindToNamespace`, `bindGatewaySolve`): a new place a solve can
  name an image or a cache — an exporter, a cache entry, a frontend attribute
  — goes through `bindRef` or the source policy. A sandbox ID is not a secret,
  so one reference left unbound is a write into, or read of, a peer's
  namespace (#380's review).
- **Binding by name is enough only while the registry admits the pool
  builder's certificate alone, and buildkitd presents it for `RegistryHost`
  alone** (`registrytls.go`). That is what makes every other name for the
  registry's address reach nothing, so `bindRef` can match the one address
  exactly. Giving buildkitd's keypair a second host, declaring the registry
  `http`/`insecure` again, dropping its client CAs, or signing a second client
  from its CA each reopens cross-sandbox reads and writes without touching the
  binding.
- **Every runtime's `DeleteSandbox` runs every shared purge step.** The
  sandbox's policy, its proxy material, its registry namespace
  (`sandboxCore.removePublishedImages`) and its tree are each removed by both
  the Docker and the machine runtime; a step added for one is added to the
  other. The machine runtime once skipped the registry namespace the Docker
  runtime removed, though both pools build.

## Snapshots

- **Nothing of a sandbox reaches a snapshot build.** A build's plan, egress
  and identity come from the create's plan alone (`snapshotPlan`): the
  sandbox's own egress level, secrets, sentinels, gateways, sources and
  account stay out, and `agentlayer.SnapshotPlan.Validate` refuses a plan
  that carries them. A new input to a build that differs between sandboxes of
  one plan either goes into the key or stays out of the build.
- **A build container has no labels of its own.** A container inherits its
  image's labels, so anything put on a build reaches every sandbox started
  from its snapshot. It carries the project and pool labels every container
  of the pool carries, and is found by its name with them.
- **A container the pool starts that is no sandbox's still enters the start
  gate.** A snapshot build starts a box and fetches through the caches a clear
  empties, so it holds the gate for its whole length, and a clear cancels it
  only once the gate is shut (`cancelSnapshotBuilds`, passed to
  `startGate.close`): cancelled before, a build asked for in between enters
  the open gate and holds the clear's drain for up to a whole build. A new
  kind of container the runtime starts does the same, or a clear runs under
  it.
- **A snapshot build's proxy record goes only with its container.** The
  proxy reads an identity with no egress entry as unrestricted, and a build
  outlives an agent that restarted under it, so the material reaper skips
  `layout.IsSnapshotID` identities, and a build is removed in one order
  (`removeSnapshotBuild`): its container, then its policy and material, then
  its tree, each only once what goes before it is gone — the tree last,
  since it is what the next sweep finds the build by to try again.
- **A build's box is described by `boxConfig`, as a sandbox's is.** A host or
  network setting added to one and not the other makes a snapshot a box of
  another kind than the ones started from it.

## Records the pool keeps

- **A type serialized into a pool-side record is a persisted schema.** The
  records beside a sandbox's tree (`delivery.json`, `agent-placed.json`) and
  a snapshot's plan outlive the agent that wrote them: a host's state
  survives the pool container being recreated. Changing the shape of what
  they hold — `agentlayer.Pins` became a list — or their file name needs a
  reader for the old one (`Pins.UnmarshalJSON`, `readLegacyRecord`), and a
  test with a record the previous release wrote. A record that no longer
  decodes fails every create of its sandbox until someone deletes it.

## The pool bundle's features

The pool bundle's units (`bundle/features/*/systemd`) run what its features
link from the bundle's stable tree. A unit that reads a file the agent renders
`Requires=` and is `After=` the agent's Type=notify unit: systemd starts it no
sooner than the agent has written it.

A feature's binary moves under its stable link without any step's definition
moving, so nothing would restart what runs it: each feature's `restart` step
names its binaries as `inputs`, and a new bundle restarts exactly what it
changed. A step added that lays a binary needs it there too.

- Bound incomplete Git stderr lines as well as completed error messages. A
  forge controls sideband text; a line without a newline must not grow the
  trusted pool process's memory without limit (`pkg/gitprogress.Writer`).

- When classifying Git HTTP authentication from a trace, keep the trace on a
  bounded, drained pipe with data disabled and redaction enabled. Never send
  it to stderr or a file: a lent request may carry real credentials.

## Removing a machine

A machine's disk is its sandbox's only copy, so nothing removes one on a single
look. The volume reaper removes a sandbox's machine with its unheld tree, after
the trees' retention. An orphan, a machine of the pool's project that no tree
and no held sandbox names, is removed only if its name is a sandbox's
(`sbx-…`), and only once the same retention has passed since it was first seen
so (`machine-orphans`). A pool's project is the provider's naming convention,
not a fence (ADR 26-10-23-551 §4), so a machine of any other name in it is
never this runtime's to remove.

## The API on a public URL

- The agent's API can be on a public URL (a boxd pool machine's, ADR
  26-10-09-106 §3). Only `/healthz`, `/readyz` and `/metadata` answer without
  a token, and they change nothing and say nothing secret — and `/tunnel/*`
  on a machine pool reached only through an edge (`Config.Tunnels`, ADR
  26-10-29-481 §2), which is spliced onto the proxy unit's tunnel listener and
  names no caller until the mTLS inside a stream completes at its service. A
  tunnel path that reaches anything not behind that mTLS is a finding; every operation of
  the generated API has a scope in `requiredPoolOperationScope`, and an
  operation added without one is reachable with any token minted for the
  pool. A request proxied into a sandbox never carries the token that
  authorized the hop (`sandboxProxy` drops `Authorization` but the sandbox
  agent's own). Why: code in a sandbox could otherwise replay the control
  plane's token at the pool's URL.
- Nothing of a pool's builder is conditioned on its runtime: the agent
  prepares it, and the mediator serves it, on a machine pool as on a Docker
  pool. Why: the two provide the same functionality (ADR 26-10-27-767), and
  a builder unit whose configuration the agent did not write fails `apply`'s
  start of it, so a machine pool would never finish provisioning. On a
  machine pool a build runs beside the provisioning key and the driver's
  machine token, which reach the provider's org: a change that widens what a
  build can reach on the pool's host (its mounts, its network, the runc
  wrapper) is judged against that. Nothing on the builder's path may assume
  a Docker pool's network either — the pool's name resolves on a pool
  machine only through `ensurePoolName`, which this path has missed once
  already — and that name is the hosts file's, never a resolver's: a build
  step's forwarder dials the pool proxy by it with the owning sandbox's
  client certificate, so whoever answers it is handed that sandbox's egress.
  The registry is not named that way: buildkitd pushes to it on loopback.

## The sandbox agent's TLS

- How the pool reaches a sandbox's agent is what that sandbox's bootstrap told
  it, recorded when it was written (`agent-tls.json`, ADR 26-10-29-481 §3),
  never what the agent says of itself and never the pool's current setting.
  Why: every path that delivers a runtime-config document — a boot, a resume,
  settling sources — runs before any status poll, and the first document
  carries the sandbox's client key; learning TLS from the agent once sent that
  document in plain HTTP to whatever answered first.
- What a runtime-config document holds that is private is taken out by
  `sandboxconfig.RuntimeConfig.WithoutPrivateKeys`, on both sides. Why: the
  pool and the sandbox once each had a hand-written copy, and a key added to
  the document and missed in one copy is a key kept on a data volume an export
  carries, or answered to a caller.
- A keypair the agent serves is placed at every start, not only when its box
  is made. Why: a certificate that expired while its sandbox was down, with no
  start placing a renewed one, locks the pool out of that sandbox for good.

## The control-plane channel

- The channel is a standard binary WebSocket, never an upgrade to a protocol
  of its own, and keeps its pinned TLS inside. Why: a hosted edge forwards
  WebSockets and need not forward anything else — a custom upgrade answered
  and then carried nothing past boxd's edge — and the edge's TLS is not end
  to end (ADR 26-10-31-152). A change to the handshake is tested through a
  proxy that refuses other upgrades
  (`TestTheChannelOpensThroughAnEdgeThatForwardsOnlyWebSockets`), not only
  against the agent directly.
- The channel's WebSocket is asked for over HTTP/1.1 alone (`cpchannel.http1`),
  with ALPN offering only `http/1.1`. Why: a WebSocket opens with an HTTP/1.1
  Upgrade, so through any HTTPS front that offers h2 a transport that
  negotiated it would never open the channel, and a test against plain
  `httptest` would not notice (`TestTheChannelOpensThroughAnHTTP2Front`).
