# 26-10-12-330 — A credential request may be answered by a rule the judge reads

- **Status**: Accepted
- **Date**: 2026-10-12
- **Relates to**: [26-09-30-782](26-09-30-782-a-discobox-answers-its-own-discoboxes-requests-within-what-it-may-delegate.md),
  whose split — the server checks facts, the judge reads two authorization
  sentences — this reuses with the project holding the bound instead of a lead
  discobox, and whose "only a person approves" a gate or a delegation a rule
  is read as (§9);
  [26-10-01-324](26-10-01-324-a-server-may-judge-with-jev-instead-of-a-judge-discobox.md),
  whose Jev-first path a rule's content is judged on unchanged;
  [26-10-02-054](26-10-02-054-commands-are-judged-by-default-and-requests-by-opt-in.md)
  §1, whose condition for keeping a judge discobox gains a third reason (§6);
  [26-09-22-838](26-09-22-838-a-dedicated-pool-harness-judges-commands-and-credential-bearing-requests.md)
  §2, whose "typed job, not a prompting endpoint" the proposal job follows (§7).
- **Issue**: [#162](https://github.com/discobox-ai/discobox/issues/162), which
  this settles in part (see Consequences and Deferred).

## Context

An agent asks for a credential through `discobox-access request`: a
credential, hosts, a justification, a lifetime, and **uses** — sentences
saying what it will do. A person reads the card and approves, and the grant
carries those sentences. Every later command is judged against them.

The asks repeat. Issue #162 reports GitHub as nine in ten of the credentials
one person approves, box after box. Skills ask for the same thing every run,
almost: `develop` asks for six GitHub uses with `discobox/<slug>` filled in;
`triage-issue` asks for four, word for word; `orchestrate-issues` asks for the
discobox API, a GitHub use grant, and a GitHub delegation. A person approves
each by recognizing the shape, not by deciding anything. That is the work a
rule should do.

Today the only standing answers are:

- **A pre-approval** (`CreateSecretGrant` with uses, at project or harness
  scope). Its uses are fixed sentences every covered discobox shares, so a
  `develop` run is granted "any branch `discobox/<slug>`" rather than its own
  branch, and every box in scope holds it whether it asked or not. It is
  made only from the CLI, never from the inbox.
- **A lead's delegation** (782). It answers only the discoboxes the lead
  created, and exists only while a lead does.

Neither answers "a box doing this kind of work on this repository asked for
what that work always asks for".

Whether an ask is the usual one, from the usual kind of box, is a reading of
sentences, which is what the judge is for. Three facts shape how it can be
used:

- **The ask is the agent's writing.** Its uses, name and justification are
  whatever the agent — or an issue it read — put there. A person reading a
  card is the check today. A rule replaces that person, so the rule's text is
  the authorization and the ask is what is measured against it.
- **Jev writes nothing** (324). It can say whether an ask is covered by a
  rule. It cannot draft a rule.
- **A judge already runs without `judgeCredentials`.** A project keeps a judge
  discobox while `judgeCommands` (on by default) or `judgeCredentials` is on
  (054 §1). Only a server with both off has none, and there a delegation is
  refused outright.

## Decision

### 1. An approval rule is a project's standing answer, and only a person makes one

An **approval rule** belongs to a project and says: a request like this, from
a discobox like that, is approved without asking. It holds:

- **scope** — which discoboxes it is tested for (§2);
- **one credential** — a well-known ID, or a project secret;
- **hosts** the grant may name, and **one purpose** (`use` or `delegate`), as
  a grant has one;
- **uses** — sentences saying what may be approved (§3);
- **a lifetime cap** for the grants it mints, and optionally its own expiry;
- who made it, and whether it is enabled.

A person makes one two ways, and both make the same thing:

- **Answering a request with "auto approve"**, which proposes rules (§7),
  creates the one chosen, and approves the request under it, in one write.
- **Directly** (`discobox secret rule create`), as a pre-approval is made.

The sandbox role has no rule routes. A discobox never makes, edits, or reads
one.

**A rule approves or says nothing.** It either covers a request, which is
then approved, or it does not, and the request waits for a person exactly as
it does today. No rule denies anything.

### 2. Scope is a sentence the judge reads, against facts the control plane recorded

A rule's scope is a sentence — "discoboxes started to deliver or triage an
issue in discobox-ai/discobox", "any discobox on a repository in the
discobox-ai organization that a person started" — and, optionally, one
discobox it is pinned to. No sentence and no pin is every discobox in the
project.

**A pin is compared, not read.** "This discobox" is its ID.

**The sentence is read against a fact sheet the server composes** from the
discobox's own record, every line fixed when it was created:

- its primary source's repository and ref, and its other sources;
- who created it — a person, or a discobox, and which;
- the prompt it was created with, **labelled with who wrote it**;
- the skills it was created with, its harness, and its pool's platform.

**Nothing a discobox writes about itself is on the sheet**: not its tags or
description (ADR 0136), not the justification on its request. The prompt is
its creator's words, not its own. When the creator is a discobox, the label
says so, and a rule that wants only a person's word says "started by a
person".

The judge gains a job kind, `scope`: the sentence is the authorization, the
fact sheet is what is judged, and the prompt in it is data, never
instructions.

**It is decided once for a discobox and a rule**, since the facts do not
change, recorded as a verdict of kind `scope`, and reused for every request
that discobox files while the rule stands as it was read.

**Jev's yes is a match, and anything else is not.** With Jev, a scope
matches at 0.8 or above and the claim-of-approval hazard clear. Nothing else
is put to the judge discobox: most discoboxes do not match most rules, and a
wrong "no match" costs only what happens today, a person being asked. A
server with no Jev key asks its judge discobox.

### 3. Content is sentences the judge reads; everything else is a fact the server checks

The server holds a request to the rule the way it holds an approval to a
delegation (782 §3):

- **the same credential** — the well-known ID the request names; for an ask
  that names none, hosts the rule's secret is bound for;
- **hosts within** the rule's hosts;
- **the same purpose**;
- **a lifetime fitted**, not refused: what the request asked for, else the
  hour, capped by the rule and by the secret's limit;
- **uses within** the rule's uses, which is the judge's question.

The judge gains a second job kind, `rule`: the rule's uses as the
authorization, the uses asked for as what is judged, and no request evidence.
It is the delegation job's shape with its own words, because who wrote what
differs. A rule's uses name a class ("any issue in org/repo", "a branch named
`discobox/<slug>`", "never `main`"), and the uses asked for are an agent's
and may be worded to pass. The rule covers the request only when every use
asked for is within it. The justification is not shown: it authorizes nothing
and can only steer.

A request that names no uses is a person's, as in 782.

### 4. A rule is tested when a request is filed

Filing a request still records it as pending and returns at once. Rules are
tested off that path, by a reconciler over pending requests:

- Only agent-credentials requests are tested. Reactive requests, refresh
  requests and host trusts stay a person's.
- A rule is tested in the order of what each step costs: the facts (§3),
  then its scope (§2, usually already decided), then its content.
- Rules are tested one at a time, those pinned to the discobox first, each
  on its own — never several rules' uses together. The first that covers the
  request ends it.
- A rule that covers approves through the same write a person's approval
  takes (`mintGrantAs`, the agent binding, the request marked approved), with
  the rule read again in that transaction: still enabled, unexpired, with the
  text the judge read.
- The grant is what a person's approval would mint — sandbox-scoped, the uses
  **as asked**, so each box holds its own branch and not the class — and
  records the rule it was minted under.
- **A request no rule covers stays pending** (§1). The card shows each rule
  tested and why it did not cover the request.
- A request is tested once per rule as that rule stands. A rule made or
  edited later is tested against what is pending. An identical ask reuses the
  open request (`asksTheSame`), so asking again does not ask the judge again.

A grant minted by a rule is independent once made. Disabling or deleting the
rule mints nothing more and revokes nothing; grants list by rule, to be
revoked deliberately.

### 5. The same judge answers the content job, the same way

A `rule` job goes through `judges.Service.put`, like a delegation:

- Jev first, when the server has a key: one noul per use asked for, plus the
  claim-of-approval hazard.
- What Jev does not allow is handled as for any job, by `jevUnsure`: put to
  the project's judge discobox, or left as Jev's answer.
- With no key, the judge discobox answers.
- No judge, no answer, or anything that does not decode is not a yes.

The fallback is kept here and not for scope because of how often each is
asked: a content job runs only for a discobox already in scope, about a
request somebody would otherwise have to read.

The verdict is recorded before anything is minted, as kind `rule`, naming the
rule, the request, and the asking discobox, with Jev's model, probabilities
and input when Jev was asked. Nothing stands for either job.

### 6. A rule is a reason to keep a judge

Rule jobs hang on neither `judgeCommands` nor `judgeCredentials`. A project
keeps a judge discobox when either switch wants one (054 §1) **or the project
has an enabled rule**, and the backend is `harness` or sends Jev's refusals to
one. A rule job is refused only when there is nothing to ask.

On a default server nothing changes: `judgeCommands` already keeps a judge.
On a server with both switches off, a project's first rule brings its judge
up, and nothing but rules is put to it.

### 7. "Auto approve" proposes rules at three levels, for scope and for content

Choosing auto approve on a request asks the server for proposals: three
scopes and three contents, each narrowest first.

**Scope.**

1. **This discobox** — the pin. No model.
2. **Discoboxes doing this work on this repository** — from the fact sheet:
   the repository, and the kind of task the prompt describes.
3. **Any discobox on this repository.**

**Content.**

1. **As asked** — the request's uses, verbatim. No model.
2. **The same work on any target of its kind** — placeholders where the
   request named one instance: issue 42 becomes any issue in that
   repository; `discobox/fix-foo` becomes any `discobox/<slug>` branch.
3. **The same kind of work across the resource** — writing issues in that
   repository; delivering pull requests from any branch but `main`.

Levels 2 and 3 of each are written by a model, as a typed **proposal job** in
the `judge` package: its own system prompt, schema and strict decode, put to
the project's judge discobox with `--no-tools`. The request's uses and the
fact sheet are data in it. Every `never …` limit a use states is kept at
every level. The proposer writes sentences only; credential, hosts and
purpose are copied from the request by code, never generated.

The person picks one scope and one content, then the lifetime, on the steps
the approval dialog already has. The card shows the rule's full text: that
text is what is being approved, and a generated sentence nobody read is the
agent's ask promoted to authority.

Where no model can write (Jev with `jevUnsure: refuse`, or no judge yet on a
server that judges nothing else), level 1 of each is offered alone and the
sentences can be written by hand.

### 8. A proposal is offered only once the judge has found it covers the request

Each candidate is put to the judge as the exact job §4 would later ask, on
the path that job takes: a scope candidate against the asking discobox's fact
sheet (§2), a content candidate against the request's uses (§5).

- Only a candidate the judge says covers is offered. One it does not is
  shown as failed, with the reason, and cannot be chosen.
- Content level 1 is tested too. A judge that will not find sentences within
  themselves is a rule that will never approve a repeat ask, and that is
  worth knowing before it is made.
- Choosing candidates approves the request under the verdicts already on
  record. Edited text is judged again before it is accepted.

`test/judge-evals` gains scope and rule cases recorded from the three skills'
real boxes and asks, with controls each level must not cover (another
repository, a push to `main`, a merge, a box started for other work).
Proposer cases are scored by the same judge: each level covers its request,
each wider level covers the narrower, and no level covers a control.

### 9. A rule may hand on a gate or a delegation, because a rule is a person's

782 keeps two approvals for a person: a request to delegate, and a gate
(`ai.discobox.sandbox`). A rule made by a person is that person's approval
given in advance, so a rule may name either. `orchestrate-issues` is not
auto-approved otherwise. The proposal card says what such a rule hands out,
in the gate card's words. A discobox still approves neither.

## Alternatives rejected

**Scope as a selector of recorded fields** (this discobox, this repository,
this creator), compared by code. It is exact and can be listed, and it was
this ADR's first draft. It cannot say an organization, a kind of task, or
"started by me or by one of my leads" without a field, a glob or an operator
for each, and the prompt — the best record of what a box is for — is free
text no selector reads.

**Let the scope read what a discobox says about itself** (tags, description,
the skill it says it is running). The server does not know which skill a box
is running. A box that can write its way into a rule's scope has chosen its
own rule.

**Send a scope Jev does not match to the judge discobox.** Most pairs of
discobox and rule do not match, so nearly every pair would cost a model
call, to avoid a miss whose whole cost is a person being asked.

**A wider pre-approval instead** (a project-scoped grant with class-shaped
uses). Every covered discobox holds it unasked, and commands are judged
against the class, not the box's own branch or issue. A rule mints the
specific ask. The two are not exclusive: approving a request at project scope
from the inbox (#162's triage, option a) can be decided on its own.

**Match by a hash or template of the ask.** `develop`'s asks differ by slug
and by the one-line change every run; a template language for uses is a
second, worse reading of sentences.

**A rule that ships with a skill or repository.** A rule that can approve is
authority, and a repository is written by whoever can write it (838's
rejection of endpoint rules in project configuration). A skill's ask is where
a proposal starts; a person makes the rule.

**Reuse the delegation job as is.** Its words describe a discobox handing a
credential on under uses a person approved for it. Told that about a rule,
the judge is told the wrong thing about who wrote the uses being judged.

**Hang rule jobs on `judgeCommands`**, as delegation does. A server that
turned command judging off would have to turn it back on to auto-approve
anything. A rule is its own opt-in.

**Call a model API from the control plane to write proposals.** 838 rejected
a second model integration; the judge discobox already runs the project's
harness with no tools and no work sources.

**Let a rule deny.** A rule that misreads would refuse work a person would
have approved, with nobody shown the question.

**Show the content judge the justification.** It is text the rule's author
did not write, placed where it can argue for itself.

## Deferred

- **Sending a refused command to a person** (#162's escalation). A request no
  rule covers already waits for a person. A command the judge refuses at run
  time is still refused, and the agent files a new request. Revisit when
  re-asking after a refusal is seen to be what agents do by hand.
- **A built-in rule for a well-known credential** (#162's default GitHub
  uses). Proposals write a rule from the first ask. Revisit if the same rule
  is being made by hand in project after project.
- **A rule above a project** (#162's user-level grant). A rule names a
  project's secret and is tested against a project's discoboxes. Revisit with
  whatever first gives a server anything shared across projects.
- **Showing which existing discoboxes a scope matches** when a rule is made.
  Only the asking discobox is tested (§8). Revisit if rules are made that
  match more than their author meant.
- **A server-wide off switch.** Disabling a rule is per rule. Revisit when an
  operator needs to forbid rules for projects they do not administer.
- **Limits on what one rule mints** (grants per discobox, per day). An open
  identical ask is reused, so a loop costs no judge calls. Revisit if rules
  are seen minting in bulk.
- **Rules for host trust and reactive requests.** Neither carries uses to
  read.

## Consequences

- **#162 is settled in part.** A standing answer for GitHub, made from the
  inbox, scoped by repository or organization, holding its "never `main`,
  never merge" limits, with what it does not cover going to a person. Not
  settled: the three items deferred above.
- New resource `ApprovalRule` with its store, service, and routes under the
  project; `SecretGrant` and `CredentialVerdict` gain the rule they were made
  under; the approve body gains a rule to create.
- `judge` gains the `scope` and `rule` job kinds and the proposal job;
  `PromptVersion` and Jev's `QuestionsVersion` change. A judge discobox whose
  sandbox agent predates a kind refuses its jobs until its image is
  replaced, so nothing is covered until then.
- A project with a rule keeps a judge discobox on a server that otherwise
  judges nothing.
- The approval dialog gains the auto-approve path; `discobox secret request
  approve` and `discobox secret rule` do the same acts from the CLI.
- Proposing costs one model call and up to six judge calls, paid when a
  person asks for it. A discobox's first request under a rule costs one
  scope job; each request in scope costs one content job per rule until one
  covers it.
- **Which discoboxes a rule covers cannot be listed**, only asked about one
  at a time. The verdicts say which it has matched.
- **A scope that should match may not.** Jev reads a long sentence against
  free text poorly (ADR 26-10-07-937's Context), and a miss is not sent on.
  The card says the scope did not match and at what score.
- **A lead's prompt is its own writing.** A lead that read hostile text can
  start a box with a prompt that reads as the kind of work a rule covers.
  The label says a discobox wrote it, and the content check and the command
  judge still hold what that box is given.
- With Jev, a rule's sentences, the fact sheet with its prompt, and the uses
  asked for go to TypeSafe.
- **A wrong yes mints a grant with no person having read the ask.** What
  still holds: the credential, hosts, purpose and lifetime are facts the rule
  fixed; the grant is one discobox's; and every command is judged against
  the use it was minted for.
- **"My pull request" is not checked at approval.** A rule's uses can say
  "its own pull request", and nothing at the ask knows which that is. The
  command judge holds each command to the minted use.
- A rule with no scope approves for discoboxes that do not exist yet,
  including ones a discobox creates. The proposal card names that in those
  words.
