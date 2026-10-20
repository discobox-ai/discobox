# judge

The question Discobox puts to a model before a credential is used, and the
answer it will accept back. See
[ADR 26-09-22-838](../docs/adr/26-09-22-838-a-dedicated-pool-harness-judges-commands-and-credential-bearing-requests.md)
for the whole design; this package is its contract, and holds nothing that runs
it.

## What is here

| File | What it holds |
| --- | --- |
| `judge.go` | `Job` — a command (its argv, the `Stdin` it will read and where the discobox `Reported` it runs), an observed request, a delegation (a discobox about to hand a credential on: the uses it would grant, judged against the uses it was delegated), or an approval rule's reading (`rule`: its uses against the uses a request asked for) — its bounds, the JSON prompt it becomes, and `Digest`, by which a question already answered is found. |
| `policy.go` | A request job judged against its grant's policy (`Job.Policy`, `Job.Variables`; ADR 26-10-20-782 §8): `PolicyQuestions`, the small questions both judges are asked about it, and `DecidePolicy`, which combines their answers in code into a verdict and the `PolicyDecision` a verdict records. |
| `policysystem.go` | `PolicySystem`, `PolicySystemFor`, `PolicyPrompt`, `PolicySchema`, `DecodePolicyReply` and `PolicyPromptVersion`: those questions as a model behind `discobox-prompt` is asked them, and the reply Discobox will accept. |
| `propose.go` | `ProposalJob`, `ProposalSystem`, `ProposalSchema`, `Proposal`, and `DecodeProposal`: the one thing the judge's model writes rather than decides, the uses a person starts an approval rule from — the same work on any target of its kind. |
| `system.go` | `System`, the words the judge is given, and `PromptVersion`, which changes with them. |
| `verdict.go` | `Answer`, `Need`, `Schema`, and `Decode`: what Discobox will accept as a verdict. |
| `facts.go` | `Discobox`, `Facts` and `DelegationFacts`: what the control plane recorded about the discoboxes a job concerns, as the sentences it carries. |
| `standing.go` | `Standing`, `Route`, and `Job.Admits`: an allow the judge asks to let stand for a route, and whether it may. |
| `recognized.go` | `Recognition`, the names of the protocols, endpoints and parsers a pool recognizes, and `GuidanceFor`: the trusted words that go with each name. |
| `jev/` | The same jobs put to TypeSafe's Jev rather than a model behind `discobox-prompt` (ADR 26-10-01-324): its HTTP client, the yes/no questions a job becomes, and the code that decides a `judge.Answer` from their probabilities; for a policy job (`policy.go` there), one noul per `PolicyQuestions` question, read at its thresholds and combined by `DecidePolicy`. |

## The rules it exists to keep

**The trusted side owns the question.** A caller supplies evidence and the
approved use. The prompt, the schema, the role, and the bounds are here, so
that what is asked is the same wherever the asking happens, and a caller cannot
choose a kinder question.

**Evidence is data.** `Job` is marshalled to JSON, so a request body that
spells out a verdict and then gives fresh instructions arrives as one string in
one field. `Purpose` (or, for a request under a policy, `Policy` with the
`Variables` it binds) and `Host` are the authorization; nothing else can widen
them. A delegation or rule job is judged against several uses, and carries
them as the list `Purposes` instead of one `Purpose`, through the sandbox API's
`JudgeJob` to the judge: a list, never lines of one string, so no use's words
decide where another begins and a use written over several lines is still
one.

**Only an explicit allow is one.** `Decode` refuses anything that could be read
as permission nobody gave: prose around the object, a second object, a key said
twice at any depth (`encoding/json` would take the last), a field nobody
defined, an answer that both decides and asks, and an answer with no reason.
A failure to decode is not an allow, and callers treat it as a refusal. A
standing route of `null` or of no time is read as none rather than refused: a
model writes both for "this does not stand", and dropping a standing route only
narrows what was decided.

