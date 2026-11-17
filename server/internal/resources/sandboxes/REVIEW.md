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
- **A wake is spent only after its pool answered the start, never before the
  send.** Clear `WakeAt` with `SpendSandboxWake` once the pool accepted or
  refused the start; `ErrPoolNotReachable`, a canceled context, every
  failure before the send (`errInstructionNotSent`) and a fence refusal
  (`ErrStartNotAuthorized`) keep it (`startNotTaken`). A fence refusal looks
  like the pool's answer, but the pool agent folds a failed authorization
  callback into it, and a wake really superseded was cleared when its epoch
  moved. A claim before the send loses the wake to
  any failure between the two, a crash included, and a pool is unreachable for
  a while after every server restart, which is when wakes that fell due while
  the server was down are sent (#1192, ADR 26-11-15-829 §1).
- **A confirmed power-down writes `WakeAt` whatever it carried, none
  included.** The confirmation reads the sandbox's wake files at that moment;
  a row kept from before it can name a wake a boot already handed over, and
  would start the sandbox again for it (ADR 26-10-16-337 §§5, 7).
- **The wake's start is always fenced** (`ref.StartEpoch` set to the
  `WakeEpoch` read with the wake). It is sent again until the pool takes it,
  and only the fence stops a lost attempt from running after a person's stop
  (ADR 26-11-15-829 §3).
- **A wake is written and spent only on the `WakeEpoch` the reconcile read**,
  and every outside power instruction moves it, wake or no wake. A write that
  skips the guard lets a person's stop be undone by the wake it raced. Suspend
  is the one instruction that moves it after it is sent, and only when the
  pool accepted it: a refused suspend changes nothing (ADR 26-10-16-420 §3),
  and the order is still safe because a reconcile that reads the epoch once
  the sandbox is frozen finds it not running and is refused. Do not move it
  back before the send.

## Settling a failure

