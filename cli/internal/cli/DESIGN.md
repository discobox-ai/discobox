
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
no saveable changes. Save-back is the only way a harness's settings and
logins reach its config (ADR 26-11-02-152).

Harness update requests carry the revision read before editing. The editor keeps
that revision while open and does not reload it at save time; a concurrent
save-back or configuration edit returns a conflict instead of replacing a newer
file bucket. The ordinary update command reads config and egress together once.

Harness save-back overwrites the candidate's saved paths and captured credentials
using current locked rows, even after a concurrent edit or renewal (ADR
26-11-04-538). The CLI and console submit the displayed candidate once, without
a force flag or second overwrite confirmation. Identity, manifest and candidate
validation still apply; unrelated fields and personal overrides remain intact.

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

## Credential policy files

`secret bind` and `admin binding create/update` accept repeatable `--policy FILE`
flags. Each following `--policy-arg NAME=VALUE` belongs to the preceding file.
The client resolves files through `pkg/credentialpolicy` before mutation,
then submits one union of concrete routes. `--route` and `--clear-route` cannot
be combined with policies. Updating a binding reapplies a snapshot explicitly;
there is no live file reference. `run --bind` uses the same resolver through
`internal/sandboxcreate` (ADR 26-10-30-584).

## Credential requests

A request has a use part, a delegate part, or both (ADR 26-11-02-022).
`secret request get` shows each part, its routes, and its roles
(`requestrole`): name, description marked as the discobox's words, arguments,
and beside each what its routes grant. `ls` keeps one compact PARTS column.

`secret request approve` reads the request first, since which parts it has
decides what the answer must say. An answer names a decision for every part
(`approvalBody`): a request with one part is approved by the command; one with
both needs `--use approve|deny` and `--delegate approve|deny` and is refused,
naming the missing flag, before anything is sent. `--route` narrows the use
part and `--delegate-route` the delegate part; `--for` applies to every part
approved; `--raw` is the use part's alone. The answer is reported part by
part, a part approved with routes other than its roles resolved to saying the
roles no longer describe it. `deny` denies the whole request.

What a part replaces (ADR 26-11-02-022 §5) is `pkg/bindingplan`'s rule, the
one the server applies, read over the asking discobox's own sandbox-level
bindings of the secret (`requestReplacements`): `get --secret` shows it for a
pending request, and `approve --secret` reads it before sending and says it
after, part by part — the binding kept, the ones that go, one that loses its
use or is split, a new one made.

A use part may be granted as an addition beside what the discobox holds
instead (ADR 26-11-03-329 §4): `get` says how it asks to be granted, and on a
pending request what applies in its variable now (`requestUseGrants`, from
the resolved bound secrets), grant by grant with its lifetime, the routes
asked that none of them names — compared as `secretroute.Format` writes them,
and said to be — and what each way would do, naming any grant a replacement
would cut short, shadow or remove (`writeUseGrants`). `approve --use-as
add|replace` chooses the way, sent only when given so the server grants it as
the request asked; it is refused on a use part denied or absent.

## Adoption

`adopt DISCOBOX --reason` files a sandbox-role adoption request and polls its
answer until approved, denied or canceled. `release DISCOBOX` gives a child back.
Both resolve by the same selection as other top-level sandbox commands.
`ls --parent ID` filters by parent; `--parent me` reads this box's static config.
Tables show PARENT; JSON preserves the creator beside it.