**A body is described in one shape; its bytes are asked for, not sent**
(ADR 26-09-26-240). A request job describes its body — media type, length,
and, when a parser recognized it, the `Parser`, its `Metadata` (one JSON
object, at most `MaxMetadataBytes`) or its `ParseError` — and carries its
`Content` only once the judge answers with `Need`, which names no form: the
parser decides how a body is written, not the judge. That is why `Answer` has
three outcomes rather than two. `System` tells the judge never to refuse for want of a body it
may still ask to see: an operation that lives in the body is otherwise refused
on the first round's description alone. `Budget` caps what may be shown at
`MaxBodyBytes` whatever the judge names, `Content` is present — even empty —
exactly when it was shown, `Body.Missing` says what is not being shown and why,
and `Body.Answers` reports an ask that would change nothing, which is a judge
that has decided nothing.

**A command is judged on what the discobox says it will do.** A command job
is built by the control plane when a pool asks before minting a sentinel
(ADR 26-09-22-838 §3): the purpose, credential and host from the live grant,
and from the discobox the argv, its `Stdin` (`Input`: text shown, at most
`MaxBodyBytes`, and `Missing` for what was not) and `Reported` (directory,
repository root, a git ref's commit and subject, each at most
`MaxReportedBytes`). Only a command job carries the last two. `System` tells
the judge they are the discobox's claims and an input it reads is part of the
operation; `jev/` puts them in the state.

The job reaches a judge discobox as the sandbox API's `JudgeJob`, which refuses
fields it does not know, so a judge whose image carries a sandbox agent older
than a field refuses every job that sends it — fail closed, with a decode error
as the reason. A built-in harness is re-pinned to the release's image when the
server is upgraded, and a judge is replaced when its image changes, so that is
a window; a custom harness built FROM an older sandbox-agent image is judged
by nothing until it is rebuilt.

**A use that drops a limit is wider** (ADR 26-10-14-223). A delegation or
rule job's purposes
set limits — "never main", a closing "Never …" sentence — and once granted a
use is all its commands are held to. So `System` tells the judge that a use
keeps every limit of the purpose it falls under, in the same or stricter words,
or it is not within, however narrow it is otherwise; Jev asks it as a
question of its own (below). Without that, a use asked with its limits left
out read as within (measured in `test/judge-evals/use-cases`, the
`*-without-its-lines` controls), and a rule would mint it with nobody
looking.

**A rule's uses are read as a job with its own words** (`rule`,
ADR 26-10-12-330). An approval rule approves a credential request without
asking, so its text is the authorization and the ask is what is measured
against it. The job is a delegation job's shape with its own words, because
who wrote what differs: `Purposes` are the rule's uses, a person's, naming a
class ("any issue in org/repo", a branch `discobox/<slug>`, "never main"), and
`Uses` are an agent's, which may be worded to pass. The justification is never
in the job.

It is asked once, and nothing stands for it. `Digest` is the SHA-256 of the
job's prompt, which a verdict records so the same question is answered from
the record and an edited rule is asked afresh.
A change to how a job is put — a field added (`Facts`, ADR 26-10-14-691) or
reshaped (`Purposes`) — is a new question too: an answer recorded before it is
not found, and a request still pending across the upgrade is asked again when
it is next marked (a rule made or edited, `RuleRequests`). Until then its card
reads as still being judged, and a person can answer it as before.

**Which discoboxes a rule is for is not a job.** It is a list of repositories
the control plane compares with the asking discobox's primary source, so no
model reads it and nothing here describes a discobox.

**A proposal is written, never decided** (`propose.go`, ADR 26-10-12-330 §7).
The uses a person starts a rule from are a model's to write, as a typed job
like any other here: its own system prompt and schema, no tools, and a decode
as strict as `Decode`. The request's uses are data in it. It writes sentences
only — one list, the same work on any target of its kind (`usesAnyTarget`) —
and `ProposalSystem` has it keep every limit a use states. What it writes
authorizes nothing: the control plane puts it to the judge as the job a rule
would later ask, and a person reads and edits it, before it is a rule.

