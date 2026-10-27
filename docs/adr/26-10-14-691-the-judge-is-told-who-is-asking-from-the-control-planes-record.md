# 26-10-14-691 — The judge is told who is asking, from the control plane's record

- **Status**: Superseded by [26-10-27-880](26-10-27-880-access-to-a-secret-is-a-binding-held-to-static-routes-and-no-judge-decides-it.md)
- **Date**: 2026-10-14
- **Supersedes in part**: [26-10-12-330](26-10-12-330-a-credential-request-may-be-answered-by-a-rule-the-judge-reads.md)'s
  rejection of a fact sheet the judge reads, for a rule's uses only (§6):
  which discoboxes a rule answers is still the server's check.
- **Relates to**: [ADR 26-10-01-324](26-10-01-324-a-server-may-judge-with-jev-instead-of-a-judge-discobox.md)
  §8, which added `judge.Job.Facts` and left each fact to be decided with its
  source; [ADR 26-09-22-838](26-09-22-838-a-dedicated-pool-harness-judges-commands-and-credential-bearing-requests.md),
  whose judge discobox these facts must reach; and
  [ADR 26-10-12-026](26-10-12-026-a-sandboxs-name-is-meta-and-renaming-it-is-a-meta-write.md),
  which makes a sandbox's name meta the sandbox writes.

## Context

A person approves a use in their own words, and the natural words refer to the
discobox: "create issues for this discobox's repository", "push to the branch
it was created from". The judge cannot resolve either. A job carries the
purpose, the host, the credential, and evidence the discobox wrote. Nothing in
it says which repository the discobox holds, so the judge refuses the request
or, worse, allows an issue in any repository.

`Job.Facts` is the slot for this, but the server states no facts yet. Facts
also cannot reach a judge discobox, because the sandbox API's `JudgeJob` has no
field for them. Only Jev is given them.

## Decision

### 1. A fact is something the discobox cannot have written

A fact may be stated only from what the control plane recorded and the
discobox under judgement cannot change afterwards. That excludes:

- everything a pool or a discobox sends with a job: the request, the argv,
  `stdin`, `reported`;
- everything copied back from the sandbox's meta: its name
  (ADR 26-10-12-026), description, and tags;
- everything recorded about the sandbox's work since it was created: the git
  status its agent reports, the commits `discobox apply` landed
  (`appliedCommits`), and whether a pushed source was delivered.

A discobox is therefore named only by its ID. The ID is randomly generated and
never shown to a person, and it is the only identifier the discobox cannot
choose.

### 2. Facts are stated for each job, at the time it is judged

What builds the job — the server's request, command, delegation and rule
judges — reads the asking sandbox's row and states the facts from it. For a
rule job (§6) the discobox asking is the one that filed the request. Facts are
not stored with the use, and not cached between jobs: a use can be held by
more than one discobox, and "this discobox" means the one asking now. The only
thing a pool supplies is the sandbox ID, which the server already checks
against the authenticated pool when it reloads the use.

### 3. Every job carries the same core facts about the discobox asking

There is no opt-in. A fact explains what the purpose's words refer to and never
widens it (ADR 26-10-01-324 §8), so stating one the judge does not need costs a
few tokens, and leaving out one it needs costs a refusal nobody can diagnose.
Within `judge.MaxFacts`, every job states:

- the asking discobox's ID;
- its primary source as it was created: its repository (`url` when it has
  one, else `upstreamUrl`, the remote a local checkout's client reported; §6)
  normalized to host and path, and the ref it was created from; or that it
  has no remote;
- the ID of the discobox that created it, when one did (`createdBySandboxId`,
  fixed at create);
- each additional source, stated as the primary source is, one fact each,
  while room remains: they come last so the limit drops only them, and a
  discobox whose other sources do not all fit is said to have that many more
  than are listed, so none is left out unremarked.

"This discobox's repository" is its primary source. The judge is told so: a
discobox's other sources are context, not what the words refer to.

Sources are the create-time record and nothing after it. A source's remote and
its checkout are fixed when the sandbox is created; the branch it has since
moved to, the commits it has made, and anything else the sandbox reports do not
change what its sources are.

A discobox a person created has no creator fact. Discobox has no strong notion
of a user's identity to state, so the absence of a creating discobox is all the
judge is told.

A discobox another discobox created is stated the same way: its sources are the
ones recorded at its create, which its parent named. They are not traced back
to an ancestor, and not set apart as a claim: what a discobox was created with
is what the control plane recorded when it was created, whoever asked.

Each is a sentence in the control plane's words, written by the `judge`
package from the fields it needs, never a slice of the sandbox's JSON, and each
names the discobox it is about by its ID. A remote or ref name is repeated only
as one printable word JSON does not escape: whoever created the discobox named
it, and a fact is Discobox speaking. A sandbox field added later is not a fact
until it is decided as one.

### 4. A delegation states the discobox receiving the credential too

A use handed on keeps its words and changes its subject: "this discobox's
repository", handed from a lead to a child, means the child's. So a delegation
job states the core facts of both discoboxes — the one handing the credential
on, which is the one asking, and the one it would hand it to — each labeled,
both before the other sources of either. The judge is told that such a use is
within only when the receiving discobox's target is the same as, or narrower
than, the giving one's. On a create the receiving discobox is not stored yet,
so the delegation is judged from the row about to be created.

### 5. Facts reach every judge

- The sandbox API's `JudgeJob` gains `facts`, and the sandbox agent passes
  them into the `judge.Job` it puts to its wrapper.
- `System` tells the judge what facts are, beside guidance: the trusted side's
  statements, each naming the discobox it is about, which it may rely on to
  resolve what the purpose refers to, and which never authorize. It says
  facts may also be about a job's targets, which ADR 26-10-01-324 §8 left
  open. `PromptVersion` moves.
- `test/judge-evals` gains a case allowing "create issues in this discobox's
  repository" for the repository its facts name, a control refusing the same
  request against another repository, and the same use handed on to a child
  created from the lead's repository (allowed) and from another (refused).

### 6. A rule's uses are read with the facts too

A person can write a rule whose uses refer to the discobox asking: "create
issues in this discobox's repository". The rule job carries the requesting
discobox's facts like every other job, so the judge can tell which repository
that is.

ADR 26-10-12-330 rejected a fact sheet the judge reads, because which
discoboxes a rule answers should not depend on a model. That still holds, and
nothing here changes it. A rule's repositories are compared by the server
first, and a request from a discobox outside them is never put to the judge.
The facts only tell the judge what the uses' words refer to for a discobox
the server has already let through. They carry no prompt, skills, or anything
else a creator wrote, which 330 also objected to: a source's repository and
ref, and IDs.

A discobox's repository has one definition (`GitSource.RepositoryURL`): its
URL when it has one, else the remote its client reported for a local checkout.
The server compares a rule's repositories with it, and the facts state it, so
the two cannot name different repositories for one discobox.

The facts are part of the rule's question. The answer on record is found by the
job's digest, so every place that composes the question — approving,
reconciling, the card, a proposal — reads the same row, and an answer recorded
before facts existed is asked again once.

## Alternatives considered

- **A use opts into the facts it needs.** Rejected. The person writing the use
  would have to know what the judge needs before they could ask for it, and a
  missing fact shows up only as a refusal. Hiding the discobox's own sources
  from the judge protects nothing: the discobox already knows them.
- **The sandbox's definition, or portions of it, as JSON.** Rejected. It mixes
  fields the control plane recorded with fields the sandbox reports, so a
  discobox could write its own facts. A local source's root is a path on the
  client that the judge cannot match against a URL. A definition's shape would
  also become the judge's input, so any new field would reach the judge
  without anyone deciding it should.
- **Naming the discobox by its name.** Rejected: the name is meta the
  discobox writes (ADR 26-10-12-026).
- **The person who created the discobox.** Deferred. A person is not yet an
  identity Discobox can state with confidence. Revisit it when users have one.
- **The sandbox's current branch and commits.** Rejected. They are what the
  sandbox has done, reported by the sandbox, and a fact it can move is not a
  fact.
- **A child's sources traced to its nearest ancestor a person created, or
  stated as its creator's claim.** Rejected. A child is created with a source,
  and that source is the record of what it is; tracing it elsewhere makes "this
  discobox's repository" mean another discobox's, and a claim the judge is told
  to discount is a fact that says nothing.
- **Telling the judge only that a handed-on use referring to "this discobox"
  changes meaning.** Rejected for stating the receiving discobox (§4): the
  judge would have to recognize every way of saying it, and a child created
  from the same repository would be refused for nothing.
- **Facts about the request's target** (that the discobox it reads was created
  by the one asking, ADR 26-10-01-324 §8's example). Not decided here. They
  depend on the request, not just on who is asking, and each is decided with
  its source.

## Consequences

- A use can say "this discobox's repository" and be judged against the
  repository the control plane recorded.
- A fact is only as trustworthy as whoever created the discobox. A discobox
  that creates another chooses its source (ADR 26-09-24-630), so its child's
  facts name what the parent chose. A use the parent hands on is judged
  knowing that (§4). A use a person scoped wider than one discobox — to a
  project or a harness configuration — reaches the child too, and there "this
  discobox's repository" is the one the parent created it with. That is the
  meaning such a use has: it was written for every discobox in its scope.
- A judge whose image has a sandbox agent older than `JudgeJob.facts` refuses
  every job until it is replaced, the window `judge/DESIGN.md` already
  describes for a new field.
- Every job costs one more sandbox read on the server.
