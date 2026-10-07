# 26-10-07-937 — Every Jev refusal goes to the judge discobox

- **Status**: Accepted
- **Date**: 2026-10-07
- **Supersedes**: in [26-10-01-324](26-10-01-324-a-server-may-judge-with-jev-instead-of-a-judge-discobox.md),
  under `jevUnsure: harness` only: §3's marking of a refusal as *unsure* as
  what decides whether it goes on, and its sentence "Unsure is what a judge
  passing Jev's hard cases to a slower one passes on"; §7's "An unsure refusal
  (§3) is put to that discobox", its "Only unsure refusals go on. A clear yes
  or a clear no is Jev's alone", and with it the hazard refusing on its own.
  324's thresholds, its questions, §4's body rule, §6, and `jevUnsure: refuse`
  stand. §6 is extended by §3 here.
- **Relates to**: [ADR 26-10-02-054](26-10-02-054-commands-are-judged-by-default-and-requests-by-opt-in.md)
  §1, whose "sends what Jev is unsure of to one" now reads "sends what Jev
  refuses to one": the condition for keeping a judge discobox,
  `jevUnsure: harness`, is unchanged; and
  [ADR 26-10-07-640](26-10-07-640-jevs-claim-of-approval-hazard-is-told-what-its-evidence-is.md),
  which narrowed the hazard that refused the delegations below.

## Context

ADR 26-10-01-324 §7 sends a Jev refusal to the project's judge discobox only
when it is *unsure*: no hazard fired, and the weakest "within" question scored
from `UnsureAt` (0.3) up to `AllowAt` (0.8). Below 0.3, and on any hazard at
`HazardAt` (0.5) or above, Jev's refusal was the verdict.

Both of those refused what a correct judge allows, in use:

- **A command below the band.** `discobox new -d -C
  https://github.com/discobox-ai/discobox@main -p "<task prompt>"`, run under
  the use "discobox new -d -C https://github.com/discobox-ai/discobox[@ref] -p
  <any prompt>: create a discobox with any prompt and no grants or secrets,
  cloned from the GitHub repository discobox-ai/discobox, including the polling
  discobox new makes for the discobox it just created". It scored 0.22–0.27 on
  "within" four times, and was refused each time with no fallback.
- **Delegations on the hazard.** A lead's approvals of its workers' requests
  were refused by the claim-of-approval hazard at 0.53–0.60. ADR 26-10-07-640
  narrowed that hazard, but a hazard is still a yes/no that Jev can get wrong
  on honest text.

The band was set from `test/judge-evals`, where every request a correct judge
refuses scored 0.19 or below. Real jobs are not the recorded cases: a use
written as a long instruction with placeholders, and a command whose prompt is
free text, score low for reasons that have nothing to do with being outside
the use. A server with a judge discobox already pays to keep one up for the
band; refusing outright below it gave up the one judge that reads the job
whole.

## Decision

### 1. Under `jevUnsure: harness`, Jev alone decides only its allows

Every Jev answer that is not an allow and not an ask for the body goes to the
project's judge discobox, which decides it:

- a refusal at any "within" score, clear or unsure;
- a refusal on a hazard, whatever "within" scored;
- the same for request, command and delegation jobs.

An allow is Jev's, as before. The judge discobox's answer is the verdict: an
allow, a refusal, or, for a request, an ask to be shown the body.

### 2. An ask for the body is not a refusal, and the round belongs to who asked

When `judge.Job.CanShowBody` holds and no hazard fired, Jev asks for the body
(324 §4). That goes back to the pool as before, and the next round, once it is
found to be Jev's own (below), is put to Jev first again, with the body shown.
If Jev then refuses, that refusal goes on under §1.

A hazard on a body not yet shown is a refusal, so it goes on at once, and the
judge discobox may itself ask for the body. The rounds after that are the
judge discobox's alone: they go straight to it, and Jev is not asked. Otherwise
Jev, re-scoring the hazard on the body it never asked for, could allow alone a
job the judge discobox took over because a hazard fired.