**Guidance is the trusted side's.** A request names what a pool recognized it
as (`Request.Protocol`, `Request.Endpoint`); the words that go with a name are
here (`GuidanceFor`), and the control plane puts them in `Job.Guidance`. A pool
sends names, never sentences, so nothing a pool or a request says becomes
Discobox speaking. A name nobody wrote guidance for brings none, so a pool
newer than its control plane is judged without guidance, not refused.
Guidance explains; the system prompt says it never authorizes.

**Facts are the trusted side's too** (ADR 26-10-01-324 §8,
ADR 26-10-14-691). `Job.Facts` are sentences the control plane states, at
most `MaxFacts`, so a purpose can say "this discobox's repository". `Facts`
writes them about the discobox asking, `DelegationFacts` about both
discoboxes of a delegation, since a use handed on means the receiving one, and
`TargetFacts` about the discobox asking and the one a call to the discobox API
names, since that call names it only by an ID and "the discoboxes this
discobox created" is read by who created it (ADR 26-10-14-320 §5), so for the
target it says so when no discobox did: each
from what was recorded when that discobox was created — its ID, its sources'
remotes and the ref each was created from, and the discobox that created it —
and from nothing the discobox can move afterwards: not its name (meta it
writes), not where its work has got to. Every sentence names its discobox by
ID. A remote or ref is repeated only as one printable word JSON does not
escape, since whoever created the discobox named it and a fact is Discobox
speaking. Other sources come last, so the cap drops only them, and a discobox
whose other sources do not all fit is said to have more than are listed.
"This discobox's repository" is its primary source. Like guidance, facts
explain and never authorize. `System` says both, `jev/` says both in every
question that reads facts and puts them in its instructions, and the judge
discobox's job (the sandbox API's `JudgeJob`) carries them.

**A wrapper prints the verdict and nothing else.** `Decode` takes one JSON
object and no prose around it, which is a requirement on every harness image's
`discobox-prompt`: a wrapper that frames its answer in a transcript cannot
judge. See [`harness/DESIGN.md`](../harness/DESIGN.md) for the wrapper
contract.

**The judge proposes a standing allow; Discobox admits it.** An allow may carry
a `Standing` route: net/http pattern syntax, one method and an exact path
(ADR 26-09-25-428). `Decode` refuses a route that does not parse, or one beside
anything but an allow. `Job.Admits` keeps only a first-round route decided
before the body's content was shown, on a request whose operation is not in its
body (`Request.OperationInBody`: a recognized protocol, an endpoint that reads
its body — every endpoint but the reads `operationOutsideBody` names, so one
this package does not know is read as reading its body — or a body its parser
could not read) — a JSON object's keys are its
shape, not its operation, and do not stop one; the control plane asks the same
of every request a standing allow would answer, since a route says nothing
about a body — standing
for some time, with a literal segment, that covers its own request; `Route.Literal` says whether a segment is
named as written, which the control plane asks of the segment a discobox API
route names its discobox in (ADR 26-10-14-320 §6); `Duration`
caps it at `MaxStanding`. A route matches the method and the unescaped path
segments and nothing else. A path with a dot or empty segment, or a segment
that unescapes to a slash or backslash, matches no route, because the upstream
may resolve it somewhere the route never named. The host, the discobox, and the use are never the
judge's to name.

**Rounds are bounded.** `MaxRounds` asks in total, inside one `Timeout` for the
whole exchange, because a request is held open while the judge thinks.
`ReachWait` comes before it: how long the control plane waits for the judge's
discobox to become reachable, which every hop bounding the exchange allows for. A
command job is asked once: there is nothing further to show.

