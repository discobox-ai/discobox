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

Whether an ask is the usual one is a reading of sentences, which is what the
judge is for. Which boxes it is usual for is not: the work repeats per
repository, and a repository is a fact the control plane already holds. Three
facts shape how the judge can be used:

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
a discobox of these repositories, is approved without asking. It holds:

- **repositories** — which discoboxes it is tested for (§2);
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

### 2. A rule is for a list of repositories, or for every discobox, and the server compares

A rule says which discoboxes it is for with a list of **repositories**. A rule
that names none is for every discobox in the project, and is one only because
a person said so. There is nothing else: no sentence, and no single discobox.

**It is compared, not read.** No model is asked which discoboxes a rule is
for. A rule that names repositories is for a discobox whose **primary source**
is one of them, exactly. A discobox's additional sources never count: a box
that pulled a repository in beside its work is not a box working on it.

**A repository is its host and path**, `github.com/org/repo`:

- the scheme and any userinfo are dropped, and the host is lower-cased;
- git's scp-like form, `git@github.com:org/repo.git`, is the same repository
  as its HTTPS URL;
- a trailing `.git` and trailing slashes are dropped.

One piece of code reduces both sides to it: what a person gives a rule, and
what a discobox's source says.

**A discobox's repository is its primary source's URL**, and for a source
delivered from a local directory, the remote its checkout tracks, when the
client that created the discobox reported one. A local source with no remote
has no repository. Only a rule for every discobox answers it: a path on
somebody's machine names nothing a rule could mean.

**A rule's repositories are stored normalised.** A person gives a URL, the
scp-like form, or `host/org/repo`. A bare `org/repo` names no host and is
refused, with the host to name: the same path on another host is another
repository. An entry is said once, and a rule names a bounded number of them.

**A rule for every discobox is said, never assumed.** It is its own answer,
`everyDiscobox`, given in so many words on every route that makes or edits a
rule, by the API and by every client, and refused beside repositories:

- made directly, a rule names at least one repository or says
  `everyDiscobox`; one that says neither is refused;
- edited, repositories replace the rule's and `everyDiscobox` removes them;
  an empty list is refused, and saying nothing leaves them as they are;
- made from a request (§7), saying nothing makes it a rule for the asking
  discobox's repository, and is refused when that discobox has none.

**Nothing a discobox writes about itself decides it**: not its tags or
description (ADR 0136), and not anything on its request. Its source is what
its creator gave it.

### 3. Content is sentences the judge reads; everything else is a fact the server checks

The server holds a request to the rule the way it holds an approval to a
delegation (782 §3):

- **the same credential** — the well-known ID the request names; for an ask
  that names none, hosts the rule's secret is bound for;
- **a discobox of one of the rule's repositories** (§2), or any discobox for
  a rule that names none;
- **hosts within** the rule's hosts;
- **the same purpose**;
- **a lifetime fitted**, not refused: what the request asked for, else the
  hour, capped by the rule and by the secret's limit;
- **uses within** the rule's uses, which is the judge's question.

The judge gains a job kind, `rule`: the rule's uses as the
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
  the repository among them (§2), then its content, which is the one thing
  the judge is asked.
- Rules are tested one at a time, those that name repositories before those
  for every discobox, each on its own — never several rules' uses together.
  The first that covers the request ends it.
- A rule that covers approves through the same write a person's approval
  takes (`mintGrantAs`, the agent binding, the request marked approved), with
  the rule read again in that transaction: still enabled, unexpired, with the
  text the judge read.
- The grant is what a person's approval would mint — sandbox-scoped, the uses
  **as asked**, so each box holds its own branch and not the class — and
  records the rule it was minted under.
- **A request no rule covers stays pending** (§1). The card shows each rule
  tested and why it did not cover the request. A rule for other repositories
  says so as a fact: nobody was asked, and no verdict is recorded for it.
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

The verdict is recorded before anything is minted, as kind `rule`, naming the
rule, the request, and the asking discobox, with Jev's model, probabilities
and input when Jev was asked. Nothing stands for a rule job.