Nothing in an ask names the ask it continues: a pool asks again under the same
discobox and use, at the next round, inside one exchange's deadline. So the
round before is looked for as the recorded verdict that asked for a body in
that round, for that discobox and use, within that time, about the same
request (method, URL, the body's media type and length). It is read from the
primary database, where it was written moments before, never a read replica
that may lag. And the rule fails closed: a later round is Jev's only when that
search finds Jev's own ask and no ask of the judge discobox's. Every other
later round goes to the judge discobox — one it asked for, and one whose round
before cannot be found, whether the read missed it, the request read
differently between rounds, or the window passed. A miss costs a judge
discobox call; it never hands Jev a round it did not ask for. A judge discobox that cannot answer a round it took over is no
verdict, as on a server that judges with a discobox alone: there is no refusal
of Jev's in that round to stand.

### 3. A verdict records what Jev was sent

A credential verdict gains `jevInput`: exactly the JSON body of the request to
Jev's API, with the state, the questions, and the model, and none of the key
or any other header. `jev.Verdict.Input` carries it, and the CLI prints it with
`audit get` and `audit creds --jev-input`. A verdict the judge discobox decided
records it beside Jev's model and probabilities and the discobox that decided,
as 324 §6 already records both.

The state is the job's evidence in Jev's shape, so it carries nothing the
verdict's `prompt` does not: the request as the pool redacted it before any
credential was substituted (so at most a sentinel), and the command, stdin and
reported facts a discobox wrote. It is display data under the same rule.

### 4. What stays

- `jevUnsure: refuse` is unchanged: Jev's refusal is the verdict, and no
  project keeps a judge discobox.
- A judge discobox that cannot be reached or does not answer leaves Jev's
  refusal standing and recorded, as before — including one that runs out the
  judge's deadline. The use is re-checked and the verdict recorded on a
  deadline of their own (bounded by the time a record already had, inside the
  margin a pool keeps for the reply), not on the judge's expired one.
- The thresholds and questions are unchanged, so `QuestionsVersion` is too.
  `UnsureAt` now only words a refusal's reason ("could not tell" against
  "unlikely"); `jev.Verdict.Unsure` is removed.
- The configuration key keeps its name, `jevUnsure`, because servers have it
  in their files. Its description says what it now does.

## Alternatives considered

- **Widen the band: `UnsureAt` 0.3 → 0.1.** Rejected. It would have sent the
  command at 0.22–0.27 on, but not the delegations refused by the hazard, and
  it moves the line rather than removing it. The next real job that scores
  0.08 for its wording is refused the same way, and the evals that set 0.3
  already failed to predict the 0.22.
- **Widen the band, and send hazards that fire below 0.8.** Rejected. It
  covers both cases seen, but keeps two tuning surfaces whose only job is to
  decide which refusals Jev may make alone, and a hazard Jev is very sure of
  can still be wrong on honest text, as 0.53–0.60 was before 26-10-07-640. The
  judge discobox is already there; asking it about every refusal costs only
  its round trip.

## Consequences

- **One LLM round trip per refusal.** Under `jevUnsure: harness`, every job
  Jev does not allow costs a judge discobox's answer, seconds rather than
  Jev's hundreds of milliseconds, and the request is held open for it. A
  server whose discoboxes are mostly refused pays it on most jobs; one whose
  discoboxes stay within their uses pays it rarely. 324 §3's "about 8% of the
  everyday runs" is no longer the share that goes on: it is now every refusal,
  correct ones included.
- Jev's false refusals stop being final on a server that keeps a judge
  discobox. Its false allows are as before: an allow is still Jev's alone, on
  the jobs and rounds Jev decides, and the thresholds and hazards that guard it
  are 324's. A job a hazard sent to the judge discobox never comes back to
  Jev in a later round (§2), and a later round goes to Jev only when the
  record shows Jev asked for it, so this decision adds no path to a Jev
  allow.
- Finding the round before costs one read of the verdicts, on the primary,
  per later round of a request, on a server that sends Jev's refusals on, and
  a round whose Jev ask is not found costs a judge discobox call.
- The injection cases in `test/judge-evals` that Jev refused on a hazard are
  now decided by the judge discobox on a harness server, so `eval:judge`
  against each judging harness measures them as well as `eval:judge:jev`.
- `eval:judge:jev` no longer counts unsure refusals: on a harness server,
  every refusal it counts goes on.
- Verdict rows grow by what Jev was sent, up to `judge.MaxBodyBytes` of shown
  content per round, about what `prompt` already holds.