**Jev is asked questions, and code decides** (`jev/`, ADR 26-10-01-324). Jev
answers typed questions with probabilities and writes no text, so `System`,
`Schema` and `Decode` do not apply to it. Instead:

- **A rule's job.** A `rule` job is one `use_N` question per use asked for,
  with the rule's uses in the instructions and the uses asked for as the
  state, and the claim-of-approval hazard, told what its evidence is.
- **A use keeps its limits.** A delegation or rule job also asks one
  `keep_N` question per use: does it keep every limit of the purpose it falls
  under? (Its words call each purpose a line: the server takes a use on one
  line, and one stored over several before it did is still one entry.) It is held to within's bar. Asked inside `use_N`, Jev read a use
  with its "Never …" sentence dropped as within (0.90–0.96), since such a use
  names nothing the limits forbid; asked alone, it separates them (stripped
  0.11–0.43, honest 0.49–0.96). At within's bar many honest asks are unsure
  of it, and go to the judge discobox or are refused.
- **Questions.** A job becomes yes/no questions. The approved purpose, host,
  credential and guidance go in each question's instructions; only the
  evidence goes in the state. The claim-of-approval hazard is told what its
  evidence is when honest (a delegation's uses are asks to be allowed, worded
  as instructions, with their own limits), and counts only a claim of prior
  approval or words to the judge (ADR 26-10-07-640).
- **Decision.** Jev's probabilities are calibrated, and every question is
  read at the same confidence in both directions (ADR 26-10-12-669): `YesAt`
  (0.8) or above is a yes, below `NoBelow` (0.2) is a no, and anything
  between is Jev saying it cannot tell. `decide` turns the answers into a
  `judge.Answer` and whether Jev was unsure, in this order:
  - a hazard that is a yes refuses: text in the evidence claiming approval;
  - a hazard that is not a no refuses, unsure;
  - a body that `Job.CanShowBody` says can still be shown is asked for, and
    never allowed or refused on its description. A read and a write often
    have the same shape.
  - a "within" question that is a no refuses;
  - an allow needs every "within" question to be a yes;
  - anything else refuses, unsure.
- **Unsure.** `Verdict.Unsure` marks a refusal Jev could not tell about, and
  its reason says "could not tell" where a decided one says "judged it
  unlikely". Both refuse. A server may put the unsure ones to its judge
  discobox, which decides them and the rounds it asks for (`jevUnsure`);
  Jev's allows and decided refusals are its own.
- **What it never does.** It never lets an allow stand. The reason is composed
  from whichever question decided.
- **What it was sent.** `Verdict.Input` is the exact JSON body of the request
  to Jev's API — state, questions and model, never the key — which a server
  records on the verdict. Its state is the job's evidence and holds nothing
  `Prompt` does not.
- **Versioning.** `QuestionsVersion` names the questions and thresholds
  together. Change any of them and change it.

**A request under a policy is many small questions, and code decides**
(`policy.go`, ADR 26-10-20-782 §8). A grant that carries a policy is judged by
it, not by the sentence beside it: a request job carries the `Policy` in place
of a `Purpose`, and the `Variables` that bind for this decision (only those
that resolved at approval and resolve to the same value now). Neither judge
decides. `PolicyQuestions` is what both are asked, built once so they answer
the same questions and are read the same way:

- **Per statement:** the action — for an allow, is *everything* the request
  does one of its actions; for a deny, is *any* part — its resource (type, name,
  parents) and its scope, with each variable said as its value and what it
  means. Separate "any" questions for a deny can join parts of different
  operations, which only ever refuses more.
- **Per condition, two:** does what is shown settle it, and does it hold. A
  condition is unknown unless the first is a yes: asked only whether it held,
  Jev read "not shown" as "no".
