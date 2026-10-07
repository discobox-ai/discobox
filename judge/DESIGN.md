# judge

The question Discobox puts to a model before a credential is used, and the
answer it will accept back. See
[ADR 26-09-22-838](../docs/adr/26-09-22-838-a-dedicated-pool-harness-judges-commands-and-credential-bearing-requests.md)
for the whole design; this package is its contract, and holds nothing that runs
it.

## What is here

| File | What it holds |
| --- | --- |
| `judge.go` | `Job` — a command (its argv, the `Stdin` it will read and where the discobox `Reported` it runs), an observed request, or a delegation (a discobox about to hand a credential on: the uses it would grant, judged against the uses it was delegated) — its bounds, and the JSON prompt it becomes. |
| `system.go` | `System`, the words the judge is given, and `PromptVersion`, which changes with them. |
| `verdict.go` | `Answer`, `Need`, `Schema`, and `Decode`: what Discobox will accept as a verdict. |
| `standing.go` | `Standing`, `Route`, and `Job.Admits`: an allow the judge asks to let stand for a route, and whether it may. |
| `recognized.go` | `Recognition`, the names of the protocols, endpoints and parsers a pool recognizes, and `GuidanceFor`: the trusted words that go with each name. |
| `jev/` | The same jobs put to TypeSafe's Jev rather than a model behind `discobox-prompt` (ADR 26-10-01-324): its HTTP client, the yes/no questions a job becomes, and the code that decides a `judge.Answer` from their probabilities. |

## The rules it exists to keep

**The trusted side owns the question.** A caller supplies evidence and the
approved use. The prompt, the schema, the role, and the bounds are here, so
that what is asked is the same wherever the asking happens, and a caller cannot
choose a kinder question.

**Evidence is data.** `Job` is marshalled to JSON, so a request body that
spells out a verdict and then gives fresh instructions arrives as one string in
one field. `Purpose` and `Host` are the authorization; nothing else can widen
them.

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

**Guidance is the trusted side's.** A request names what a pool recognized it
as (`Request.Protocol`, `Request.Endpoint`); the words that go with a name are
here (`GuidanceFor`), and the control plane puts them in `Job.Guidance`. A pool
sends names, never sentences, so nothing a pool or a request says becomes
Discobox speaking. A name nobody wrote guidance for brings none, so a pool
newer than its control plane is judged without guidance, not refused.
Guidance explains; the system prompt says it never authorizes.

**Facts are the trusted side's too** (ADR 26-10-01-324 §8). `Job.Facts` are
sentences the control plane states about a job's targets and the discobox
asking, at most `MaxFacts`. Like guidance, they explain and never authorize.
`jev/` puts them in its questions' instructions. The judge discobox's job (the
sandbox API's `JudgeJob`) does not carry them, and the server states none yet.

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
for some time, with a literal segment, that covers its own request; `Duration`
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

- **Questions.** A job becomes yes/no questions. The approved purpose, host,
  credential and guidance go in each question's instructions; only the
  evidence goes in the state. The claim-of-approval hazard is told what its
  evidence is when honest (a delegation's uses are asks to be allowed, worded
  as instructions, with their own limits), and counts only a claim of prior
  approval or words to the judge (ADR 26-10-07-640).
- **Decision.** `decide` turns the answers into a `judge.Answer` and fails
  closed:
  - a hazard at `HazardAt` refuses: text in the evidence claiming approval;
  - a body that `Job.CanShowBody` says can still be shown is asked for, and
    never allowed on its description. A read and a write often have the same
    shape.
  - an allow needs every "within" question at `AllowAt` (0.8). Jev's
    probabilities are calibrated, and a value near 0.5 means it cannot tell.
  - anything else refuses. Its reason says Jev could not tell when the
    weakest "within" is at `UnsureAt` or above, and that it is unlikely
    below; the two refuse alike. A server may put every refusal, hazards
    included, to its judge discobox, which decides it and the rounds it
    asks for (`jevUnsure`, ADR 26-10-07-937), so Jev alone decides only its
    allows.
- **What it never does.** It never lets an allow stand. The reason is composed
  from whichever question decided.
- **What it was sent.** `Verdict.Input` is the exact JSON body of the request
  to Jev's API — state, questions and model, never the key — which a server
  records on the verdict. Its state is the job's evidence and holds nothing
  `Prompt` does not.
- **Versioning.** `QuestionsVersion` names the questions and thresholds
  together. Change any of them and change it.

**Recorded cases measure a harness's judge.** `test/judge-evals` holds jobs and
what a correct judge does with each; `go tool task eval:judge` asks a harness's
own `discobox-prompt` about them with this package's `System`, `Schema`,
`Prompt`, and `GuidanceFor`, and scores the answers with `Decode` the way the
control plane acts on them. Change the system prompt, a job's fields, or a
harness's judge model, and run it against every harness that judges.
`go tool task eval:judge:jev` asks Jev about the same cases through `jev/`;
change a question or a threshold there and run it.

**A job says beside its request whether the judge may still ask.** `Prompt`
derives `BodyCanBeShown` and `AsksLeft` — a request job before its last round,
with a body not yet shown in full — and replaces whatever a caller set. The
rule to ask rather than refuse is in `System`, but a model deciding one request
follows what that request says it can do: replaying refused requests, the
judge never asked for a body until the job said it could.