### 6. A rule is a reason to keep a judge

Rule jobs hang on neither `judgeCommands` nor `judgeCredentials`. A project
keeps a judge discobox when either switch wants one (054 §1) **or the project
has an enabled rule**, and the backend is `harness` or sends Jev's refusals to
one. A rule job is refused only when there is nothing to ask.

On a default server nothing changes: `judgeCommands` already keeps a judge.
On a server with both switches off, a project's first rule brings its judge
up, and nothing but rules is put to it.

### 7. "Auto approve" offers the asking discobox's repository, and proposes uses at three levels

Choosing auto approve on a request asks the server for proposals.

**Which discoboxes** has two answers, and no model writes either:

1. **Discoboxes of this request's repository** — the asking discobox's (§2),
   which the answer names. Offered only when it has one.
2. **Every discobox in this project.**

**Content** comes at three levels, narrowest first.

1. **As asked** — the request's uses, verbatim. No model.
2. **The same work on any target of its kind** — placeholders where the
   request named one instance: issue 42 becomes any issue in that
   repository; `discobox/fix-foo` becomes any `discobox/<slug>` branch.
3. **The same kind of work across the resource** — writing issues in that
   repository; delivering pull requests from any branch but `main`.

Levels 2 and 3 are written by a model, as a typed **proposal job** in the
`judge` package: its own system prompt, schema and strict decode, put to the
project's judge discobox with `--no-tools`. The request's uses are data in
it. Every `never …` limit a use states is kept at every level. The proposer
writes sentences only; credential, hosts and purpose are copied from the
request by code, never generated.

The person picks which discoboxes and one content, then the lifetime, on the
steps the approval dialog already has. The card shows the rule's full text:
that text is what is being approved, and a generated sentence nobody read is
the agent's ask promoted to authority.

Where no model can write (Jev with `jevUnsure: refuse`, or no judge yet on a
server that judges nothing else), the uses as asked are offered alone and
wider ones can be written by hand.

### 8. A proposal is offered only once the judge has found it covers the request

Each content candidate is put to the judge as the exact job §4 would later
ask, on the path that job takes: against the request's uses (§5).

- Only a candidate the judge says covers is offered. One it does not is
  shown as failed, with the reason, and cannot be chosen.
- Level 1 is tested too. A judge that will not find sentences within
  themselves is a rule that will never approve a repeat ask, and that is
  worth knowing before it is made.
- Choosing a candidate approves the request under the verdict already on
  record. Edited text is judged again before it is accepted.
- A rule made from a request is held to the request's own discobox as well:
  repositories it is not of are refused, as uses that do not cover it are.

`test/judge-evals` gains rule cases recorded from the three skills' real
asks, with controls each level must not cover (another repository, a push to
`main`, a merge). Proposer cases are scored by the same judge: each level
covers its request, each wider level covers the narrower, and no level covers
a control.

### 9. A rule may hand on a gate or a delegation, because a rule is a person's

782 keeps two approvals for a person: a request to delegate, and a gate
(`ai.discobox.sandbox`). A rule made by a person is that person's approval
given in advance, so a rule may name either. `orchestrate-issues` is not
auto-approved otherwise. The proposal card says what such a rule hands out,
in the gate card's words. A discobox still approves neither.

## Alternatives rejected

**Scope as a sentence the judge reads**, against a fact sheet the server
composes from a discobox's record: its repository and ref, who created it, the
prompt it was created with, its skills. It can say an organization, a kind of
task, or "started by a person", and it was what this ADR first accepted.
Turned down by the project's owner before anything shipped: which discoboxes
a rule answers should not depend on a model. A rule that mints grants nobody
reads is the wrong place for a reading that may differ between two asks, that
cannot be listed, and that puts a creator's prompt — a lead's own writing,
when a discobox created the box — in front of the model that decides.
Repositories, or all of them, is enough.

