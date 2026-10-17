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

The pool's only write into a sandbox's config volume is the bootstrap, before
its container exists. Everything after that — secrets, proxy material, the idle
timeout, readiness — is a change to the runtime-config record
(`decideRuntimeConfig`) and a delivery, never a file. And nothing the pool
decides is read back from the sandbox's volumes, which the sandbox can write:
keep a record of your own beside the tree (`runtime-config.json`,
`project-layer.json`) instead (ADR 26-10-08-127).

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
- **A cache is never shallow, and never fetches.** Do not add a path that runs
  `git fetch` against a remote from the pool, or one that accepts a shallow
  push.
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
