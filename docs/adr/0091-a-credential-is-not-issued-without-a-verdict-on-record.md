# 0091 — A credential is not issued without a verdict on record

- **Status**: Accepted
- **Date**: 2026-09-03

## Context

[ADR 0079](0079-a-local-judge-gates-every-wrapped-credential-use.md) made a model
the gate in front of every wrapped credential use.
[ADR 0090](0090-the-judge-is-handed-facts-and-given-no-tools.md) bounded what
that model is told and denied it the ability to go looking, and deferred the
other half: nothing records what the judge decided.

`judgeCommand` returns `nil` or an error. The verdict, its reason, the prompt it
answered and the model that answered it are all discarded when the function
returns. What survives is incidental: `discobox-prompt` on claude-code is
`claude --print`, which writes a session transcript under
`~/.claude/projects/<cwd-slug>/<uuid>.jsonl`. Four such files were the only
evidence of four real verdicts. They are the wrong record in three ways — they
are mode-0600 files in the judged agent's own home directory, so the thing being
gated can rewrite or delete the record of its gating; the harness prunes them on
its own schedule (`~/.claude/.last-cleanup`); and their `thinking` blocks come
back as `""` with a signature, so a verdict that cost 5,903 thinking tokens
preserves the conclusion and none of the argument.

ADR 0031 §"consequences" claims "the declared command plus activation records
give the audit log a per-use story". The activation records are
`pool-agent/proxyagent/activations.go`: in-memory, pool-local, 5-minute TTL,
"deliberately not control-plane state". They are a live authorization registry,
not a trail. ADR 0079's own Context notes the declared command "is currently
only written to an audit log nobody reads" — the judge was added to *read* it at
decision time, not to leave anything behind.

Three facts about the existing plumbing decide the design.

**The pool agent already receives the argv on trusted ground.** The call that
takes a value is `POST /v1/credentials/use` (`agentcreds.PathUse`, served in
`agentcreds/server.go`) — a POST, not an HTTP GET, though the broker method that
serves it is spelled `Get`, and the CLI had a subcommand by that name too before
[ADR 0092](0092-the-cli-has-no-unjudged-way-to-take-a-value.md) removed it. Its
body, `agentcreds.UseBody{UseID, Command}`, already carries the declared argv,
and `credentialBroker.Get` mints the activation from it. The command crosses the
boundary today; only the verdict does not. **`/v1/credentials/use` is what the
rest of this ADR means by "the use call".**

**The pool agent has no database.** It holds activations in memory and talks to
the control plane over HTTP. It is not a place to keep anything.

**The sandbox-agent database cannot answer "grab these later", and is not
protected.** It is real storage — SQLite through `gormdb` at
`/var/lib/discobox/sandbox-agent.db`, with GORM models, `AutoMigrate` and a
retention policy. Its file mode (`root:root 0644`, agent unprivileged at uid
1002) looks like a boundary and is not one: the agent has sudo, so root in the
sandbox is a command away and every byte of that database is rewritable by the
thing it would be recording. Two more things disqualify it: it lives and dies
with its sandbox, and the control plane only *proxies* reads of its records
(`sandbox_agent_terminals_proxy.go`) rather than copying them, so a deleted
sandbox takes its trail with it. An audit record that a sandbox deletion erases,
or that its subject can edit, is not an audit record.

## Decision

**The verdict is a required field of `POST /v1/credentials/use`, and the control
plane persists it. No credential value is issued without an audit row on trusted
ground.**

### 1. The verdict rides the request that takes the value

