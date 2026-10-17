# Sandboxes Review Notes

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