**A rule pinned to one discobox.** It was the narrowest scope the first
decision offered, compared by ID. A discobox is disposable and its asks end
with it, so a rule for one answers little a single approval does not, and it
had to be deleted with its discobox. Turned down with the sentence, for the
same reason: repositories, or all of them, is enough.

**More selectors than the repository** (creator, ref, an organization glob).
Each is a field, a glob or an operator a person has to learn, for a
difference the uses already hold: a rule's uses name the repository and the
branches it may touch, and every command is judged against the use minted.
The ref is no part of the match: a box on any branch of a repository is a box
of that repository.

**Match a discobox's additional sources too.** A box that pulled a repository
in for reference would be answered as one working on it.

**Let a rule read what a discobox says about itself** (tags, description,
the skill it says it is running). The server does not know which skill a box
is running. A box that can write its way into a rule has chosen its own rule.

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
- **Showing which existing discoboxes a rule is for** when it is made. It is
  a comparison now, so it can be listed; nothing does yet. Revisit if rules
  are made that answer more than their author meant.
- **Matching a renamed, mirrored or forked repository.** A repository is
  compared by the host and path it was given. Revisit if rules are seen
  missing boxes cloned from another spelling of one repository.
- **A server-wide off switch.** Disabling a rule is per rule. Revisit when an
  operator needs to forbid rules for projects they do not administer.
- **Limits on what one rule mints** (grants per discobox, per day). An open
  identical ask is reused, so a loop costs no judge calls. Revisit if rules
  are seen minting in bulk.
- **Rules for host trust and reactive requests.** Neither carries uses to
  read.

## Consequences

- **#162 is settled in part.** A standing answer for GitHub, made from the
  inbox, for the discoboxes of a repository, holding its "never `main`,
  never merge" limits, with what it does not cover going to a person. Not
  settled: the three items deferred above.
- New resource `ApprovalRule` with its store, service, and routes under the
  project; `SecretGrant` and `CredentialVerdict` gain the rule they were made
  under; the approve body gains a rule to create.
- `judge` gains the `rule` job kind and the proposal job;
  `PromptVersion` and Jev's `QuestionsVersion` change. A judge discobox whose
  sandbox agent predates a kind refuses its jobs until its image is
  replaced, so nothing is covered until then.
- A project with a rule keeps a judge discobox on a server that otherwise
  judges nothing.
- The approval dialog gains the auto-approve path; `discobox secret request
  approve` and `discobox secret rule` do the same acts from the CLI.
- Proposing costs one model call and up to three judge calls, paid when a
  person asks for it. Each request from a discobox a rule is for costs one
  content job per rule until one covers it. Which discoboxes a rule is for
  costs nothing: it is a comparison.
- **Which discoboxes a rule is for is exact, and can be said without asking
  anybody.** It is the same answer every time it is asked.
- **A rule cannot say an organization, a kind of work, or who started a
  box.** A project with many repositories names each, or makes a rule for
  every discobox. A discobox another discobox created on the same repository
  is answered like one a person started; what it is given is still held by
  the rule's uses and by the command judge.
- **A box whose source is spelled differently is not matched.** A fork, a
  mirror, or a local checkout with no remote is another repository or none,
  and its request waits for a person, which is what happens today.
- With Jev, a rule's uses and the uses asked for go to TypeSafe.
- **A wrong yes mints a grant with no person having read the ask.** What
  still holds: the credential, hosts, purpose and lifetime are facts the rule
  fixed; the grant is one discobox's; and every command is judged against
  the use it was minted for.
- **"My pull request" is not checked at approval.** A rule's uses can say
  "its own pull request", and nothing at the ask knows which that is. The
  command judge holds each command to the minted use.
- A rule with no repositories approves for discoboxes that do not exist yet,
  including ones a discobox creates. The proposal card names that in those
  words, and it is never what saying nothing makes.
- A database built before repositories replaced the scope sentence and the
  pin is upgraded in place: a rule that had either is deleted, and logged
  with what it said, since neither says a repository and it would otherwise
  read as a rule for every discobox, or one switch away from it. Its grants
  stand, as they do when a person deletes a rule.