`agentcreds.UseBody` gains the verdict alongside the command it already
carries: `allow`, `reason`, the role that answered (`judge` today), the prompt
the judge was given, and how long it took. Not a vendor model id — ADR 0079 §2
keeps that from this CLI on purpose ("`--model judge` names a role, not a
vendor's model id... The CLI never learns a model name"), and a verdict schema
of `{allow, reason}` gives no channel to smuggle one back even if a wrapper
wanted to. The role is what the CLI legitimately has. The handler rejects a
body without a verdict the way it rejects one without a `useId`.

This is the property worth having, and it is worth more than the record itself:
**a credential cannot be issued without an accompanying verdict**, because the
call that issues it is the call that carries the verdict. Not "the CLI is
expected to report" — there is no path to a value that skips it. ADR 0079 §1
ordered judging before taking so a refusal mints nothing; this makes the
converse true, that a mint always has a judgment attached to it.

### 2. The record is the control plane's, in the database it already has

`server/internal/store` gets a model and a table, migrated with the rest
(`internal/database`'s `DB.Migrate`). The rows join to the grant, the request
and the sandbox that are already there, which is what makes the trail answer
questions worth asking — every use of one grant, every verdict for one sandbox,
every denial across a project.

**No new database.** A dedicated one would need its own migrations, its own
backup story and its own lifecycle, and would keep the audit rows in a place
where they cannot be joined to the grant they are about. The control plane
database is one schema per server process by design (`internal/database`
DESIGN.md) and this belongs in it.

Volume does not argue otherwise: a row is a prompt and a sentence, on the order
of a kilobyte, once per credential use.

### 3. Denials are reported best-effort, and marked as such

A denial never reaches the use call — that is ADR 0079 §1's ordering, and it is
why refusals are free. So denials arrive by an explicit report call that the CLI
makes and nothing forces, and they are stored flagged as volunteered by the
sandbox rather than observed at issue time.

That asymmetry is honest and acceptable. A denial issued no credential, so the
security-relevant trail is complete without it; what unreported denials cost is
*signal* — an agent probing the gate repeatedly is exactly what someone would
want to see — and best-effort collection gets that from every agent that is not
actively hiding, which is the population the judge is for at all (ADR 0079: it
catches the honest failure).

### 4. What is stored, and what cannot be

Stored: the approved use sentence, the credential name, env var and host, the
argv, the facts block from ADR 0090 §2, `allow`, `reason`, the role that
answered, the verdict's latency, and the sandbox, grant and use it belongs to.

Never stored: the credential value. Not by policy but by construction — ADR 0079
§1 judges before the value is taken, so at verdict time no value exists. The
audit trail inherits that.

The prompt is stored in full rather than digested. The four transcripts were
useful precisely because the exact text could be re-read; a hash answers "was it
this" and never "what was it".

## Alternatives rejected

**Record into the sandbox-agent database.** Rejected on both counts. As storage
it is exactly right — structured, efficient, migrated, and with the precedent
already built: harness hooks reach it from inside the sandbox over a unix socket
at `/run/discobox/harness-hooks/hooks.sock` and land in `harness_hook_logs`. But
it dies with its sandbox and the control plane never copies it, so "grab these
records later" fails for exactly the sandboxes most worth asking about, the ones
already torn down. And it is inside the sandbox: the agent has sudo, so it can
rewrite or drop any row, and the socket is `0666`, so any process there can
forge one. A record its subject can edit answers nothing that is worth
asking.

**Have the control plane pull the records from sandbox-agent instead of being
told.** Rejected: it inverts the lifetime problem rather than solving it. The
puller has to win a race against sandbox deletion for every sandbox, forever,
and a sandbox that is stopped, unreachable, or deleted between two polls loses
its rows. Pushing the verdict on the call that mints the value has no race —
there is no window in which the value exists and the record does not.

**Report the verdict on a separate call and leave the use call alone.** Rejected: it
makes the audit row optional in practice. A separate call is one an agent can
skip, and skipping it is invisible, so the trail would be complete exactly for
agents that were not the reason to have it. Folding it into the use call is what turns
"we ask agents to report" into "a value is not issued without a record".

**Keep the harness transcripts and just stop pruning them.** Rejected: they are
in the judged agent's home directory, at `0600`, owned by it. Retention is not
the problem; custody is. They are also per-harness — codex-cli leaves a
different shape, and `shell` leaves nothing.

**Store a digest of the prompt rather than the prompt.** Rejected: see §4. The
whole value of a record here is being able to read what the judge was actually
asked.

## Consequences

- `agentcreds.UseBody` grows required fields, so the protocol version moves and
  both sides change together. `agentcred`'s rule of depending on nothing but
  `agentcreds` is unaffected.
- A use call from an older `discobox-access` — one baked into a sandbox image
  built before this lands — is rejected for want of a verdict. That is a
  fail-closed break rather than a silent unjudged path, which is the direction
  ADR 0079 chose for every other non-yes, but it means image and control plane
  roll forward together.
- The control plane stores text written inside a sandbox — the argv, the facts
  block, and a model's reason. It is display data in an audit view and must never
  be read as instruction, in the control plane or by anything reading the trail
  later. This is the same rule ADR 0090 §3 applies to the judge's own prompt.
- Denials are undercounted by exactly the agents that chose not to report them,
  and the schema says so rather than presenting the count as complete.
- The sandbox-agent database stays out of this. Nothing here changes it, and
  ADR 0090's `--no-tools` work does not depend on it.
- The trail makes ADR 0079 §5's trusted-side judge cheaper to build, not harder:
  the verdict field and its record are the shape that judge would report anyway,
  and by then the use call already has the argv, the facts and a place to write.
- **Nothing in the sandbox is a place to keep a record about the sandbox.** The
  sudo finding above generalises: `/var/lib/discobox/sandbox-agent.db` holds exec
  transcripts, harness hook logs and resource samples that are also evidence, and
  all of it is editable by its subject. Whatever of that is meant to be trusted
  has to move to storage the sandbox can append to and not alter — remote,
  credential-based, and writable only forward. That is a larger piece of work
  than this ADR and is not a precondition for it: the verdict trail proposed here
  never lands in the sandbox at all, so it is unaffected either way. Worth its
  own ADR.
