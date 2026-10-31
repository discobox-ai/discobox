# Sandboxes Review Notes

## What a created discobox inherits

- **What a child inherits from its harness config is part of what its creator
  gives it.** A discobox with `DiscoboxAPI` may create one on any harness config
  in the project, and the child runs with that config's credentials (ADR
  26-10-27-880 §9). A change that lets a create pick up more from a harness
  config widens what every `DiscoboxAPI` discobox may hand on; weigh it as such.

## Values the client hands to host tools

- **A stored path the client hands to a host tool is checked where it is
  stored and where it is used.** A source's paths become the folder an
  editor's `ssh-remote+<host><folder>` URL opens and `{workdir}` in a host
  tool's command, where anything not absolute can name a host instead. The
  client refuses such a folder for every source and record
  (`sandboxSSHFolder`); the server refuses a working directory outside the
  source's directory on create and import (`validateSourceWorkingDirectory`,
  ADR 26-10-15-417). A box can create boxes and an archive is anyone's, so no
  request is trusted to be the client's own.
- **A consumer's check is not the field's validation.** The working directory
  was only ever refused by the pool, because Docker refused it as a
  `WorkingDir`. Removing the consumer removed the check and left a stored value
  unchecked (#234). Before deleting what reads a stored value, find what else
  the deletion stops refusing.
- **Check a client-produced path with `Clean`, never `Rooted`.** `Rooted` is
  the pool's placement reading: it refuses whitespace, so as a validity test it
  refuses every repository whose path has a space in it, which the client
  mirrors as is.

## Power on a sandbox's own request

- **What a status asks for is marked as drift (`MarkDirtyDrift`), never with
  intent.** Status arrives every poll; an intent mark from it would cancel the
  backoff of a power-down that keeps failing and retry it every 15 seconds
  for every idle sandbox on the pool (ADR 26-10-16-337 §5).
- **The stored runtime state lags an instruction this same reconcile just
  sent.** A power-down returns before the pool reports `stopped`, so a
  decision made on `RuntimeState` right after one reads the sandbox as still
  running. The wake is spent on a start whatever the row says, because a start
  on a running sandbox is a no-op and one skipped on a stopping sandbox loses
  the wake (ADR 26-10-16-337 §6).
- **A wake is written and claimed only on the `WakeEpoch` the reconcile read**,
  and every outside power instruction moves it, wake or no wake. A write that
  skips the guard lets a person's stop be undone by the wake it raced. Suspend
  is the one instruction that moves it after it is sent, and only when the
  pool accepted it: a refused suspend changes nothing (ADR 26-10-16-420 §3),
  and the order is still safe because a reconcile that reads the epoch once
  the sandbox is frozen finds it not running and is refused. Do not move it
  back before the send.

## Marks a pool observation earns

- **A delivery a pool's report earns is marked as drift (`MarkDirtyDriftTx`),
  never intent, in the transaction that records the report.** A state report
  of a sandbox coming up (`cameUp`) re-delivers its proxy record, and it
  arrives at every start: an intent mark from it would cancel the backoff of a
  delivery that keeps failing each time the sandbox starts, and a mark made
  after the commit is lost for good, since the delivery reconcilers have no
  scan to re-find it. This is the second time an observation was marked as
  intent (power-down, above).

## The spec fingerprint

- **Feed the fingerprint only what the sandbox itself pinned, never a shared
  row read at reconcile.** A harness config is every sandbox's on it, and is
  re-seeded whenever its bundle moves: an input read from it changes the
  fingerprint of every sandbox at once, and the pool rebuilds and restarts
  them all with no upgrade recorded, even under a manual upgrade policy (ADR
  0016, 0021 §2). A first cut of the delivered fingerprint hashed the live
  config; it is `SandboxManifest.DeliveredFingerprint` now, over the manifest
  alone.
- **A field added to `SandboxManifest` is judged for `DeliveredFingerprint`
  too.** It leaves out the pins a delivered box applies at its start (image,
  platform) and covers everything else; another such pin belongs in its
  list, and anything a container is built from must not.

## Harness changes

- Clear the previous initial prompt when selecting another harness: retaining
  `SandboxManifest.Prompt` replays it through the revived terminal's boot prompt
  into the new agent. Terminal identity survives; a conversation is not migrated.
- Keep pending target references protected from deletion, and publish an archive
  marker before clearing a pool fence. Either gap can strand an accepted switch
  or let an old runtime start after a failed archive.
- Read a program's reported status off `agent_status` only for a running
  sandbox. A stopped sandbox's row keeps the last report its agent posted and
  is never rewritten, so a msg or state read from it describes nothing that is
  happening; the display name drops the msg when the box is not running, as
  the CLI's `sandboxProgramStatus` drops the whole status.