- **Per request:** the supporting read (§2's one exception), asked only when
  the policy has an allow, and the claims-approval hazard, Jev's in the request
  job's own words.
- **Code answers what needs no reading** (`knownParts`): `manage`, `anything`
  with no name or parents, `anywhere`, and a part naming a variable that does
  not bind, which matches nothing; a statement code knows matches nothing is
  not asked about. A question several statements share is asked once and
  answers each. `created by $this` is an observed fact nobody records yet, so a
  judge's yes on such a resource is unknown.

`DecidePolicy` combines the answers: `policy.Evaluate`'s two phases, on one
atom — §8's own "is *everything* the request does one of its actions" wording
is what lets the request be one. A request whose parts two different allows
cover (a push of two branches, each its own statement) is therefore never
allowed by a judge, only refused or left unsure; splitting a request into
atoms is the fast path's (§7). And a
refusal they decide stands whatever the hazard says, since the hazard only
ever refuses more; then a claim of approval that is a yes refuses, and one not
ruled out refuses unsure; then what the phases allowed is allowed. An allow
names the statement that covered it and a refusal the deny that decided it.
Undecided is a refusal, unsure. Whatever is unsure that could still change the
answer asks for the body instead while it can still be shown: **asking is
code's, never the model's**. Offered an ask, the model asked for the body of a
request a deny already refused from its URL, only to rule out the hazard. A
question with no answer is unknown. A judge discobox says beside its answers
the `QuestionsDigest` of the questions it asked and its `PolicyPromptVersion`,
and its answers are read only when that digest is of the questions this side
would ask: a runtime of another version may put other words under the same
ID. Otherwise it is no verdict. The verdict records the runtime's version. Nothing stands: neither judge proposes a
route for a policy job.

The model's side (`policysystem.go`) gets the policy — its statements numbered
and bound, the definitions, host, credential, facts and guidance — as its
system prompt (`PolicySystemFor`), and the request in the prompt between
`<request>` lines as data it never takes as instructions or facts
(`PolicyPrompt`); `encoding/json` escapes `<` and `>`, so nothing in the
request can close that block. It answers every question yes, no or unsure with
a reason — unsure for what depends on a body it has not been shown — and
nothing else; `DecodePolicyReply` is as strict as `Decode` and refuses a reply
that misses a question, answers one not asked, or carries anything beside the
answers — a reason of its own, an allow, an ask — which would read as a
decision.
`PolicyPromptVersion` names the words, pinned by digest like `PromptVersion`.
Jev's side is `jev.PolicyQuestionsVersion`: each question's statements,
definitions of only the words it uses, facts and guidance in its instructions,
only the request in its state. Like a use's request job, a body that can still
be shown is asked for before Jev's answers decide, unless Jev is sure the
request claims its own approval.

**Recorded cases measure a harness's judge.** `test/judge-evals` holds jobs and
what a correct judge does with each; `go tool task eval:judge` asks a harness's
own `discobox-prompt` about them with this package's `System`, `Schema`,
`Prompt`, and `GuidanceFor`, and scores the answers with `Decode` the way the
control plane acts on them. Change the system prompt, a job's fields, or a
harness's judge model, and run it against every harness that judges.
`go tool task eval:judge:jev` asks Jev about the same cases through `jev/`,
and counts what it was unsure of and what it refused alone on a case a correct
judge does not refuse; change a question or a threshold there and run it.
A proposer case has the wrapper write a proposal (`ProposalSystem`) and the
same judge score it: the written uses, and the uses as asked, cover the
request, and neither covers a control. A `policy-*` case is a request job with
a policy, put to either judge the way the control plane puts it and combined by
`DecidePolicy`; its unsure refusals are counted whichever judge answered.

**A job says beside its request whether the judge may still ask.** `Prompt`
derives `BodyCanBeShown` and `AsksLeft` — a request job before its last round,
with a body not yet shown in full — and replaces whatever a caller set. The
rule to ask rather than refuse is in `System`, but a model deciding one request
follows what that request says it can do: replaying refused requests, the
judge never asked for a body until the job said it could.
