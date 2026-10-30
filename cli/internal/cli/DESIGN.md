
## Harness changes

`admin harness changes SANDBOX` previews the immutable candidate the sandbox captured.
Only the server's validated portable settings and credential identities reach
this command. Each changed file prints as `add`, `update`, `remove` or `reset`
(a removal that falls back to the harness's own file) and its unified diff
hunks from the preview's `previous` to its `file` — what the server says a
save replaces and leaves — so a reviewer reads what changes, not the whole
result; the console's review card draws the same hunks (`harnessChangeDiff`).
A side that is JSON is re-indented as the exporters write it (`diffText`)
before the comparison: `previous` is stored as it shipped, often on one line,
and `file` is exporter output, so byte for byte every line would differ.
Past `harnessDiffLines` the file is shown replaced whole, so a box cannot make
the comparison cost the client unbounded memory.
`admin harness save SANDBOX CANDIDATE` commits that exact candidate;
repeating it retries acknowledgment. `admin harness reconsider` requires explicit
`--keep-file` and `--replace-credential` selections before creating a new
candidate against current revisions. `admin harness discard` leaves local files alone.
The console reports a pending candidate from agent status; `H` opens its review
card with explicit save and discard actions. CLI previews print the commands for
the exact displayed candidate and identify diagnostic-only candidates as having
no saveable changes. The configure commands
remain available; save-back does not depend on a configure session.

Harness update requests carry the revision read before editing. The editor keeps
that revision while open and does not reload it at save time; a concurrent
save-back or configuration edit returns a conflict instead of replacing a newer
file bucket. The ordinary update command reads config and egress together once.

Harness save-back accepts an explicit force option to bypass config and secret
revision conflicts for the reviewed candidate. Identity, manifest and candidate
validation still apply; unrelated files remain intact. The console offers an
overwrite confirmation after a save conflict and retries the displayed candidate
with force only after confirmation. CLI: `admin harness save --force`.

## WebSocket audit recordings

The existing HTTP audit stream action renders `discobox-websocket-messages-v1`
through `pkg/wsrecord`: timestamp, sent/received direction, type and decoded
payload, with binary and control payloads in hex. Text is escaped for the
terminal; application JSON is kept in its original layout to bound expansion.
The console caps rendered output like other body recordings. A terminal's
`admin audit http --body … --part stream` renders the same messages; piped
output preserves the binary artifact for saving. Legacy raw upgrade spools
remain readable as recorded bytes. Capture diagnostics and partial final
records are displayed explicitly.
