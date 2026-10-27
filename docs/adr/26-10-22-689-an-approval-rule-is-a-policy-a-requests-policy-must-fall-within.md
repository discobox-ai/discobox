# 26-10-22-689 — An approval rule is a policy a request's policy must fall within

- **Status**: Superseded by [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
- **Date**: 2026-10-22
- **Relates to**: [26-10-20-782](26-10-20-782-approved-access-is-a-policy-decided-at-the-network-boundary-fast-path-first.md)
  §§1–4 (the policy, its words and variables) and §11 (a delegation's policy,
  whose comparison a rule reuses);
  [26-10-14-691](26-10-14-691-the-judge-is-told-who-is-asking-from-the-control-planes-record.md)
  (the facts the judge is told, unchanged).
  Decided on issue #282 for #317; #312 depends on it.

## Context

ADR 26-10-12-330 made an approval rule a project's standing answer to a
credential request: facts the server checks — repositories, credential,
hosts, purpose, lifetime — and **uses**, sentences the judge reads against
the uses a request asks for. ADR 26-10-20-782 then made a grant's
authorization a policy of statements, and since #316 agents write one. A rule
was never tested against a request that carries a policy (`ruleTestable`):
read against uses alone, it would have approved a policy nobody read. Once
#312 makes every request carry a policy, every rule would be dead.

782 deferred approval rules over policies until policies were what grants
carry, and noted that a policy makes most of a rule's test a structural
comparison, as §11 makes it for a delegation. That is now true.

## Decision

### 1. A rule carries a policy, in place of uses

A rule's content is a policy — statements and the definitions of their
words — stored as structure, written and shown as sentences
(`policy.Parse`/`Format`), and validated as a grant's is (`Validate`,
`judge.PolicyFits`). It must allow something. Its repositories, credential,
hosts, purpose and lifetime cap stay the server's facts, as in 330 §§2–3.

A rule records no resolved values: its variables bind to whichever discobox
asks, at each test, so `$primary.repo` is the asking discobox's repository.

### 2. Code decides coverage; the judge reads only conditions

A request is covered when its policy is `policy.Within` the rule's, both
resolved for the discobox that asked:

- every allow asked for falls within an allow of the rule — the same or
  fewer actions, the same or a narrower resource, name and scope (782 §11's
  test);
- every deny of the rule is carried: the request has a deny that refuses at
  least as much — the same or more actions, the same or a wider resource,
  name and scope, `anywhere` being every scope — with no condition the rule's
  deny lacks;
- a word both use is defined alike; one defined otherwise is not covered;
- a policy none of whose allows resolves for the asking discobox allows
  nothing, and is not covered.

An allow code finds covered in every part but its conditions is put to the
judge. An allow with no condition at all is wider than one with any, and code
refuses it. Every other difference in `when` or `unless` is the judge's.

The same comparison decides a delegation's hand-off (782 §11), so the two
refinements above — a wider deny carries, and an allow that drops every
condition is refused by code — amend §11's "the same deny" and "conditions
are the judge's", before anything has delegated a policy.

### 3. The rule job is one allow against one

A rule job (`judge.KindRule`) is one allow asked for against one allow of the
rule that covers it but for its conditions, each as its sentence bound for
the asking discobox: is the one asked for within the rule's, its conditions
applying only where the rule's apply? It is within when the judge finds it
within any one of the rule's allows. The judge is told the asking discobox's
facts, as 26-10-14-691 has it. Jev asks one question, `use_0`, and no `keep_N`:
a statement's limits are its denies and conditions, which code and that one
question compare, not a closing "Never …" sentence (26-10-14-223, for rule
jobs).

`judge.HoldToRule` builds the questions, in the root module, so the control
plane and the judge evals hold a request to a rule the same way.

### 4. What a rule answers, and what it mints

A rule answers only a pending agent request that **carries a policy** and asks
to **use** a credential (narrowing 330 §4). An ask of uses alone waits for a
person, as there is nothing to hold to the rule's policy.

A rule does not approve a **delegation** yet (narrowing 330 §9). A
delegation's uses still bound a hand-off of uses alone (782 §11), and a rule
holds a request's policy, never its uses, so a delegation it minted would
carry uses nobody read. Making a delegating rule is refused.

The grant a rule mints carries the request's uses and policy as asked, held
to the values the policy resolved to when it was compared.

### 5. Existing rules are deleted

Every rule with no policy is deleted at startup (`retireApprovalRuleUses`),
each logged with what it said; its grants stand, and they and its verdicts go
on naming it, as when a person deletes one. People make again, as policies,
the rules they want. A rule with a policy is never touched, so an older server
that adds the uses column back costs nothing made since, and a rule it writes
without a policy is deleted at this one's next start.

### 6. "Auto approve" proposes a policy, held to the request

The proposal job (330 §7) writes statements, not uses: the request's policy,
as the same work on any target of its kind (`ProposalSystem`), keeping every
variable, deny and condition that is not one instance. The model defines no
word: the written policy takes the request's definitions for the words it
uses, so a word it brings in has none and is refused (`judge.WrittenPolicy`).

What it wrote is held, before it is shown (330 §8), to what a rule may be,
to covering the request as a rule later would (§2, its condition questions
recorded), and to carrying every deny the request asked for. One that drops a
deny is shown as not covering: as a rule it would approve later asks without
it. A person edits it as sentences before it is a rule.

330 §6 is unchanged: a project keeps a judge while it has a rule switched on,
since a rule's conditions may still need one.

## Alternatives rejected

- **Keep judging uses beside the policy.** The request's policy is what its
  grant enforces; judging its uses too would ask a model the question 782
  found it answers worst, and approve nothing a policy alone would not.
- **Put the whole policy to the judge.** A model asked whether a list is
  within a list leaks across items (782, Context). Code decides what
  structure settles, and the judge reads one pair of conditions at a time.
- **Keep the old rules, disabled, with their uses shown.** No code reads a
  sentence as a policy, so the console and API would carry a second kind of
  rule that can only be read and deleted, for good.
- **Convert the old rules through the proposal job.** A rule is a person's
  approval given in advance (330 §9); a model's rewrite of it would become
  one nobody approved. Rules are few, and the person who made one can make
  it again.
- **Propose the request's policy as asked, with no model.** It approves only
  the very branch and issue asked for. It is still the starting point when no
  model can write, and is what a rule approves when the person names none.
- **Let a rule delegate, judging the uses of a delegation it mints.** That is
  the uses job this decision removes, kept for one purpose until #312 removes
  the hand-off of uses alone that needs it.

## Deferred

- **A rule that delegates.** Revisit when every hand-off is bound by the
  delegation's policy (#312), so a delegation's uses bound nothing. That
  condition is met (#312); it stays deferred until a rule that hands on the
  power to hand on is asked for, since a person approves every delegation
  today and the uses no longer stand in the way. (Amended in issue #312,
  before this decision shipped.)
- **A word the rule and the request define otherwise, or a synonym.** Not
  covered, and the request waits for a person. Revisit if agents writing from
  the skills' fixed words still miss rules because of it.

## Consequences

- A released database loses its approval rules on upgrade; each is logged.
  The grants they minted stand.
- Most rules approve with no judge asked at all; only conditions code cannot
  compare reach one, and a server with no judge still approves what code
  finds within a rule.
- A request of uses alone, and any ask to delegate, always waits for a
  person.
- The `rule-*` and `propose-*` cases in `test/judge-evals` are policies, and
  most are decided by code.
