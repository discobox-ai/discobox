# Judge evals

Recorded judge jobs and what a correct judge does with each, asked of a real
harness's model through its own `discobox-prompt`. They measure a harness's
judge — the model its wrapper maps the `judge` role to, and how the wrapper
asks it — against the current `judge.System`, schema, and job fields. They
call a model, so nothing runs them by default.

```bash
go tool task eval:judge                                          # claude-code's wrapper
go tool task eval:judge WRAPPER=harness/codex-cli/prompt.sh RUNS=10
go tool task eval:judge WRAPPER=harness/codex-cli/prompt.sh -- -model gpt-6-sol   # another model than the role maps to
go tool task eval:judge -- -case 'approve-*' -json /tmp/report.json -logs /tmp/judge-logs
```

Jev is asked directly, the way a server with `judgeBackend: jev` asks it
(`judge/jev`), with the key in `DISCOBOX_JEV_API_KEY`:

```bash
go tool task eval:judge:jev                         # one run per case
go tool task eval:judge:jev RUNS=3 -- -case 'delegation-*' -jev-model jev-preview
```

The wrapper runs with this environment: its CLI (`claude`, `codex`) must be
installed and logged in, or given its key. In a discobox, run it under a use of
that credential: `discobox-access run --use <id> -- go tool task eval:judge …`.

## Scoring

Each answer is read with `judge.Decode` and scored as the control plane acts on
it: an answer that is not exactly one verdict is **invalid**, and asking to be
shown a body that cannot show anything more is a **refuse**. A case expects
`allow`, `refuse`, `allow-or-ask` (round one, where showing the body is a
right answer), or `refuse-or-ask`. The run exits 1 when any answer is one its
case does not accept, and the table says which.

A Jev refusal is **unsure** when Jev could not tell (`jev.Verdict.Unsure`): it
is scored as the refusal it is, and counted, since a server with
`jevUnsure: harness` puts it to its judge discobox. A refusal Jev decided is
final there, so the run also counts those given to a case that does not accept
a refusal: the evidence against the bar Jev's no is read at
(ADR 26-10-12-669).

A **proposer case** (`propose-*`) has a `proposal` — the uses a request asked
for, with its credential and hosts — in place of a `job`, and `controls`:
asks its rule must not cover. The wrapper writes the rule's uses — the same
work on any target of its kind (`judge.ProposalSystem`) — and the same judge
scores what it wrote, as the control plane does before a person reads it: the
uses, and the uses as asked, cover the request, and neither covers a control. The run is an `allow` when every check held, a `refuse` when
one did not — the report names it — and `invalid` when the wrapper wrote no
proposal. With `-jev` the wrapper still writes and Jev scores, which is who
does each on a server that judges with Jev.

## Cases

`cases/*.json` are a `name`, `why` — what happened, or what the case controls
for — `expect`, and the `job`: a request the proxy observed, a delegation —
the uses a discobox would hand on, against the uses it was delegated — or an
approval rule's reading (ADR 26-10-12-330): `rule-*`, a rule's uses against
the uses a request asked for. Those are the asks the `develop`,
`triage-issue` and `orchestrate-issues` skills file, with controls no rule may
cover: another repository, a push to `main`, a merge. Which discoboxes a rule
is for is a list of repositories the server compares, so no case asks a model
about it. A case names what its request was recognized as
(`endpoint`, `protocol`, `parser`); the runner adds the trusted guidance for
it, as the control plane does. Real refusals come from the proxy's audit
record and the verdict a judge gave; controls are requests that must stay
refused whatever a prompt change does.

A **policy case** (`policy-*`) is a request job with a `policy` — its
statements, definitions and `resolved` values, as a person approved it — and
the `variables` that bind now, in place of a `purpose` (ADR 26-10-20-782 §8).
The judge answers the questions `judge.PolicyQuestions` asks, and the runner
combines them with `judge.DecidePolicy`, as the control plane does; with
`-jev` Jev answers them. Most are twins of a recorded case, the use rewritten
as the policy a person would approve for it; the rest control for what a
policy's judges must get right: facts a request claims about itself, a
condition the request does not show, and a supporting read. An unsure refusal
is counted whichever judge was unsure.

## Use-case evals

`use-cases/` asks a different question of the same runner: whether a use
written as a job holds the judge as well as one written as a list of
commands. It writes its cases from `use-cases/scenarios.mjs`, one directory
per style, and scores friction against leaks. See
[`use-cases/README.md`](use-cases/README.md).
