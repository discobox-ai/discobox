# judge

The question Discobox puts to a model before a credential is used, and the
answer it will accept back. See
[ADR 26-09-22-838](../docs/adr/26-09-22-838-a-dedicated-pool-harness-judges-commands-and-credential-bearing-requests.md)
for the whole design; this package is its contract, and holds nothing that runs
it.

## What is here

| File | What it holds |
| --- | --- |
| `judge.go` | `Job` — a command or an observed request, judged against one approved use — its bounds, and the JSON prompt it becomes. |
| `system.go` | `System`, the words the judge is given, and `PromptVersion`, which changes with them. |
| `verdict.go` | `Answer`, `Need`, `Schema`, and `Decode`: what Discobox will accept as a verdict. |
| `standing.go` | `Standing`, `Route`, and `Job.Admits`: an allow the judge asks to let stand for a route, and whether it may. |

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
A failure to decode is not an allow, and callers treat it as a refusal.

**The body is asked for, not sent.** A request job describes its body — media
type and length — and carries it only once the judge answers with `Need`. That
is why `Answer` has three outcomes rather than two. `Budget` caps what may be
shown at `MaxBodyBytes` whatever the judge names, `Body.Missing` says what is
not being shown and why, and `Body.Answers` reports an ask that would change
nothing, which is a judge that has decided nothing.

**A wrapper prints the verdict and nothing else.** `Decode` takes one JSON
object and no prose around it, which is a requirement on every harness image's
`discobox-prompt`: a wrapper that frames its answer in a transcript cannot
judge. See [`harness/DESIGN.md`](../harness/DESIGN.md) for the wrapper
contract.

**The judge proposes a standing allow; Discobox admits it.** An allow may carry
a `Standing` route: net/http pattern syntax, one method and an exact path
(ADR 26-09-25-428). `Decode` refuses a route that does not parse, or one beside
anything but an allow. `Job.Admits` keeps only a first-round route, standing
for some time, with a literal segment, that covers its own request; `Duration`
caps it at `MaxStanding`. A route matches the method and the unescaped path
segments and nothing else. A path with a dot or empty segment, or a segment
that unescapes to a slash or backslash, matches no route, because the upstream
may resolve it somewhere the route never named. The host, the discobox, and the use are never the
judge's to name.

**Rounds are bounded.** `MaxRounds` asks in total, inside one `Timeout` for the
whole exchange, because a request is held open while the judge thinks. A
command job is asked once: there is nothing further to show.
