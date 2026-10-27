# 26-10-12-669 — Jev's confident refusal is final, and only what it is unsure of goes on

- **Status**: Superseded by [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
- **Date**: 2026-10-12
- **Supersedes**: in [26-10-07-937](26-10-07-937-every-jev-refusal-goes-to-the-judge-discobox.md):
  §1's "Every Jev answer that is not an allow and not an ask for the body goes
  to the project's judge discobox" — a refusal at any "within" score, and a
  refusal on a hazard whatever "within" scored; and §4's `UnsureAt` as only
  the wording of a reason, with `jev.Verdict.Unsure` removed. 937 §2's
  ownership of rounds, §3's `jevInput`, and the rest of §4 stand.
  In [26-10-01-324](26-10-01-324-a-server-may-judge-with-jev-instead-of-a-judge-discobox.md)
  §3: the hazard refusing at 0.5, and a refusal being unsure from 0.3. Its
  allow bar (0.8), its questions, and §4's body rule stand.
- **Relates to**: [26-10-02-054](26-10-02-054-commands-are-judged-by-default-and-requests-by-opt-in.md)
  §1, whose "sends what Jev is unsure of to one" reads as written again; the
  condition for keeping a judge discobox, `jevUnsure: harness`, is unchanged.

## Context

Jev answers a yes/no question with one calibrated probability that the answer
is yes. There is no separate "refuse" output: a confident no is a probability
near 0, as a confident yes is one near 1, and a value near 0.5 is Jev saying
it cannot tell.

The server has never read the two ends alike:

- **Allow** needs every "within" question at 0.8 or above: 80% sure of yes.
- **A final refusal**, under 324, was anything below 0.3: 70% sure of no.
- **The claim-of-approval hazard** refuses at 0.5: a coin's worth of yes.

937 found Jev's refusals wrong on real work and stopped trusting any of them.
Under `jevUnsure: harness`, the default, every answer that is not an allow now
goes to the project's judge discobox. Its evidence was:

- a legitimate `discobox new` scoring **0.22–0.27** on "within", four times;
- honest delegations refused by the hazard at **0.53–0.60**.

Neither is a confident answer. The first is 73–78% sure of no, short of the
80% an allow needs of yes. The second is barely past even. What 937 showed is
that the refusal bars sat inside the range where Jev is unsure, not that a
confident no is unreliable. The recorded cases say the same from the other
side: every request a correct judge refuses scored 0.19 or below on "within"
(324 §3), with the controls at 0.02–0.13.

Sending every refusal on has a cost 937 named and accepted: one LLM round
trip, seconds against Jev's hundreds of milliseconds, on every job Jev does
not allow, with the request held open. Correct refusals pay it too, and they
are most of them.

## Decision

### 1. Jev's answer is read at the same confidence in both directions

A probability at or above **0.8** is a yes. One below **0.2** is a no.
Anything from 0.2 up to 0.8 is **unsure**. That holds for every question,
"within" and hazard alike.

`decide` turns a job's probabilities into one of four outcomes, in this
order:

1. **Refuse, decided** — the hazard at 0.8 or above: Jev is sure the evidence
   claims approval.
2. **Unsure** — the hazard from 0.2 up to 0.8.
3. **Ask for the body** — when `Job.CanShowBody` holds. A body still unseen
   is never allowed or refused on its description, whatever "within" scored
   (324 §4).
4. On "within", by the weakest question:
   - **Refuse, decided** — below 0.2;
   - **Allow** — every one at 0.8 or above;
   - **Unsure** — anything else.

A delegation's one-question-per-use reads the same way: one use below 0.2
refuses the delegation, and it is allowed only when every use is at 0.8 or
above.

### 2. Only what Jev is unsure of goes to the judge discobox

Under `jevUnsure: harness`:

- **An allow is Jev's**, as before.
- **A decided refusal is Jev's.** It is the verdict, and the judge discobox
  is not asked.
- **An unsure answer goes to the project's judge discobox**, which decides
  it: allow, refuse, or, for a request, an ask to be shown the body. If the
  discobox cannot be reached or does not answer, the job is refused, as 937
  §4 leaves Jev's refusal standing.

The rounds of a request belong to whoever asked for the body, as 937 §2
decides. An unsure hazard on a body not yet shown goes on at once, and the
rounds after it are the judge discobox's alone.

Under `jevUnsure: refuse`, unsure refuses, and no judge discobox runs.

### 3. A verdict says which it was

A Jev verdict records whether Jev decided or was unsure, and its reason says
so in words: "judged it unlikely" for a decided refusal, "could not tell" for
an unsure one. `jev.Verdict.Unsure` returns. The thresholds change, so
`QuestionsVersion` does.

## Alternatives rejected

**Keep 937: every refusal goes on.** It pays an LLM round trip on every
correct refusal to recover wrong ones that, in everything recorded, sit at
0.2 or above. A discobox working outside its uses is the case that costs
most, and it is the one that least needs a second reading.

**A floor at 0.1** (937's "widen the band", which it rejected for moving the
line rather than removing it). There is still a line here, and the objection
stands that some legitimate job may score below it. What differs is where the
line comes from: 0.2 is the confidence an allow already requires, not a value
fitted to the last case that was refused. A no that is trusted less than a
yes at the same confidence is the asymmetry that put 0.22 on the wrong side.

**Restore 324's bars** (0.3 on "within", 0.5 on the hazard). They are what
refused the cases in Context.

**Leave the hazard at 0.5 and final.** A hazard is a yes/no Jev can get wrong
on honest text, as 0.53–0.60 was. An injection Jev is sure of scores well
above that, and one it half-suspects is exactly what the slower judge is for.

**Ignore the hazard below 0.5 rather than below 0.2.** A hazard at 0.4 beside
a "within" of 0.9 would be Jev's allow alone. The allow is the answer nothing
re-reads, so the hazard is held to the same confidence of no before it is
set aside.

**Different bars for commands and requests.** A command holds no connection
open and could afford to send every refusal on. Nothing recorded shows the
kinds scoring differently, and two rules are two tuning surfaces.

## Consequences

- **A legitimate job that scores below 0.2 is refused with no second
  reading.** Under 937 it was not. Nothing recorded has done so. Every
  refusal sent on since 937 has Jev's probabilities beside the judge
  discobox's decision on its verdict, so an allow there with "within" below
  0.2 is the evidence against this bar, and `eval:judge:jev` counts decided
  refusals of cases a correct judge allows.
- Correct refusals stop costing a judge discobox call on a harness server.
  The share of jobs that goes on is again about what Jev is unsure of: 8% of
  the everyday eval runs under 324's narrower band, more under this one.
- **Fewer hazards are Jev's alone, in both directions.** One from 0.5 up to
  0.8 no longer refuses outright, and one from 0.2 up to 0.5 no longer lets
  an allow through: both go on.
- **A server with `jevUnsure: refuse` refuses more.** A hazard from 0.2 up to
  0.5 beside a high "within" was allowed there, and now refuses. A refusal
  from 0.2 up to 0.3 only changes its wording.
- No path to a Jev allow is added: the allow bar is unchanged, and the hazard
  must now be below 0.2, not below 0.5, for Jev to allow alone.
- `eval:judge:jev` counts unsure answers again, beside decided refusals.