- **A teardown settles only on the pool's answer.** Archive and a repair's
  teardown record `failed` and advance `ObservedGeneration` only for
  something the pool said; nothing re-drives a settled failure (ADR 0017
  §§3–4). `ErrPoolNotReachable`, an agent older than the server
  (`ErrPoolAgentUnsupported`) or an ended context is no answer: return the
  error unsettled so the engine retries (`teardownNotAnswered`). A pool is
  unreachable after every server restart, and an archive settled there stayed
  `error` for good (#1043). Create is not a teardown: it already waits up to
  `poolAgentWaitCeiling` for its host and then settles, by design
  (`server/providers/DESIGN.md`); it waits on the older agent only.
- **A delete never settles, the pool's own refusal included** (ADR 0022 §3).
  It returns every removal failure unsettled and records nothing, so the box
  reads `deleting` while the engine retries. Settled, a purge on a pool whose
  host was away kept its data while its 409 promised a retry, and a
  retention's purge has no one to purge again (#928). Do not record the cause
  as `ErrorMessage` on an unsettled teardown either: `DisplayState` reads any
  `ErrorMessage` as `error`. The scan re-drives every row asking for
  deletion, so a purge settled before that fix is not stranded.

## Marks a pool observation earns

- **A delivery a pool's report earns is marked as drift (`MarkDirtyDriftTx`),
  never intent, in the transaction that records the report.** A state report
  of a sandbox coming up (`cameUp`) re-delivers its proxy record, and it
  arrives at every start: an intent mark from it would cancel the backoff of a
  delivery that keeps failing each time the sandbox starts, and a mark made
  after the commit is lost for good, since the delivery reconcilers have no
  scan to re-find it. This is the second time an observation was marked as
  intent (power-down, above).
- **A capability a pool declares on its reports, which changes what the
  server sends its sandboxes, re-delivers to them when it flips.** Nothing
  else about them changed, so nothing else marks them: an agent upgraded in
  place over running containers is otherwise sent the old shape until some
  unrelated change happens to push. `Pool.SentinelGrants` (ADR 26-11-03-329
  §3) marks every present sandbox's sentinel delivery, as drift, in the
  transaction that records the report (`MarkPoolSentinelsTx`); the next
  field of this kind does the same.
- **The one intent mark a pool observation earns is its turn to active and
  ready, for its due wakes** (`MarkPoolDueWakesTx`, ADR 26-11-15-829 §2): it
  must cut a failing wake's backoff, which drift cannot. It is safe only
  because it fires on the transition, computed from the pool as its writer
  found it; marked on every ready report it would be the retry at every poll
  the drift rule above exists to prevent. Every write that can complete that
  turn hands it over, the pool reconcile's settle as well as the status
  report: a pool back from `offline` turns `active` in the reconcile, after
  the report, and a check on reports alone never sees it.

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
- Record nothing but `deleted` over a sandbox whose desired state is
  `deleted`. A purge or a retention expiry that failed is still retrying in
  the background, and any intent recorded over it — an archive, an upgrade's
  or a source push's `present` — cancels the purge. Discoboxes reach those
  routes for the boxes they created (ADR 26-11-03-724), so the delete undone
  can be a person's. `recordSandboxIntent` refuses it (409) reading the row
  under its write, and `DeleteSandbox` takes the refusal as done; a write
  that records intent on its own (`UpdateSandbox`'s harness change, the
  automatic repair) does so only over a row it read as `present`, under that
  read's generation. A check in the caller before either races a purge
  recorded after its read.
- **The purge's 409 text is matched by `test/ux-smoke`** (`purge_box`), which
  waits for the background purge only on `sandbox purge did not complete; it
  will be retried in the background`. Change it there in the same change, or
  keep that text as the message's start (`purgePending`).
- Read a program's reported status off `agent_status` only for a running
  sandbox. A stopped sandbox's row keeps the last report its agent posted and
  is never rewritten, so a msg or state read from it describes nothing that is
  happening; the display name drops the msg when the box is not running, as
  the CLI's `sandboxProgramStatus` drops the whole status.
- A sandbox-role route that reaches a whole update body (the sandbox `PATCH`,
  for passthrough, ADR 26-10-31-819 §5) checks the fields it may set by
  listing them, read off the body's own encoding (`onlyFields`), never by
  refusing the fields known today. Otherwise the next field added to the
  update is writable by every discobox on the boxes it created, and no test
  fails.

## The event stream

- **A stream's tests resume from an ID the stream sent**, never one picked by
  hand. A replay's events are old, so the IDs a client is given can sit below
  the watermark; a test that resumed from the head instead hid that every
  reconnect to a quiet project reset and reprinted every discobox
  (ADR 26-10-29-316 §4). Resume from `Cursor()`, or from the last ID written.
- **`pushSandboxSentinels` sends every field it owns non-nil, even
  empty** (`Sentinels`, `SecretEnv`). A nil field in `UpdateOptions` means
  "leave the pool's copy as it is", so the last sentinel removed, or turned
  raw, stayed in the sandbox; a variable turned raw kept its old sentinel,
  which is the pool's handle for the value and shadowed an `env` delivery of
  it (ADR 26-10-27-840 §3).

## A sandbox owed its delivery

- **Every path that has the pool run a sandbox checks `awaitingSourcePush`
  first, parked included** — a `sandboxStart` or `sandboxRestart`
  instruction, `EnsureRunning`, and a repair's start. A sandbox owed its
  client's delivery has no source in its workspace, and only that delivery
  may start it (ADR 26-11-03-618 §2). Twice a path got this wrong: repair
  re-parked such a sandbox and started it anyway (#70), and a start
  instruction let a parked one through on the theory that a wait held it,
  which only an action has.

- Keep access-revocation notifications separate from egress notifications.
  File downloads remain authorized after an egress-only edit, while commands
  and writes must lose authority on a coverage change. A single combined
  version once canceled reads even though the policy explicitly permits them.
- Revalidate deletion as well as API access and parenthood immediately before
  forwarding a sandbox-authenticated lease. A caller may be deleted while a
  waiting acquire is preparing its target; registering only after that deletion
  must not turn the completed revocation into a fresh authorization baseline.

- When first registration ends an initial-bootstrap wait, reset the ordinary
  readiness stall clock once. A silent bounded apply can outlast its old clock
  without changing display telemetry; registration need not report ready yet.
